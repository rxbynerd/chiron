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

	"github.com/rxbynerd/chiron/internal/researcher"
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
// paid turns. Result maps the Finding. Unlike the Gemini adapter, the id is a
// local handle, not a durable resume token: a crashed in-process run cannot be
// recovered by `chiron get <id>`.
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

	done    chan struct{}
	finding Finding
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
	go func() {
		defer cancel()
		st.finding = RunWorker(runCtx, w.deps, brief)
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
			Tools:     workerTools(),
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
		Tools:        workerTools(),
		Status:       f.Status,
		StatusDetail: f.Detail,
		Citations:    f.Citations,
		Usage:        f.Usage,
		CreatedAt:    st.started,
		CompletedAt:  time.Now(),
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
// web tools the loop dispatches.
func workerTools() []string {
	return []string{"web_search", "web_fetch"}
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
