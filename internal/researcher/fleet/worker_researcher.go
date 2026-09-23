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
// Await blocks on that run; if Await's context ends first the run is
// cancelled, because an in-process run nobody is waiting for can only waste
// paid turns. Result maps the Finding. When deps.Remember is set, a Completed
// finding is saved to the knowledge store before Await returns (bounded by
// rememberTimeout). Unlike the Gemini adapter, the id is a local handle, not
// a durable resume token: a crashed in-process run cannot be recovered by
// `chiron get <id>`.
type Worker struct {
	deps WorkerDeps

	mu   sync.Mutex
	runs map[string]*workerState
}

// workerState is the per-run record keyed by interaction id. done is closed
// exactly once, when the loop returns; cancel stops the loop early.
type workerState struct {
	query   string
	started time.Time
	cancel  context.CancelFunc

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
	if deps.Model == nil {
		return nil, errWorkerNoModel
	}
	if deps.Search == nil {
		return nil, errors.New("fleet: worker requires a search client")
	}
	if deps.Fetch == nil {
		return nil, errors.New("fleet: worker requires a fetch client")
	}
	if deps.Caps.MaxTurns <= 0 {
		return nil, fmt.Errorf("fleet: worker requires a positive turn cap, got %d", deps.Caps.MaxTurns)
	}
	return &Worker{
		deps: deps,
		runs: make(map[string]*workerState),
	}, nil
}

// Start implements researcher.Researcher. It builds a Brief from the query,
// allocates an opaque local id, launches RunWorker in a goroutine, and returns
// the id immediately. A follow-up (PreviousInteractionID set) is rejected
// before any work starts: the worker has no stored interaction chain.
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

	id, err := newInteractionID()
	if err != nil {
		return "", err
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	st := &workerState{
		query:   task.Query,
		started: time.Now(),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	w.mu.Lock()
	w.runs[id] = st
	w.mu.Unlock()

	brief := Brief{Objective: task.Query}
	remember := func(ctx context.Context, f Finding, span trace.Span) {
		w.rememberFinding(ctx, id, brief.Objective, f, span)
	}
	go func() {
		defer cancel()
		st.finding = runWorker(runCtx, w.deps, brief, remember)
		st.completed = time.Now()
		close(st.done)
	}()

	return id, nil
}

// Await implements researcher.Researcher: block until the run for id finishes.
// If ctx ends first, the run is cancelled so no further paid turns are taken,
// and the context error is returned. An unknown id is a programming fault, not
// a user condition.
func (w *Worker) Await(ctx context.Context, id string) error {
	st, err := w.state(id)
	if err != nil {
		return err
	}
	select {
	case <-st.done:
		return nil
	case <-ctx.Done():
		st.cancel()
		return fmt.Errorf("fleet: awaiting worker %s: %w", id, ctx.Err())
	}
}

// Result implements researcher.Researcher: map the finished run's Finding onto
// a types.Interaction. Called before completion, it reports the in-progress
// state rather than blocking.
func (w *Worker) Result(_ context.Context, id string) (*types.Interaction, error) {
	st, err := w.state(id)
	if err != nil {
		return nil, err
	}
	select {
	case <-st.done:
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
// delay Await.
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

// rememberedFinding is the Memory saved for a finding: a provenance header,
// the objective, the answer, and the cited locators, scrubbed and bounded to
// maxRememberBytes. The header leads the text because a store may keep only
// the text, dropping the labels, and a reader must still see that the memory
// is an unreviewed worker finding and which run produced it.
func rememberedFinding(id, objective string, f Finding, saved time.Time) memory.Memory {
	var b strings.Builder
	fmt.Fprintf(&b, "Chiron worker finding\ninteraction: %s\nsaved: %s\n\n", id, saved.UTC().Format(time.RFC3339))
	b.WriteString(strings.TrimSpace(objective))
	b.WriteString("\n\n")
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
			Name: boundRunes(oneLine(secret.Scrub(objective)), maxRememberName),
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
// run. 128 bits of randomness makes collisions within a process negligible.
func newInteractionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("fleet: generating interaction id: %w", err)
	}
	return InteractionIDPrefix + hex.EncodeToString(b[:]), nil
}
