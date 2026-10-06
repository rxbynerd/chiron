package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/secret"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/types"
)

// findingPutTimeout bounds storing one finding. The Put is detached from the
// run's cancellation, because a cancelled worker's partial finding is
// already paid for; the bound stops a slow store holding the pool open.
const findingPutTimeout = 10 * time.Second

// maxFindingBytes bounds a stored finding, on write and on read-back.
const maxFindingBytes = memory.DefaultMaxArtifactBytes

// findingMediaType is the stored finding's media type.
const findingMediaType = "application/json"

// findingDocumentKind identifies a stored finding, as planDocumentKind does
// the plan.
const findingDocumentKind = "fleet_finding"

// findingDocumentVersion is the stored finding's schema version.
const findingDocumentVersion = 1

// maxFindingEchoRunes bounds a stored field a read-back error repeats.
const maxFindingEchoRunes = 64

// ErrInvalidFinding wraps every refusal of a stored finding's body.
var ErrInvalidFinding = errors.New("fleet: the stored finding is invalid")

// poolDeps are the worker pool's collaborators and fan-out caps.
type poolDeps struct {
	// Worker is shared by every worker the pool runs, and its Caps apply to
	// each worker unchanged. The pool ignores Remember and ReportTemplate: a
	// fleet worker never saves to the knowledge store, and its brief carries
	// its output format.
	Worker WorkerDeps
	// Routes dispatches each brief by target. Pass the table that validated
	// the plan; nil selects liveRoutes.
	Routes router
	// Store and Namespace receive each finding as its worker returns:
	// Namespace is the run's open session.
	Store     memory.ContextStore
	Namespace memory.Namespace
	// MaxWorkers caps how many workers one run starts; a brief past it is
	// dropped. Concurrency caps how many run at once. Both are positive, and
	// Concurrency does not exceed MaxWorkers.
	MaxWorkers  int
	Concurrency int
}

// pool runs a plan's briefs as bounded, concurrent workers.
type pool struct {
	deps   poolDeps
	tracer trace.Tracer
}

// briefDisposition says what the pool did with a planned brief.
type briefDisposition string

const (
	// dispositionRan: a worker ran the brief to a Finding.
	dispositionRan briefDisposition = "ran"
	// dispositionDropped: the brief was past the worker cap, or its target
	// has no route, so no worker ran it.
	dispositionDropped briefDisposition = "dropped"
	// dispositionNotStarted: the run's context ended before a worker slot
	// was free for the brief.
	dispositionNotStarted briefDisposition = "not_started"
)

// briefResult is what the pool reports for one planned brief: the reference
// to its stored finding and the metadata a caller needs without reading the
// finding back.
type briefResult struct {
	BriefID string
	// WorkerID is set only when Disposition is dispositionRan.
	WorkerID    string
	Disposition briefDisposition
	// Ref is the stored finding; zero when no worker ran the brief or the
	// store refused its finding.
	Ref memory.Reference
	// Status, Usage, Turns and CitationCount come from the worker's Finding
	// and are zero when no worker ran the brief.
	Status        types.Status
	Usage         types.Usage
	Turns         int
	CitationCount int
	// Detail is the Finding's detail, followed by the store error when the
	// finding was not stored; for a brief no worker ran, the reason.
	Detail string
}

// poolResult is one briefResult per planned brief, in plan order, and the
// summed usage of every worker that ran.
type poolResult struct {
	Briefs []briefResult
	Usage  types.Usage
}

// newPool validates deps, as NewWorker does the worker collaborators, and
// applies the defaults.
func newPool(deps poolDeps) (*pool, error) {
	if err := checkWorkerDeps(deps.Worker); err != nil {
		return nil, err
	}
	switch {
	case deps.Store == nil:
		return nil, errors.New("fleet: the worker pool requires a context store")
	case deps.Namespace == "":
		return nil, errors.New("fleet: the worker pool requires a session namespace")
	case deps.MaxWorkers <= 0:
		return nil, fmt.Errorf("fleet: the worker pool's worker cap %d is not positive", deps.MaxWorkers)
	case deps.Concurrency <= 0:
		return nil, fmt.Errorf("fleet: the worker pool's concurrency %d is not positive", deps.Concurrency)
	case deps.Concurrency > deps.MaxWorkers:
		return nil, fmt.Errorf("fleet: the worker pool's concurrency %d exceeds its worker cap %d", deps.Concurrency, deps.MaxWorkers)
	}
	if deps.Routes == nil {
		deps.Routes = liveRoutes()
	}
	deps.Worker.Remember = nil
	deps.Worker.ReportTemplate = nil
	tracer := deps.Worker.Tracer
	if tracer == nil {
		tracer = trace.Noop{}
	}
	return &pool{deps: deps, tracer: tracer}, nil
}

// run dispatches each brief through the router to its own worker goroutine,
// with at most Concurrency running and at most MaxWorkers started; a brief
// past the cap is dropped, never queued. Once ctx ends no further worker
// starts, running workers stop at their next context check, and each brief
// still waiting is reported not started. A non-nil progressGate holds every
// worker's progress deliveries until it is closed.
//
// run returns only after every worker goroutine it started has returned and
// stored its finding; if dispatching panics, it cancels those workers first.
// Results are in brief order, whatever the completion order. A failed worker
// is never dispatched again.
func (p *pool) run(ctx context.Context, briefs []plannedBrief, progressGate <-chan struct{}) poolResult {
	ctx, cancel := context.WithCancel(ctx)
	results := make([]briefResult, len(briefs))
	slots := make(chan struct{}, p.deps.Concurrency)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	for i, pb := range briefs {
		if i >= p.deps.MaxWorkers {
			results[i] = p.skip(ctx, pb, dispositionDropped, fmt.Sprintf(
				"dropped: brief %d of %d is past the %d-worker cap", i+1, len(briefs), p.deps.MaxWorkers))
			continue
		}
		dispatch, err := p.deps.Routes.route(pb.Target)
		if err != nil {
			results[i] = p.skip(ctx, pb, dispositionDropped, "dropped: "+err.Error())
			continue
		}
		if !acquireSlot(ctx, slots) {
			results[i] = p.skip(ctx, pb, dispositionNotStarted,
				"not started: the run ended before a worker slot was free: "+ctx.Err().Error())
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			results[i] = p.runBrief(ctx, fmt.Sprintf("worker-%d", i+1), pb, dispatch, progressGate)
		}()
	}
	wg.Wait()

	out := poolResult{Briefs: results}
	for _, r := range results {
		out.Usage = addUsage(out.Usage, r.Usage)
	}
	return out
}

// acquireSlot takes a worker slot, or reports false once ctx has ended. A
// slot won in the same instant ctx ends is handed back, so a cancelled run
// starts nothing.
func acquireSlot(ctx context.Context, slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	if ctx.Err() != nil {
		<-slots
		return false
	}
	return true
}

// panicRecoveryDetail is the finding detail for a worker goroutine that
// panicked. The recovered value is never included: it can carry request or
// response content from an attacker-influenced source.
const panicRecoveryDetail = "the worker's goroutine panicked"

// dispatchSafely runs dispatch, recovering a panic into a Failed Finding so
// one brief's crash cannot take the process, or a sibling brief still in
// flight, down with it.
func (p *pool) dispatchSafely(ctx context.Context, dispatch dispatchFunc, deps WorkerDeps, brief Brief, progressGate <-chan struct{}) (f Finding) {
	defer func() {
		if recover() != nil {
			f = Finding{Status: types.StatusFailed, Detail: panicRecoveryDetail}
		}
	}()
	return dispatch(ctx, deps, brief, progressGate)
}

// runBrief runs one brief under its own delegate span, stores the finding
// as soon as the worker returns, and records the outcome on the span. The
// worker's own span nests beneath the delegate.
func (p *pool) runBrief(ctx context.Context, workerID string, pb plannedBrief, dispatch dispatchFunc, progressGate <-chan struct{}) briefResult {
	ctx, span := p.tracer.StartSpan(ctx, trace.SpanDelegate)
	span.SetAttr(trace.AttrWorkerID, workerID)
	span.SetAttr(trace.AttrBriefID, pb.ID)
	span.SetAttr("disposition", string(dispositionRan))

	f := p.dispatchSafely(ctx, dispatch, p.workerDeps(workerID), pb.Brief, progressGate)
	res := briefResult{
		BriefID:       pb.ID,
		WorkerID:      workerID,
		Disposition:   dispositionRan,
		Status:        f.Status,
		Usage:         f.Usage,
		Turns:         f.Turns,
		CitationCount: len(f.Citations),
		Detail:        f.Detail,
	}
	ref, err := p.storeFinding(ctx, workerID, pb.ID, f)
	stored := err == nil
	if stored {
		res.Ref = ref
	} else {
		storeErr := "the finding was not stored: " + boundDetail(secret.Scrub(err.Error()))
		res.Detail = joinDetail(f.Detail, storeErr)
		span.SetAttr("store_error", storeErr)
	}

	// The digest is not an attribute: the tracers' scrubber redacts a
	// SHA-256 hex digest as high-entropy. The brief id correlates the span
	// with the stored finding, whose body carries it.
	span.SetAttr("finding_stored", stored)
	span.SetAttr("status", string(f.Status))
	span.SetAttr("turns", f.Turns)
	span.SetAttr("citation_count", res.CitationCount)
	span.SetAttr("search_count", f.Usage.SearchCount)
	span.SetAttr("recall_count", f.Usage.RecallCount)
	span.SetAttr("input_tokens", f.Usage.InputTokens)
	span.SetAttr("output_tokens", f.Usage.OutputTokens)
	span.SetAttr("estimated_cost_gbp", f.Usage.EstimatedCostGBP)
	if f.Detail != "" {
		span.SetAttr("detail", f.Detail)
	}
	var spanErr error
	if f.Status == types.StatusFailed || !stored {
		spanErr = errors.New(res.Detail)
	}
	span.End(spanErr)
	return res
}

// skip reports a brief no worker ran: a delegate span with no worker beneath
// it, and a result carrying the reason.
func (p *pool) skip(ctx context.Context, pb plannedBrief, d briefDisposition, reason string) briefResult {
	_, span := p.tracer.StartSpan(ctx, trace.SpanDelegate)
	span.SetAttr(trace.AttrBriefID, pb.ID)
	span.SetAttr("disposition", string(d))
	span.SetAttr("reason", reason)
	span.End(nil)
	return briefResult{BriefID: pb.ID, Disposition: d, Detail: reason}
}

// workerDeps returns the shared worker deps with each progress delivery
// stamped with workerID.
func (p *pool) workerDeps(workerID string) WorkerDeps {
	deps := p.deps.Worker
	if hook := deps.Progress; hook != nil {
		deps.Progress = func(ctx context.Context, pr Progress) {
			pr.WorkerID = workerID
			hook(ctx, pr)
		}
	}
	return deps
}

// findingDocument is a stored finding. Its identity lives in the body
// because a store may coalesce identical content under the first write's
// meta.
type findingDocument struct {
	Kind     string        `json:"kind"`
	Version  int           `json:"version"`
	WorkerID string        `json:"worker_id"`
	BriefID  string        `json:"brief_id"`
	Finding  findingRecord `json:"finding"`
}

// findingRecord is a Finding's stored form. It mirrors Finding field for
// field so each converts to the other, and a field added to Finding does not
// compile until it is added here too.
type findingRecord struct {
	Text      string           `json:"text"`
	Citations []types.Citation `json:"citations"`
	Usage     types.Usage      `json:"usage"`
	Status    types.Status     `json:"status"`
	Detail    string           `json:"detail,omitempty"`
	Turns     int              `json:"turns"`
}

// storeFinding writes f to the run's session under its own timeout,
// detached from the run's cancellation.
func (p *pool) storeFinding(ctx context.Context, workerID, briefID string, f Finding) (memory.Reference, error) {
	body, err := json.Marshal(findingDocument{
		Kind:     findingDocumentKind,
		Version:  findingDocumentVersion,
		WorkerID: workerID,
		BriefID:  briefID,
		Finding:  findingRecord(f),
	})
	if err != nil {
		return memory.Reference{}, err
	}
	if len(body) > maxFindingBytes {
		return memory.Reference{}, fmt.Errorf("the finding is %d bytes, over the %d-byte bound", len(body), maxFindingBytes)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), findingPutTimeout)
	defer cancel()
	return p.deps.Store.Put(ctx, p.deps.Namespace, bytes.NewReader(body), memory.ArtifactMeta{
		Name:      "fleet-finding-" + briefID + ".json",
		MediaType: findingMediaType,
	})
}

// storedFinding is a finding read back from the store with its identity.
type storedFinding struct {
	WorkerID string
	BriefID  string
	Finding  Finding
}

// findingReadError reports a failed read-back with a bounded, scrubbed
// message while keeping the store's error reachable via errors.Is/errors.As.
type findingReadError struct {
	detail string
	err    error
}

func (e *findingReadError) Error() string {
	return "fleet: reading a stored finding: " + e.detail
}

func (e *findingReadError) Unwrap() error { return e.err }

// readFinding fetches the finding ref names and decodes it strictly. A body
// over maxFindingBytes, invalid JSON, an unknown field, trailing content,
// another kind or version, a missing worker or brief id, or a non-terminal
// status is ErrInvalidFinding; a store failure is a *findingReadError.
func readFinding(ctx context.Context, store memory.ContextStore, ref memory.Reference) (storedFinding, error) {
	rc, _, err := store.Get(ctx, ref)
	if err != nil {
		return storedFinding{}, &findingReadError{detail: boundDetail(secret.Scrub(err.Error())), err: err}
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, maxFindingBytes+1))
	if err != nil {
		return storedFinding{}, &findingReadError{detail: boundDetail(secret.Scrub(err.Error())), err: err}
	}
	if len(body) > maxFindingBytes {
		return storedFinding{}, fmt.Errorf("%w: the body is over the %d-byte bound", ErrInvalidFinding, maxFindingBytes)
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var doc findingDocument
	if err := dec.Decode(&doc); err != nil {
		return storedFinding{}, fmt.Errorf("%w: %s", ErrInvalidFinding, boundDetail(secret.Scrub(err.Error())))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return storedFinding{}, fmt.Errorf("%w: content follows the JSON object", ErrInvalidFinding)
	}
	switch {
	case doc.Kind != findingDocumentKind:
		return storedFinding{}, fmt.Errorf("%w: kind %q, want %q", ErrInvalidFinding, boundRunes(secret.Scrub(doc.Kind), maxFindingEchoRunes), findingDocumentKind)
	case doc.Version != findingDocumentVersion:
		return storedFinding{}, fmt.Errorf("%w: version %d, want %d", ErrInvalidFinding, doc.Version, findingDocumentVersion)
	case doc.WorkerID == "" || doc.BriefID == "":
		return storedFinding{}, fmt.Errorf("%w: the worker or brief id is missing", ErrInvalidFinding)
	case !doc.Finding.Status.Terminal():
		return storedFinding{}, fmt.Errorf("%w: status %q is not terminal", ErrInvalidFinding, boundRunes(secret.Scrub(string(doc.Finding.Status)), maxFindingEchoRunes))
	}
	return storedFinding{
		WorkerID: doc.WorkerID,
		BriefID:  doc.BriefID,
		Finding:  Finding(doc.Finding),
	}, nil
}

// joinDetail appends extra to detail.
func joinDetail(detail, extra string) string {
	if detail == "" {
		return extra
	}
	return detail + "; " + extra
}

// addUsage sums two usages field by field.
func addUsage(a, b types.Usage) types.Usage {
	a.InputTokens += b.InputTokens
	a.CachedTokens += b.CachedTokens
	a.OutputTokens += b.OutputTokens
	a.ToolUseTokens += b.ToolUseTokens
	a.ThoughtTokens += b.ThoughtTokens
	a.SearchCount += b.SearchCount
	a.RecallCount += b.RecallCount
	a.PollCount += b.PollCount
	a.ReconnectCount += b.ReconnectCount
	a.EstimatedCostGBP += b.EstimatedCostGBP
	return a
}
