package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/types"
)

// Worker is the single-query binding of the Researcher seam
// (docs/V2-RESEARCH-AGENT §5, "Mapping to Chiron types"): it adapts one
// user query to one bounded RunWorker run and maps the resulting Finding onto
// a types.Interaction the existing formatter and sinks render. It is the
// --agent worker path. Wave 4's fleet lead is a separate binding that will
// dispatch many RunWorker runs; both reuse RunWorker unchanged.
//
// Start allocates an opaque local id and launches the loop in a goroutine,
// returning the id immediately (§3): the run core emits it as the resume
// handle before awaiting. Await blocks on that run; Result maps its Finding.
// Unlike the Gemini adapter, the id is a local handle, not a durable
// server-side resume token — a crashed in-process run cannot be recovered by
// `chiron get <id>` (§3), which is the accepted limitation of the in-process
// path until control-plane durability lands.
type Worker struct {
	deps WorkerDeps

	mu   sync.Mutex
	runs map[string]*workerState
}

// workerState is the per-run record keyed by interaction id: the goroutine's
// completion signal, the query the run answers, and the Finding once done.
// done is closed exactly once, when the loop returns.
type workerState struct {
	query   string
	started time.Time

	done    chan struct{}
	finding Finding
}

var _ researcher.Researcher = (*Worker)(nil)

// NewWorker builds the worker researcher over shared deps. It validates the
// collaborators that a run cannot proceed without — the model, search and
// fetch clients — at construction, so a misconfigured worker fails at the
// composition root rather than mid-run after a resume handle has been emitted.
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
// the id immediately. A follow-up (PreviousInteractionID set) is not supported
// by the in-process worker — the worker has no stored interaction chain to
// follow up on — so it is rejected before any work starts.
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

	st := &workerState{
		query:   task.Query,
		started: time.Now(),
		done:    make(chan struct{}),
	}
	w.mu.Lock()
	w.runs[id] = st
	w.mu.Unlock()

	brief := Brief{Objective: task.Query}
	// The run is detached from the caller's context: Start returns immediately
	// (§3) and the goroutine outlives this call. Cancellation and the
	// wall-clock bound are carried by Await's context and the per-worker
	// Timeout cap inside RunWorker — the run core awaits under its own
	// deadline. Using the Start ctx here would cancel the run the instant
	// Start returns.
	go func() {
		st.finding = RunWorker(context.WithoutCancel(ctx), w.deps, brief)
		close(st.done)
	}()

	return id, nil
}

// Await implements researcher.Researcher: block until the run for id finishes,
// or until ctx is cancelled. An unknown id is an error — the run core only
// ever awaits an id Start returned, so an unknown id is a programming fault,
// not a user condition.
func (w *Worker) Await(ctx context.Context, id string) error {
	st, err := w.state(id)
	if err != nil {
		return err
	}
	select {
	case <-st.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("fleet: awaiting worker %s: %w", id, ctx.Err())
	}
}

// Result implements researcher.Researcher: map the finished run's Finding onto
// a types.Interaction. It is called after Await returns, so the run is done;
// if it is called before completion (Result on an unfinished run), it reports
// the in-progress state rather than blocking or racing the Finding.
func (w *Worker) Result(_ context.Context, id string) (*types.Interaction, error) {
	st, err := w.state(id)
	if err != nil {
		return nil, err
	}
	select {
	case <-st.done:
		return w.mapInteraction(id, st), nil
	default:
		// Not finished: report in-progress. The run core always awaits before
		// retrieving, so this is a defensive branch, not the normal path.
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
// formatter renders (docs/V2-RESEARCH-AGENT §5). Status comes from the
// Finding; the answer becomes the single text Output that is the report body;
// the Finding's citations (already deduplicated by the loop) become the
// Citation list; usage carries the accumulated counters; StatusDetail carries
// the Finding's diagnostic. Agent, Query, Tools and timestamps are the
// worker's own contributions — the API-shaped fields the loop does not carry.
// No separate reporting path is added: this one Interaction is the whole
// output surface.
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

// agentWorker is the Agent value recorded on a worker Interaction — the tier
// name the report's front matter and the run's events carry.
const agentWorker = "worker"

// workerTools is the tool set recorded on a worker Interaction, the read-only
// web tools the loop dispatches. It matches the tool-name convention the
// formatter's front matter expects (a flat list of tool type names).
func workerTools() []string {
	return []string{"web_search", "web_fetch"}
}

// newInteractionID mints an opaque, unguessable local id for one in-process
// run. It is a local handle only (not a durable resume token): the "wkr_"
// prefix marks its origin, distinct from the Gemini adapter's server-issued
// ids. 128 bits of randomness makes collisions within a process negligible.
func newInteractionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("fleet: generating interaction id: %w", err)
	}
	return "wkr_" + hex.EncodeToString(b[:]), nil
}
