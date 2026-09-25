package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/secret"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/types"
)

// Worker is the single-query binding of the Researcher seam: it adapts one
// user query to one bounded RunWorker run and maps the resulting Finding onto
// a types.Interaction the existing formatter and sinks render.
//
// Start allocates an opaque local id and launches the loop in a goroutine,
// returning the id immediately so the run core can emit it before awaiting.
// Await blocks on that run; if Await's context ends before the loop has
// produced its Finding the run is cancelled, because an in-process run nobody
// is waiting for can only waste paid turns. Result maps the Finding. When
// deps.Remember is set, a Completed finding is saved to the knowledge store
// after the Finding is recorded and, normally, before Await returns (bounded
// by rememberTimeout); a save still running when Await's context ends never
// costs the recorded Finding. Unlike the Gemini adapter, the id is a local
// handle, not a durable resume token: a crashed in-process run cannot be
// recovered by `chiron get <id>`.
//
// The loop can finish a turn before the run core has emitted the id Start
// returned, so deps.Progress deliveries for a run are held until Await is
// first entered for it; a delivery still held at its progress deadline is
// dropped, never reordered.
type Worker struct {
	deps WorkerDeps

	mu   sync.Mutex
	runs map[string]*workerState
}

// workerState is the per-run record keyed by interaction id. loopDone is
// closed once finding and completed are stored, before any save-back; done is
// closed once the run, save-back included, has finished. finding and
// completed are written only before loopDone closes. cancel stops the loop
// early. awaiting is the progress gate, closed once by the first Await.
type workerState struct {
	query   string
	started time.Time
	cancel  context.CancelFunc

	awaiting     chan struct{}
	awaitingOnce sync.Once

	loopDone  chan struct{}
	done      chan struct{}
	finding   Finding
	completed time.Time
}

var _ researcher.Researcher = (*Worker)(nil)

// InteractionIDPrefix marks an id minted by the in-process worker, distinct
// from the server-issued ids of the managed adapter. The CLI uses it to refuse
// `chiron get`/`follow-up` on an id that has no server-side state.
const InteractionIDPrefix = "wkr_"

// IsWorkerInteractionID reports whether id was minted by the in-process
// worker.
func IsWorkerInteractionID(id string) bool {
	return strings.HasPrefix(id, InteractionIDPrefix)
}

// NewWorker builds the worker researcher over shared deps. It validates the
// collaborators a run cannot proceed without at construction, so a
// misconfigured worker fails at the composition root rather than mid-run after
// a resume handle has been emitted.
func NewWorker(deps WorkerDeps) (*Worker, error) {
	if err := checkWorkerDeps(deps); err != nil {
		return nil, err
	}
	return &Worker{
		deps: deps,
		runs: make(map[string]*workerState),
	}, nil
}

// checkWorkerDeps validates the collaborators and cap every worker loop
// needs; the Worker and the fleet's worker pool both apply it at
// construction.
func checkWorkerDeps(deps WorkerDeps) error {
	switch {
	case deps.Model == nil:
		return errWorkerNoModel
	case deps.Search == nil:
		return errors.New("fleet: worker requires a search client")
	case deps.Fetch == nil:
		return errors.New("fleet: worker requires a fetch client")
	case deps.Caps.MaxTurns <= 0:
		return fmt.Errorf("fleet: worker requires a positive turn cap, got %d", deps.Caps.MaxTurns)
	}
	return nil
}

// Start implements researcher.Researcher. It builds a Brief from the query,
// with deps.ReportTemplate rendered as its OutputFormat when set, allocates an
// opaque local id, launches RunWorker in a goroutine, and returns the id
// immediately. A follow-up (PreviousInteractionID set) is rejected before any
// work starts, because the worker has no stored interaction chain; so is a
// template that fails to render.
//
// The run is detached from the Start context's cancellation (the run core
// scopes that context to the start phase) and given its own cancel, which
// Await triggers when its caller stops waiting.
func (w *Worker) Start(ctx context.Context, task researcher.Task) (string, error) {
	if task.Query == "" {
		return "", errors.New("fleet: worker query must not be empty")
	}
	if task.PreviousInteractionID != "" {
		return "", errors.New("fleet: the in-process worker does not support follow-up interactions")
	}
	brief := Brief{Objective: task.Query}
	if w.deps.ReportTemplate != nil {
		format, err := w.deps.ReportTemplate.Render(task.Query)
		if err != nil {
			return "", err
		}
		brief.OutputFormat = format
	}

	id, err := newInteractionID(InteractionIDPrefix)
	if err != nil {
		return "", err
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	st := &workerState{
		query:    task.Query,
		started:  time.Now(),
		cancel:   cancel,
		awaiting: make(chan struct{}),
		loopDone: make(chan struct{}),
		done:     make(chan struct{}),
	}
	w.mu.Lock()
	w.runs[id] = st
	w.mu.Unlock()

	after := func(ctx context.Context, f Finding, span trace.Span) {
		st.finding = f
		st.completed = time.Now()
		close(st.loopDone)
		w.rememberFinding(ctx, id, brief.Objective, f, span)
	}
	go func() {
		defer cancel()
		runWorker(runCtx, w.deps, brief, st.awaiting, after)
		close(st.done)
	}()

	return id, nil
}

// Await implements researcher.Researcher: block until the run for id
// finishes, save-back included. If ctx ends while the loop is still running,
// the run is cancelled so no further paid turns are taken, and the context
// error is returned. If ctx ends after the loop has recorded its Finding,
// Await returns nil: the finding is complete and paid for, and the save-back
// continues under its own rememberTimeout. Entering Await releases the run's
// held progress deliveries. An unknown id is a programming fault, not a user
// condition.
func (w *Worker) Await(ctx context.Context, id string) error {
	st, err := w.state(id)
	if err != nil {
		return err
	}
	st.awaitingOnce.Do(func() { close(st.awaiting) })
	select {
	case <-st.done:
		return nil
	case <-ctx.Done():
	}
	select {
	case <-st.loopDone:
		return nil
	default:
		st.cancel()
		return fmt.Errorf("fleet: awaiting worker %s: %w", id, ctx.Err())
	}
}

// Result implements researcher.Researcher: map the run's Finding onto a
// types.Interaction once the loop has recorded it, whether or not a
// save-back is still running. Called before then, it reports the in-progress
// state rather than blocking.
func (w *Worker) Result(_ context.Context, id string) (*types.Interaction, error) {
	st, err := w.state(id)
	if err != nil {
		return nil, err
	}
	select {
	case <-st.loopDone:
		return w.mapInteraction(id, st), nil
	default:
		return &types.Interaction{
			ID:        id,
			Agent:     agentWorker,
			Query:     st.query,
			Tools:     workerTools(w.deps),
			Status:    types.StatusInProgress,
			CreatedAt: st.started,
		}, nil
	}
}

// mapInteraction maps a finished worker run onto the domain Interaction the
// formatter renders: the answer becomes the single text Output, the Finding's
// citations become the Citation list, usage carries the accumulated counters,
// and StatusDetail carries the Finding's diagnostic.
func (w *Worker) mapInteraction(id string, st *workerState) *types.Interaction {
	f := st.finding
	in := &types.Interaction{
		ID:           id,
		Agent:        agentWorker,
		Query:        st.query,
		Tools:        workerTools(w.deps),
		Status:       f.Status,
		StatusDetail: f.Detail,
		Citations:    f.Citations,
		Usage:        f.Usage,
		CreatedAt:    st.started,
		CompletedAt:  st.completed,
	}
	if f.Text != "" {
		in.Outputs = []types.Output{{Type: types.OutputText, Text: f.Text}}
	}
	return in
}

// state returns the run record for id, or an error if the id is unknown.
func (w *Worker) state(id string) (*workerState, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.runs[id]
	if !ok {
		return nil, fmt.Errorf("fleet: no worker run with id %q", id)
	}
	return st, nil
}

// agentWorker is the Agent value recorded on a worker Interaction.
const agentWorker = "worker"

// workerTools is the tool set recorded on a worker Interaction: the read-only
// web tools the loop dispatches, the knowledge store recall when configured,
// and knowledge_remember when the finding is saved back, so an operator
// reading the Interaction sees the write.
func workerTools(deps WorkerDeps) []string {
	tools := []string{"web_search", "web_fetch"}
	if deps.Knowledge != nil {
		tools = append(tools, "knowledge_recall")
	}
	if deps.Remember != nil {
		tools = append(tools, "knowledge_remember")
	}
	return tools
}

// Save-back bounds. rememberTimeout also bounds how long a slow store can
// delay Await, and how long a save can run on after Await has returned.
const (
	rememberTimeout  = 30 * time.Second
	maxRememberBytes = 32 << 10
	maxRememberName  = 120
)

// rememberFinding saves a Completed, non-empty finding to deps.Remember under
// its own timeout, detached from the run's cancellation. The outcome is
// recorded on span and logged, both scrubbed; a failure never changes the
// finding.
func (w *Worker) rememberFinding(ctx context.Context, id, objective string, f Finding, span trace.Span) {
	if w.deps.Remember == nil || f.Status != types.StatusCompleted || strings.TrimSpace(f.Text) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rememberTimeout)
	defer cancel()

	mem := rememberedFinding(id, objective, f, time.Now())
	ref, err := w.deps.Remember.Remember(ctx, w.deps.KnowledgeNamespace, mem)
	if err != nil {
		detail := boundDetail(secret.Scrub(err.Error()))
		if span != nil {
			span.SetAttr("remember_error", detail)
		}
		w.deps.logger().Warn("worker: saving the finding to the knowledge store failed",
			"interaction_id", id, "error", detail)
		return
	}
	loc := ref.Locator
	if loc == "" {
		loc = ref.Digest
	}
	loc = boundDetail(secret.Scrub(loc))
	if span != nil {
		span.SetAttr("remember_ref", loc)
	}
	w.deps.logger().Info("worker: saved the finding to the knowledge store",
		"interaction_id", id, "ref", loc)
}

// rememberedFinding is the Memory saved for a finding: the objective on one
// line, a provenance line, the objective in full when it does not fit on that
// line, the answer, and the cited locators, scrubbed and bounded to
// maxRememberBytes. The header leads the text because a store may keep only
// the text, dropping the labels, and name a memory from its first line: the
// first line must be the objective and the second must say that the memory
// is an unreviewed worker finding and which run produced it.
func rememberedFinding(id, objective string, f Finding, saved time.Time) memory.Memory {
	objective = secret.Scrub(objective)
	name := boundRunes(oneLine(objective), maxRememberName)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n(Chiron worker finding; interaction %s; saved %s)\n\n", name, id, saved.UTC().Format(time.RFC3339))
	if full := strings.TrimSpace(objective); full != name {
		b.WriteString(full)
		b.WriteString("\n\n")
	}
	b.WriteString(strings.TrimSpace(f.Text))
	if len(f.Citations) > 0 {
		b.WriteString("\n\nSources:")
		for _, c := range f.Citations {
			b.WriteString("\n")
			b.WriteString(c.URI)
		}
	}
	return memory.Memory{
		Text: boundBytes(secret.Scrub(b.String()), maxRememberBytes),
		Meta: memory.ArtifactMeta{
			Name: name,
			Labels: map[string]string{
				"kind":           "fact",
				"agent":          agentWorker,
				"interaction_id": id,
			},
		},
	}
}

// boundRunes cuts s to at most n runes.
func boundRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// newInteractionID mints an opaque, unguessable local id for one in-process
// run, under the researcher's prefix. 128 bits of randomness makes collisions
// within a process negligible.
func newInteractionID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("fleet: generating interaction id: %w", err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}
