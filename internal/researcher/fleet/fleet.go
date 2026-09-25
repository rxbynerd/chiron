// Package fleet holds Chiron's own in-process research agents behind the
// Researcher seam: Worker, a bounded search -> read -> synthesise loop over
// one standard model with a web-search MCP tool, web_fetch and, when
// configured, knowledge store recall (docs/KNOWLEDGE.md), and Fleet, a lead
// that decomposes the query, runs the briefs as a bounded pool of those
// workers and synthesises a cited report from their findings
// (docs/V2-RESEARCH-AGENT.md §6). To the run core both are just Researchers.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/secret"
	"github.com/rxbynerd/chiron/internal/types"
)

// FleetInteractionIDPrefix marks an id minted by the in-process fleet. Like
// a worker id it names no server-side state, so the CLI refuses it for
// `chiron get`/`follow-up`.
const FleetInteractionIDPrefix = "flt_"

// IsFleetInteractionID reports whether id was minted by the in-process
// fleet.
func IsFleetInteractionID(id string) bool {
	return strings.HasPrefix(id, FleetInteractionIDPrefix)
}

// agentFleet is the Agent value recorded on a fleet Interaction.
const agentFleet = "fleet"

// sessionCloseTimeout bounds closing a run's session. The close is detached
// from the run's cancellation, so a cancelled run still releases it.
const sessionCloseTimeout = 5 * time.Second

// cancelUnwindGrace bounds how long Await waits for a run it has cancelled
// to unwind. Storing a finding, reading the findings back and closing the
// session are detached from the run's cancellation under their own bounds,
// so the grace covers all three.
const cancelUnwindGrace = findingPutTimeout + findingsReadTimeout + sessionCloseTimeout

// validationNamespace stands in for a run's session when NewFleet checks
// that a run's lead and pool can be built.
const validationNamespace memory.Namespace = "fleet-validation"

// FleetDeps are the fleet researcher's collaborators and fan-out caps.
type FleetDeps struct {
	// Worker is shared by every worker and by the lead. Its Model, Tracer
	// and Logger serve the lead as well; its Caps bound each worker and
	// price the lead's calls; its ReportTemplate shapes the synthesis, never
	// a worker's brief. Remember must be nil: a fleet never saves to the
	// knowledge store.
	Worker WorkerDeps
	// Store holds each run's session: the plan and every finding pass
	// through it by reference. A run opens its own session at Start and
	// closes it once the run has concluded.
	Store memory.ContextStore
	// MaxWorkers caps how many workers one run starts; Concurrency caps how
	// many run at once. Both are positive, and Concurrency does not exceed
	// MaxWorkers.
	MaxWorkers  int
	Concurrency int

	// unwindGraceOverride replaces cancelUnwindGrace when positive, so tests
	// can shorten it.
	unwindGraceOverride time.Duration
}

// unwindGrace is the bound on waiting for a cancelled run to unwind.
func (d FleetDeps) unwindGrace() time.Duration {
	if d.unwindGraceOverride > 0 {
		return d.unwindGraceOverride
	}
	return cancelUnwindGrace
}

// Fleet is the multi-worker binding of the Researcher seam. One run is a
// lead that decomposes the query into briefs, a bounded pool that runs each
// brief as an in-process worker and stores its finding in the run's session
// by reference, then the lead's synthesis and citation passes over the
// findings read back. The run's Interaction carries the one rolled-up Usage.
//
// Start mints an opaque local id, opens the run's session and launches the
// lead in a goroutine, returning the id immediately so the run core can
// emit it before awaiting. Await blocks on the run; if Await's context ends
// before the lead has concluded, the run is cancelled and Await waits, up to
// cancelUnwindGrace, for every worker and lead pass to return, so no paid
// call outlives it. Result maps the concluded outcome. As with Worker, the id
// is a local handle, not a durable resume token.
//
// Every worker's Progress deliveries are held until Await is first entered
// for the run, as Worker holds its own.
type Fleet struct {
	deps FleetDeps
	// routes is the one routing table a run's lead validates its plan
	// against and its pool dispatches the plan through.
	routes router

	mu   sync.Mutex
	runs map[string]*fleetState
}

// fleetState is the per-run record keyed by interaction id. concluded is
// closed once outcome and completed are stored, after the lead's last paid
// call; done is closed once the session is closed as well. outcome and
// completed are written only before concluded closes. cancel stops the run
// early. awaiting is the progress gate, closed once by the first Await.
type fleetState struct {
	query   string
	started time.Time
	cancel  context.CancelFunc

	awaiting     chan struct{}
	awaitingOnce sync.Once

	concluded chan struct{}
	done      chan struct{}
	outcome   fleetOutcome
	completed time.Time
}

var _ researcher.Researcher = (*Fleet)(nil)

// NewFleet builds the fleet researcher. It validates everything a run needs
// at construction, including building a run's lead and pool once, so a
// misconfigured fleet fails at the composition root rather than after Start
// has minted an id.
func NewFleet(deps FleetDeps) (*Fleet, error) {
	switch {
	case deps.Store == nil:
		return nil, errors.New("fleet: requires a context store")
	case deps.Worker.Remember != nil:
		return nil, errors.New("fleet: a fleet never saves findings to the knowledge store; WorkerDeps.Remember must be nil")
	}
	f := &Fleet{
		deps:   deps,
		routes: liveRoutes(),
		runs:   make(map[string]*fleetState),
	}
	if _, _, err := f.components(validationNamespace); err != nil {
		return nil, err
	}
	return f, nil
}

// components builds one run's lead and pool over the session namespace ns.
// Both take the Fleet's one routing table, and the lead's prices come from
// the workers' Caps, so the whole run is priced alike.
func (f *Fleet) components(ns memory.Namespace) (*lead, *pool, error) {
	wd := f.deps.Worker
	l, err := newLead(leadDeps{
		Model:            wd.Model,
		Store:            f.deps.Store,
		Namespace:        ns,
		Tracer:           wd.Tracer,
		Logger:           wd.Logger,
		Routes:           f.routes,
		ReportTemplate:   wd.ReportTemplate,
		InputGBPPerMTok:  wd.Caps.InputGBPPerMTok,
		OutputGBPPerMTok: wd.Caps.OutputGBPPerMTok,
	})
	if err != nil {
		return nil, nil, err
	}
	p, err := newPool(poolDeps{
		Worker:      wd,
		Routes:      f.routes,
		Store:       f.deps.Store,
		Namespace:   ns,
		MaxWorkers:  f.deps.MaxWorkers,
		Concurrency: f.deps.Concurrency,
	})
	if err != nil {
		return nil, nil, err
	}
	return l, p, nil
}

// Start implements researcher.Researcher. It refuses an empty query, a
// follow-up (the fleet has no stored interaction chain) and a report
// template that fails to render for the query, then mints an opaque local
// id, opens the run's session under it and launches the run in a goroutine,
// returning the id immediately. Every refusal, a failed session open
// included, happens before an id is returned or anything is spent.
//
// The run is detached from the Start context's cancellation (the run core
// scopes that context to the start phase) and given its own cancel, which
// Await triggers when its caller stops waiting.
func (f *Fleet) Start(ctx context.Context, task researcher.Task) (string, error) {
	if strings.TrimSpace(task.Query) == "" {
		return "", errors.New("fleet: the query must not be empty")
	}
	if task.PreviousInteractionID != "" {
		return "", errors.New("fleet: the in-process fleet does not support follow-up interactions")
	}
	if rt := f.deps.Worker.ReportTemplate; rt != nil {
		if _, err := rt.Render(task.Query); err != nil {
			return "", err
		}
	}

	id, err := newInteractionID(FleetInteractionIDPrefix)
	if err != nil {
		return "", err
	}
	sess, err := f.deps.Store.OpenSession(ctx, memory.SessionRef{ID: id})
	if err != nil {
		return "", &sessionOpenError{detail: boundDetail(secret.Scrub(err.Error())), err: err}
	}
	l, p, err := f.components(sess.Namespace())
	if err != nil {
		f.closeSession(ctx, id, sess)
		return "", err
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	st := &fleetState{
		query:     task.Query,
		started:   time.Now(),
		cancel:    cancel,
		awaiting:  make(chan struct{}),
		concluded: make(chan struct{}),
		done:      make(chan struct{}),
	}
	f.mu.Lock()
	f.runs[id] = st
	f.mu.Unlock()

	go func() {
		defer close(st.done)
		defer cancel()
		st.outcome = l.conduct(runCtx, task.Query, p, st.awaiting)
		st.completed = time.Now()
		close(st.concluded)
		f.closeSession(runCtx, id, sess)
	}()
	return id, nil
}

// sessionOpenError reports a failed session open with a bounded, scrubbed
// message while keeping the store's error reachable via errors.Is/errors.As,
// as persistPlanError does for the plan.
type sessionOpenError struct {
	detail string
	err    error
}

func (e *sessionOpenError) Error() string {
	return "fleet: opening the run's session: " + e.detail
}

func (e *sessionOpenError) Unwrap() error { return e.err }

// conduct runs one fleet run's lead flow to its outcome: decompose, run the
// plan through p with every worker's progress held behind progressGate, then
// conclude. A failed decomposition dispatches nothing: the run is Failed,
// with the decomposition's usage because a refused reply is still billed.
func (l *lead) conduct(ctx context.Context, query string, p *pool, progressGate <-chan struct{}) fleetOutcome {
	plan, planUsage, err := l.decompose(ctx, query)
	if err != nil {
		return fleetOutcome{
			Status: types.StatusFailed,
			Detail: boundDetail(secret.Scrub(err.Error())),
			Usage:  l.runUsage(types.Usage{}, planUsage),
		}
	}
	pooled := p.run(ctx, plan.Briefs, progressGate)
	return l.conclude(ctx, query, plan, planUsage, pooled)
}

// closeSession closes a run's session under sessionCloseTimeout, detached
// from ctx's cancellation. A failure is logged, scrubbed, and never changes
// the run's outcome.
func (f *Fleet) closeSession(ctx context.Context, id string, sess memory.Session) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCloseTimeout)
	defer cancel()
	if err := sess.Close(ctx); err != nil {
		f.deps.Worker.logger().Warn("fleet: closing the run's session failed",
			"interaction_id", id, "error", boundDetail(secret.Scrub(err.Error())))
	}
}

// Await implements researcher.Researcher: block until the run for id
// finishes, its session closed. Entering Await releases the run's held
// progress deliveries. If ctx ends after the lead has concluded, Await
// returns nil: the paid work is complete, and only the session close
// remains. If ctx ends before then, Await cancels the run, waits up to
// cancelUnwindGrace for it to unwind, and returns the context error; a run
// that unwound within the grace has concluded, so Result then reports the
// cancelled run's outcome and usage. An unknown id is a programming fault,
// not a user condition.
func (f *Fleet) Await(ctx context.Context, id string) error {
	st, err := f.state(id)
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
	case <-st.concluded:
		return nil
	default:
	}

	st.cancel()
	grace := time.NewTimer(f.deps.unwindGrace())
	defer grace.Stop()
	select {
	case <-st.done:
	case <-grace.C:
	}
	return fmt.Errorf("fleet: awaiting fleet run %s: %w", id, ctx.Err())
}

// Result implements researcher.Researcher: map the run's outcome onto a
// types.Interaction once the lead has concluded, whether or not its session
// close is still running. Called before then, it reports the in-progress
// state rather than blocking.
func (f *Fleet) Result(_ context.Context, id string) (*types.Interaction, error) {
	st, err := f.state(id)
	if err != nil {
		return nil, err
	}
	in := &types.Interaction{
		ID:        id,
		Agent:     agentFleet,
		Query:     st.query,
		Tools:     fleetTools(f.deps.Worker),
		CreatedAt: st.started,
	}
	select {
	case <-st.concluded:
		st.outcome.applyTo(in)
		in.CompletedAt = st.completed
	default:
		in.Status = types.StatusInProgress
	}
	return in, nil
}

// state returns the run record for id, or an error if the id is unknown.
func (f *Fleet) state(id string) (*fleetState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.runs[id]
	if !ok {
		return nil, fmt.Errorf("fleet: no fleet run with id %q", id)
	}
	return st, nil
}

// fleetTools is the tool set recorded on a fleet Interaction: the read-only
// tools its workers dispatch. A fleet never saves back, so knowledge_remember
// is never listed.
func fleetTools(deps WorkerDeps) []string {
	deps.Remember = nil
	return workerTools(deps)
}
