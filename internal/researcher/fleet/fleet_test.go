package fleet

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/types"
)

// sessionStore is an InMemory store that records each session opened in it
// and each closed. It fails every OpenSession when openErr is set, holds
// each Close until closeGate is closed when that is set, and holds each Put
// until putGate is closed when that is set, ignoring the context either way.
type sessionStore struct {
	*memory.InMemory
	openErr   error
	closeGate chan struct{}
	putGate   chan struct{}

	mu     sync.Mutex
	opened []string
	closed []memory.Namespace
}

func newSessionStore() *sessionStore {
	return &sessionStore{InMemory: memory.NewInMemory(memory.InMemoryOptions{})}
}

func (s *sessionStore) OpenSession(ctx context.Context, ref memory.SessionRef) (memory.Session, error) {
	s.mu.Lock()
	s.opened = append(s.opened, ref.ID)
	s.mu.Unlock()
	if s.openErr != nil {
		return nil, s.openErr
	}
	sess, err := s.InMemory.OpenSession(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &recordedSession{Session: sess, store: s}, nil
}

func (s *sessionStore) Put(ctx context.Context, ns memory.Namespace, body io.Reader, meta memory.ArtifactMeta) (memory.Reference, error) {
	if s.putGate != nil {
		<-s.putGate
	}
	return s.InMemory.Put(ctx, ns, body, meta)
}

// sessions returns the session ids opened and the namespaces closed, in
// order.
func (s *sessionStore) sessions() ([]string, []memory.Namespace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.opened), slices.Clone(s.closed)
}

type recordedSession struct {
	memory.Session
	store *sessionStore
}

func (r *recordedSession) Close(ctx context.Context) error {
	if g := r.store.closeGate; g != nil {
		<-g
	}
	r.store.mu.Lock()
	r.store.closed = append(r.store.closed, r.Namespace())
	r.store.mu.Unlock()
	return r.Session.Close(ctx)
}

// newTestFleet builds a three-worker Fleet over one model endpoint and
// search fake, storing its sessions in store. edit, when non-nil, adjusts
// the deps first.
func newTestFleet(t *testing.T, modelURL string, searchSrv *searchtest.FakeServer, store memory.ContextStore, edit func(*FleetDeps)) *Fleet {
	t.Helper()
	deps := FleetDeps{
		Worker:      poolWorkerDeps(t, newModelClientAt(t, modelURL), searchSrv),
		Store:       store,
		MaxWorkers:  3,
		Concurrency: 3,
	}
	if edit != nil {
		edit(&deps)
	}
	f, err := NewFleet(deps)
	if err != nil {
		t.Fatalf("NewFleet: %v", err)
	}
	return f
}

// runState returns the Fleet's record for id.
func runState(t *testing.T, f *Fleet, id string) *fleetState {
	t.Helper()
	st, err := f.state(id)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	return st
}

// TestFleetStartAwaitResult: a fleet run through the Researcher seam opens
// and closes a session under its flt_ id, and Result carries the synthesised
// report, sources from both workers and one Usage with the lead's calls
// priced at the workers' rates.
func TestFleetStartAwaitResult(t *testing.T) {
	fm := syntheticFleetModel(t)
	modelSrv := httptest.NewServer(fm)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(syntheticSearchResults)
	defer searchSrv.Close()
	store := newSessionStore()
	const inPrice, outPrice = 2.0, 10.0
	f := newTestFleet(t, modelSrv.URL, searchSrv, store, func(d *FleetDeps) {
		d.Worker.Caps.InputGBPPerMTok, d.Worker.Caps.OutputGBPPerMTok = inPrice, outPrice
	})

	ctx := context.Background()
	id, err := f.Start(ctx, researcher.Task{Query: testQuery})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !IsFleetInteractionID(id) || IsWorkerInteractionID(id) || len(id) != len(FleetInteractionIDPrefix)+32 {
		t.Errorf("id = %q, want a local flt_ handle", id)
	}
	if opened, _ := store.sessions(); !slices.Equal(opened, []string{id}) {
		t.Errorf("sessions opened = %v, want one under the interaction id", opened)
	}

	if err := f.Await(ctx, id); err != nil {
		t.Fatalf("Await: %v", err)
	}
	awaited := time.Now()
	if _, closed := store.sessions(); !slices.Equal(closed, []memory.Namespace{memory.Namespace("session/" + id)}) {
		t.Errorf("sessions closed = %v, want the run's own", closed)
	}
	in, err := f.Result(ctx, id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}

	if in.Status != types.StatusCompleted || in.StatusDetail != "" {
		t.Errorf("status = %s (%s), want completed", in.Status, in.StatusDetail)
	}
	if in.ID != id || in.Agent != "fleet" || in.Query != testQuery {
		t.Errorf("identity = %q/%q/%q, want %q/fleet/the query", in.ID, in.Agent, in.Query, id)
	}
	if want := []string{"web_search", "web_fetch"}; !slices.Equal(in.Tools, want) {
		t.Errorf("tools = %v, want %v", in.Tools, want)
	}
	if len(in.Outputs) != 1 || in.Outputs[0].Type != types.OutputText || in.Outputs[0].Text != syntheticReport {
		t.Errorf("outputs = %+v, want the synthesised report", in.Outputs)
	}
	wantCites := []types.Citation{
		{URI: heatPumpURL, Title: "Heat pump running costs"},
		{URI: boilerURL, Title: "Gas boiler running costs"},
	}
	if !reflect.DeepEqual(in.Citations, wantCites) {
		t.Errorf("citations = %+v, want one source from each worker %+v", in.Citations, wantCites)
	}
	if in.CreatedAt.IsZero() || in.CompletedAt.Before(in.CreatedAt) || in.CompletedAt.After(awaited) {
		t.Errorf("timestamps = %v / %v, want the run's start and its conclusion before Await returned", in.CreatedAt, in.CompletedAt)
	}

	// Three workers of two turns at 10 input and 5 output tokens each, one
	// search each, plus the decompose, synthesis and citation calls.
	const workerIn, workerOut = 3 * 2 * 10, 3 * 2 * 5
	leadIn := decomposeUsage.InputTokens + synthesisUsage.InputTokens + citeUsage.InputTokens
	leadOut := decomposeUsage.OutputTokens + synthesisUsage.OutputTokens + citeUsage.OutputTokens
	u := in.Usage
	if u.InputTokens != workerIn+leadIn || u.OutputTokens != workerOut+leadOut || u.SearchCount != 3 {
		t.Errorf("usage tokens = %d in / %d out / %d searches, want %d / %d / 3", u.InputTokens, u.OutputTokens, u.SearchCount, workerIn+leadIn, workerOut+leadOut)
	}
	wantCost := costGBP(workerIn, workerOut, inPrice, outPrice) + costGBP(leadIn, leadOut, inPrice, outPrice)
	if math.Abs(u.EstimatedCostGBP-wantCost) > 1e-12 {
		t.Errorf("estimated cost = %v, want %v: the lead's calls priced at the workers' rates", u.EstimatedCostGBP, wantCost)
	}
	if n := searchSrv.ToolCallCount(); n != 3 {
		t.Errorf("search calls = %d, want one per worker", n)
	}
}

// TestFleetComponentsShareRoutesAndPrices: a run's lead validates its plan
// against the same routing table its pool dispatches through, and prices
// its calls at the workers' Caps rates.
func TestFleetComponentsShareRoutesAndPrices(t *testing.T) {
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	f := newTestFleet(t, modelSrv.URL(), searchSrv, newSessionStore(), func(d *FleetDeps) {
		d.Worker.Caps.InputGBPPerMTok, d.Worker.Caps.OutputGBPPerMTok = 1.25, 7.5
	})
	var dispatched int
	f.routes = countingRoutes(&dispatched)

	l, p, err := f.components("session/test")
	if err != nil {
		t.Fatalf("components: %v", err)
	}
	fleetTable := reflect.ValueOf(f.routes).UnsafePointer()
	if reflect.ValueOf(l.deps.Routes).UnsafePointer() != fleetTable || reflect.ValueOf(p.deps.Routes).UnsafePointer() != fleetTable {
		t.Error("the lead and the pool must share the Fleet's one routing table")
	}
	if l.deps.InputGBPPerMTok != 1.25 || l.deps.OutputGBPPerMTok != 7.5 {
		t.Errorf("lead prices = %v / %v, want the workers' 1.25 / 7.5", l.deps.InputGBPPerMTok, l.deps.OutputGBPPerMTok)
	}
	if p.deps.Worker.Caps != f.deps.Worker.Caps {
		t.Errorf("pool caps = %+v, want the Fleet's %+v", p.deps.Worker.Caps, f.deps.Worker.Caps)
	}
	if l.deps.Namespace != "session/test" || p.deps.Namespace != "session/test" {
		t.Errorf("namespaces = %q / %q, want the run's session", l.deps.Namespace, p.deps.Namespace)
	}
}

// TestFleetRefusesBeforeStarting: an empty query, a follow-up, a report
// template that fails to render for the query and a session the store
// refuses all fail Start with no id and no model request; only the last
// reaches the store.
func TestFleetRefusesBeforeStarting(t *testing.T) {
	failingTemplate := `{{if eq .Query "` + reportTemplateSampleQuery + `"}}Format.{{else}}{{.Missing}}{{end}}`
	for _, tt := range []struct {
		name     string
		task     researcher.Task
		template string
		openErr  error
		wantErr  string
		wantOpen int
	}{
		{name: "empty query", task: researcher.Task{Query: " \n"}, wantErr: "must not be empty"},
		{name: "follow-up", task: researcher.Task{Query: "q", PreviousInteractionID: "flt_prev"}, wantErr: "follow-up"},
		{name: "template render failure", task: researcher.Task{Query: "q"}, template: failingTemplate, wantErr: "can't evaluate field Missing"},
		{name: "session open failure", task: researcher.Task{Query: "q"}, openErr: memory.ErrSessionExists, wantErr: "opening the run's session", wantOpen: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			modelSrv := modeltest.NewFakeServer()
			defer modelSrv.Close()
			searchSrv := searchtest.NewFakeServer(nil)
			defer searchSrv.Close()
			store := newSessionStore()
			store.openErr = tt.openErr
			f := newTestFleet(t, modelSrv.URL(), searchSrv, store, func(d *FleetDeps) {
				if tt.template != "" {
					d.Worker.ReportTemplate = mustLoadTemplate(t, tt.template)
				}
			})

			id, err := f.Start(context.Background(), tt.task)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Start = %q, %v, want an error containing %q", id, err, tt.wantErr)
			}
			if tt.openErr != nil && !errors.Is(err, tt.openErr) {
				t.Errorf("Start error = %v, want it to wrap the store's error", err)
			}
			if id != "" {
				t.Errorf("Start returned id %q alongside its error", id)
			}
			if opened, _ := store.sessions(); len(opened) != tt.wantOpen {
				t.Errorf("sessions opened = %v, want %d", opened, tt.wantOpen)
			}
			f.mu.Lock()
			runs := len(f.runs)
			f.mu.Unlock()
			if runs != 0 || modelSrv.CallCount() != 0 {
				t.Errorf("runs registered = %d, model requests = %d, want neither", runs, modelSrv.CallCount())
			}
		})
	}
}

// TestFleetAwaitCancellationStopsEveryPaidCall: when Await's context ends
// mid-run, Await returns only once the run has unwound, so no model request
// follows it, and Result reports the failed run and the decomposition spend.
func TestFleetAwaitCancellationStopsEveryPaidCall(t *testing.T) {
	var inFlight atomic.Int32
	release := make(chan struct{})
	fm := &fleetModel{
		t:         t,
		decompose: decomposeReply(decompositionJSON(t, testBriefs(3))),
		worker: func(int, int) string {
			inFlight.Add(1)
			<-release
			return `{"action":"search","query":"late"}`
		},
	}
	modelSrv := httptest.NewServer(fm)
	defer modelSrv.Close()
	defer close(release)
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	store := newSessionStore()
	f := newTestFleet(t, modelSrv.URL, searchSrv, store, nil)

	id, err := f.Start(context.Background(), researcher.Task{Query: testQuery})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	awaitErr := make(chan error, 1)
	go func() { awaitErr <- f.Await(ctx, id) }()
	waitFor(t, "three workers in flight", func() bool { return inFlight.Load() == 3 })
	cancel()

	select {
	case err = <-awaitErr:
	case <-time.After(10 * time.Second):
		t.Fatal("Await did not return after its context was cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Await = %v, want the context error", err)
	}
	select {
	case <-runState(t, f, id).done:
	default:
		t.Error("Await returned before the run unwound")
	}
	atReturn := len(fm.requestsOf(kindDecompose)) + len(fm.requestsOf(kindWorker))
	time.Sleep(100 * time.Millisecond)
	after := len(fm.requestsOf(kindDecompose)) + len(fm.requestsOf(kindWorker)) +
		len(fm.requestsOf(kindSynthesise)) + len(fm.requestsOf(kindCite))
	if atReturn != 4 || after != atReturn {
		t.Errorf("model requests: %d at Await's return, %d after a grace; want 4 (decompose and three worker turns) and no more", atReturn, after)
	}
	if n := poolGoroutines(); n != 0 {
		t.Errorf("pool goroutines after Await = %d, want 0", n)
	}
	if _, closed := store.sessions(); len(closed) != 1 {
		t.Errorf("sessions closed = %v, want the run's", closed)
	}

	in, err := f.Result(context.Background(), id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Status != types.StatusFailed || !strings.Contains(in.StatusDetail, "no worker produced a finding with text") ||
		!strings.Contains(in.StatusDetail, "context ended") {
		t.Errorf("Result = %s (%s), want failed with the cancelled workers named", in.Status, in.StatusDetail)
	}
	if in.Usage.InputTokens != decomposeUsage.InputTokens || in.Usage.OutputTokens != decomposeUsage.OutputTokens {
		t.Errorf("usage = %+v, want the decomposition's %+v", in.Usage, decomposeUsage)
	}
}

// TestFleetAwaitGraceBoundsTheUnwind: a store that stalls past the run's
// cancellation cannot hold Await beyond its grace; Result then reports the
// run still in progress. Once the store answers, the cancelled run
// dispatches no worker and concludes.
func TestFleetAwaitGraceBoundsTheUnwind(t *testing.T) {
	fm := &fleetModel{
		t:         t,
		decompose: decomposeReply(decompositionJSON(t, testBriefs(3))),
		worker: func(int, int) string {
			t.Error("a worker ran after the run was cancelled")
			return ""
		},
	}
	modelSrv := httptest.NewServer(fm)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	store := newSessionStore()
	store.putGate = make(chan struct{})
	f := newTestFleet(t, modelSrv.URL, searchSrv, store, func(d *FleetDeps) {
		d.unwindGraceOverride = 50 * time.Millisecond
	})

	id, err := f.Start(context.Background(), researcher.Task{Query: testQuery})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the decomposition", func() bool { return len(fm.requestsOf(kindDecompose)) == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	begun := time.Now()
	if err := f.Await(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Await = %v, want the context error", err)
	}
	if waited := time.Since(begun); waited > 5*time.Second {
		t.Errorf("Await took %v, want it bounded by the grace", waited)
	}
	if in, _ := f.Result(context.Background(), id); in.Status != types.StatusInProgress {
		t.Errorf("Result before the run unwound = %s, want in_progress", in.Status)
	}

	close(store.putGate)
	if err := f.Await(context.Background(), id); err != nil {
		t.Fatalf("Await after the store answered: %v", err)
	}
	if in, _ := f.Result(context.Background(), id); in.Status != types.StatusFailed {
		t.Errorf("Result after the unwind = %s (%s), want failed", in.Status, in.StatusDetail)
	}
	if n := len(fm.requestsOf(kindWorker)) + len(fm.requestsOf(kindSynthesise)) + len(fm.requestsOf(kindCite)); n != 0 {
		t.Errorf("model requests after the decomposition = %d, want 0", n)
	}
}

// TestFleetAwaitReturnsNilOnceConcluded: a context that ends after the lead
// has concluded, while the session close is still running, does not turn a
// paid, complete run into an error.
func TestFleetAwaitReturnsNilOnceConcluded(t *testing.T) {
	modelSrv := httptest.NewServer(syntheticFleetModel(t))
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(syntheticSearchResults)
	defer searchSrv.Close()
	store := newSessionStore()
	store.closeGate = make(chan struct{})
	f := newTestFleet(t, modelSrv.URL, searchSrv, store, nil)

	id, err := f.Start(context.Background(), researcher.Task{Query: testQuery})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	awaitErr := make(chan error, 1)
	go func() { awaitErr <- f.Await(ctx, id) }()
	waitFor(t, "the lead to conclude", func() bool {
		in, _ := f.Result(context.Background(), id)
		return in.Status != types.StatusInProgress
	})
	cancel()
	select {
	case err := <-awaitErr:
		if err != nil {
			t.Errorf("Await = %v, want nil once the run has concluded", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Await did not return")
	}
	close(store.closeGate)
	if err := f.Await(context.Background(), id); err != nil {
		t.Errorf("Await after the session closed: %v", err)
	}
	if in, _ := f.Result(context.Background(), id); in.Status != types.StatusCompleted {
		t.Errorf("Result = %s (%s), want completed", in.Status, in.StatusDetail)
	}
}

// TestFleetDecomposeFailureFailsTheRun: a refused decomposition dispatches
// no worker; the run is Failed with a bounded detail and the refused call's
// usage, priced, and its session is still closed.
func TestFleetDecomposeFailureFailsTheRun(t *testing.T) {
	fm := &fleetModel{
		t:         t,
		decompose: decomposeReply(decompositionJSON(t, testBriefs(2))),
		worker: func(int, int) string {
			t.Error("a worker ran without a plan")
			return ""
		},
	}
	modelSrv := httptest.NewServer(fm)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	store := newSessionStore()
	f := newTestFleet(t, modelSrv.URL, searchSrv, store, func(d *FleetDeps) {
		d.Worker.Caps.InputGBPPerMTok, d.Worker.Caps.OutputGBPPerMTok = 2, 10
	})

	ctx := context.Background()
	id, err := f.Start(ctx, researcher.Task{Query: testQuery})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := f.Await(ctx, id); err != nil {
		t.Fatalf("Await: %v", err)
	}
	in, err := f.Result(ctx, id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Status != types.StatusFailed || !strings.Contains(in.StatusDetail, "the reply has 2 briefs") ||
		len(in.StatusDetail) > maxDetailBytes || len(in.Outputs) != 0 || len(in.Citations) != 0 {
		t.Errorf("Result = %+v, want failed on the decomposition with no report", in)
	}
	wantCost := costGBP(decomposeUsage.InputTokens, decomposeUsage.OutputTokens, 2, 10)
	if in.Usage.InputTokens != decomposeUsage.InputTokens || in.Usage.OutputTokens != decomposeUsage.OutputTokens ||
		math.Abs(in.Usage.EstimatedCostGBP-wantCost) > 1e-12 {
		t.Errorf("usage = %+v, want the decomposition's, priced at %v", in.Usage, wantCost)
	}
	if searchSrv.CallCount() != 0 {
		t.Errorf("search requests = %d, want 0", searchSrv.CallCount())
	}
	if _, closed := store.sessions(); len(closed) != 1 {
		t.Errorf("sessions closed = %v, want the run's", closed)
	}
}

// TestFleetProgressIsHeldUntilAwait: every worker's progress is held while
// no Await has been entered for the run, then delivered stamped with its
// worker id.
func TestFleetProgressIsHeldUntilAwait(t *testing.T) {
	fm := syntheticFleetModel(t)
	modelSrv := httptest.NewServer(fm)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(syntheticSearchResults)
	defer searchSrv.Close()
	var log progressLog
	f := newTestFleet(t, modelSrv.URL, searchSrv, newSessionStore(), func(d *FleetDeps) {
		d.Worker.Progress = log.hook
		d.Worker.progressTimeoutOverride = time.Minute
	})

	id, err := f.Start(context.Background(), researcher.Task{Query: testQuery})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "each worker's first turn", func() bool { return len(fm.requestsOf(kindWorker)) == 3 })
	time.Sleep(50 * time.Millisecond)
	if got := log.snapshot(); len(got) != 0 {
		t.Fatalf("deliveries before Await = %+v, want none", got)
	}
	if n := len(fm.requestsOf(kindWorker)); n != 3 {
		t.Errorf("worker requests before Await = %d, want each worker held at its first report", n)
	}

	if err := f.Await(context.Background(), id); err != nil {
		t.Fatalf("Await: %v", err)
	}
	var got []string
	for _, pr := range log.snapshot() {
		got = append(got, pr.WorkerID+" "+pr.Action)
	}
	slices.Sort(got)
	want := []string{
		"worker-1 final", "worker-1 search",
		"worker-2 final", "worker-2 search",
		"worker-3 final", "worker-3 search",
	}
	if !slices.Equal(got, want) {
		t.Errorf("deliveries = %v, want each worker's search and final", got)
	}
}

// TestFleetHasNoWriteSurface: the tool list names only the read-only tools,
// with knowledge_recall only when a knowledge store is configured and never
// knowledge_remember; a Fleet refuses a Rememberer outright, and a run's
// pool hands its workers neither the Rememberer nor the template.
func TestFleetHasNoWriteSurface(t *testing.T) {
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	recall := recallFunc(func(context.Context, memory.Namespace, memory.Query) ([]memory.Recalled, error) { return nil, nil })
	remember := rememberFunc(func(context.Context, memory.Namespace, memory.Memory) (memory.Reference, error) {
		t.Error("a fleet saved to the knowledge store")
		return memory.Reference{}, nil
	})

	withRemember := FleetDeps{
		Worker:      poolWorkerDeps(t, newModelClientAt(t, modelSrv.URL()), searchSrv),
		Store:       newSessionStore(),
		MaxWorkers:  3,
		Concurrency: 3,
	}
	withRemember.Worker.Remember = remember
	if _, err := NewFleet(withRemember); err == nil || !strings.Contains(err.Error(), "Remember must be nil") {
		t.Errorf("NewFleet with a Rememberer = %v, want it refused", err)
	}

	for _, tt := range []struct {
		name      string
		knowledge memory.Recaller
		want      []string
	}{
		{"web only", nil, []string{"web_search", "web_fetch"}},
		{"with recall", recall, []string{"web_search", "web_fetch", "knowledge_recall"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestFleet(t, modelSrv.URL(), searchSrv, newSessionStore(), func(d *FleetDeps) {
				d.Worker.Knowledge = tt.knowledge
				d.Worker.ReportTemplate = mustLoadTemplate(t, "Sections.")
			})
			if got := fleetTools(f.deps.Worker); !slices.Equal(got, tt.want) {
				t.Errorf("tools = %v, want %v", got, tt.want)
			}
			_, p, err := f.components("session/test")
			if err != nil {
				t.Fatalf("components: %v", err)
			}
			if p.deps.Worker.Remember != nil || p.deps.Worker.ReportTemplate != nil {
				t.Error("a fleet worker must receive neither the Rememberer nor the report template")
			}
		})
	}
}

// TestNewFleetValidates: a store, the worker collaborators and positive
// fan-out caps within each other are required at construction.
func TestNewFleetValidates(t *testing.T) {
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	good := func() FleetDeps {
		return FleetDeps{
			Worker:      poolWorkerDeps(t, newModelClientAt(t, modelSrv.URL()), searchSrv),
			Store:       newSessionStore(),
			MaxWorkers:  3,
			Concurrency: 2,
		}
	}
	if _, err := NewFleet(good()); err != nil {
		t.Fatalf("NewFleet(valid) = %v", err)
	}
	for _, tt := range []struct {
		name    string
		edit    func(*FleetDeps)
		wantErr string
	}{
		{"no store", func(d *FleetDeps) { d.Store = nil }, "context store"},
		{"no model", func(d *FleetDeps) { d.Worker.Model = nil }, "model client"},
		{"no search", func(d *FleetDeps) { d.Worker.Search = nil }, "search client"},
		{"no turn cap", func(d *FleetDeps) { d.Worker.Caps.MaxTurns = 0 }, "turn cap"},
		{"no worker cap", func(d *FleetDeps) { d.MaxWorkers = 0 }, "worker cap"},
		{"no concurrency", func(d *FleetDeps) { d.Concurrency = 0 }, "concurrency"},
		{"concurrency past the cap", func(d *FleetDeps) { d.Concurrency = 4 }, "exceeds its worker cap"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			deps := good()
			tt.edit(&deps)
			if f, err := NewFleet(deps); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("NewFleet = %v, %v, want an error containing %q", f, err, tt.wantErr)
			}
		})
	}
}

// TestFleetUnknownIDRejected: Await and Result on an id this Fleet never
// issued are programming faults reported as errors.
func TestFleetUnknownIDRejected(t *testing.T) {
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	f := newTestFleet(t, modelSrv.URL(), searchSrv, newSessionStore(), nil)

	if err := f.Await(context.Background(), "flt_not_issued"); err == nil || !strings.Contains(err.Error(), "no fleet run") {
		t.Errorf("Await(unknown) = %v, want a no-fleet-run error", err)
	}
	if in, err := f.Result(context.Background(), "flt_not_issued"); err == nil || in != nil {
		t.Errorf("Result(unknown) = %+v, %v, want nil and an error", in, err)
	}
}
