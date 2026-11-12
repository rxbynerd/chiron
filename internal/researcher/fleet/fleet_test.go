package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/types"
)

// sessionStore is an InMemory store that records each session opened in it
// and each closed, with the number of Puts completed when each close began.
// It fails every OpenSession when openErr is set, holds each Close until
// closeGate is closed when that is set, and holds each Put until putGate is
// closed when that is set, ignoring the context either way.
type sessionStore struct {
	*memory.InMemory
	openErr   error
	closeGate chan struct{}
	putGate   chan struct{}

	mu          sync.Mutex
	opened      []string
	closed      []memory.Namespace
	puts        int
	putsAtClose []int
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
	ref, err := s.InMemory.Put(ctx, ns, body, meta)
	s.mu.Lock()
	s.puts++
	s.mu.Unlock()
	return ref, err
}

// putCountsAtClose returns the number of Puts completed when each session
// close began, in close order.
func (s *sessionStore) putCountsAtClose() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.putsAtClose)
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
	r.store.mu.Lock()
	r.store.putsAtClose = append(r.store.putsAtClose, r.store.puts)
	r.store.mu.Unlock()
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
	must := require.New(t)
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
	must.NoError(err, "NewFleet")
	return f
}

// runState returns the Fleet's record for id.
func runState(t *testing.T, f *Fleet, id string) *fleetState {
	t.Helper()
	must := require.New(t)
	st, err := f.state(id)
	must.NoError(err, "state")
	return st
}

// TestFleet_Start_ResultCarriesReportSourcesAndUsage: a fleet run through the
// Researcher seam opens and closes a session under its flt_ id, and Result
// carries the synthesised report, sources from both workers and one Usage with
// the lead's calls priced at the workers' rates.
func TestFleet_Start_ResultCarriesReportSourcesAndUsage(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

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
	must.NoError(err, "Start")
	check.True(IsFleetInteractionID(id) && !IsWorkerInteractionID(id) && len(id) == len(FleetInteractionIDPrefix)+32,
		"id = %q, want a local flt_ handle", id)
	opened, _ := store.sessions()
	check.Equal([]string{id}, opened, "sessions opened, want one under the interaction id")

	must.NoError(f.Await(ctx, id), "Await")
	awaited := time.Now()
	_, closed := store.sessions()
	check.Equal([]memory.Namespace{memory.Namespace("session/" + id)}, closed, "sessions closed, want the run's own")
	in, err := f.Result(ctx, id)
	must.NoError(err, "Result")

	check.Equal(types.StatusCompleted, in.Status, "status")
	check.Empty(in.StatusDetail, "status detail")
	check.Equal(id, in.ID, "interaction id")
	check.Equal("fleet", in.Agent, "agent")
	check.Equal(testQuery, in.Query, "query")
	check.Equal([]string{"web_search", "web_fetch"}, in.Tools, "tools")
	must.Len(in.Outputs, 1, "outputs")
	check.Equal(types.OutputText, in.Outputs[0].Type, "output type")
	check.Equal(syntheticReport, in.Outputs[0].Text, "report text")
	check.Equal([]types.Citation{
		{URI: heatPumpURL, Title: "Heat pump running costs"},
		{URI: boilerURL, Title: "Gas boiler running costs"},
	}, in.Citations, "citations, want one source from each worker")
	check.False(in.CreatedAt.IsZero(), "the run has a start time")
	check.False(in.CompletedAt.Before(in.CreatedAt), "the run concluded before it started")
	check.False(in.CompletedAt.After(awaited), "the run concluded after Await returned")

	// Three workers of two turns at 10 input and 5 output tokens each, one
	// search each, plus the decompose, synthesis and citation calls.
	const workerIn, workerOut = 3 * 2 * 10, 3 * 2 * 5
	leadIn := decomposeUsage.InputTokens + synthesisUsage.InputTokens + citeUsage.InputTokens
	leadOut := decomposeUsage.OutputTokens + synthesisUsage.OutputTokens + citeUsage.OutputTokens
	u := in.Usage
	check.Equal(workerIn+leadIn, u.InputTokens, "input tokens")
	check.Equal(workerOut+leadOut, u.OutputTokens, "output tokens")
	check.Equal(3, u.SearchCount, "searches, want one per worker")
	wantCost := costGBP(workerIn, workerOut, inPrice, outPrice) + costGBP(leadIn, leadOut, inPrice, outPrice)
	check.InDelta(wantCost, u.EstimatedCostGBP, 1e-12, "estimated cost, want the lead's calls priced at the workers' rates")
	check.Equal(3, searchSrv.ToolCallCount(), "search calls, want one per worker")
}

// TestFleet_Components_ShareRoutesAndPrices: a run's lead validates its plan
// against the same routing table its pool dispatches through, and prices
// its calls at the workers' Caps rates.
func TestFleet_Components_ShareRoutesAndPrices(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

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
	must.NoError(err, "components")
	fleetTable := reflect.ValueOf(f.routes).UnsafePointer()
	check.Equal(fleetTable, reflect.ValueOf(l.deps.Routes).UnsafePointer(), "the lead must share the Fleet's one routing table")
	check.Equal(fleetTable, reflect.ValueOf(p.deps.Routes).UnsafePointer(), "the pool must share the Fleet's one routing table")
	check.InDelta(1.25, l.deps.InputGBPPerMTok, 0, "lead input price, want the workers' 1.25")
	check.InDelta(7.5, l.deps.OutputGBPPerMTok, 0, "lead output price, want the workers' 7.5")
	check.Equal(f.deps.Worker.Caps, p.deps.Worker.Caps, "pool caps, want the Fleet's")
	check.Equal(memory.Namespace("session/test"), l.deps.Namespace, "lead namespace, want the run's session")
	check.Equal(memory.Namespace("session/test"), p.deps.Namespace, "pool namespace, want the run's session")
}

// TestFleet_Start_RefusalsRegisterNoRun: an empty query, a follow-up, a report
// template that fails to render for the query and a session the store
// refuses all fail Start with no id and no model request; only the last
// reaches the store.
func TestFleet_Start_RefusalsRegisterNoRun(t *testing.T) {
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
			must := require.New(t)

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
			must.ErrorContains(err, tt.wantErr, "Start")
			if tt.openErr != nil {
				must.ErrorIs(err, tt.openErr, "Start should wrap the store's error")
			}
			must.Empty(id, "Start returned an id alongside its error")
			opened, _ := store.sessions()
			must.Len(opened, tt.wantOpen, "sessions opened")
			f.mu.Lock()
			runs := len(f.runs)
			f.mu.Unlock()
			must.Zero(runs, "runs registered")
			must.Zero(modelSrv.CallCount(), "model requests")
		})
	}
}

// TestFleet_Await_CancellationStopsEveryPaidCall: when Await's context ends
// mid-run, Await returns only once the run has unwound, so no model request
// follows it, and Result reports the failed run and the decomposition spend.
func TestFleet_Await_CancellationStopsEveryPaidCall(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

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
	must.NoError(err, "Start")
	ctx, cancel := context.WithCancel(context.Background())
	awaitErr := make(chan error, 1)
	go func() { awaitErr <- f.Await(ctx, id) }()
	waitFor(t, "three workers in flight", func() bool { return inFlight.Load() == 3 })
	cancel()

	select {
	case err = <-awaitErr:
	case <-time.After(10 * time.Second):
		must.Fail("Await did not return after its context was cancelled")
	}
	must.ErrorIs(err, context.Canceled, "Await")
	select {
	case <-runState(t, f, id).done:
	default:
		check.Fail("Await returned before the run unwound")
	}
	atReturn := len(fm.requestsOf(kindDecompose)) + len(fm.requestsOf(kindWorker))
	time.Sleep(100 * time.Millisecond)
	after := len(fm.requestsOf(kindDecompose)) + len(fm.requestsOf(kindWorker)) +
		len(fm.requestsOf(kindSynthesise)) + len(fm.requestsOf(kindCite))
	check.Equal(4, atReturn, "model requests at Await's return, want the decomposition and three worker turns")
	check.Equal(atReturn, after, "model requests after a grace, want no more than at Await's return")
	check.Zero(poolGoroutines(), "pool goroutines after Await")
	_, closed := store.sessions()
	check.Len(closed, 1, "sessions closed, want the run's")

	in, err := f.Result(context.Background(), id)
	must.NoError(err, "Result")
	check.Equal(types.StatusFailed, in.Status, "status")
	check.Contains(in.StatusDetail, "no worker produced a finding with text", "status detail, want the workers' outcome")
	check.Contains(in.StatusDetail, "context ended", "status detail, want the cancellation named")
	check.Equal(decomposeUsage.InputTokens, in.Usage.InputTokens, "input tokens, want the decomposition's")
	check.Equal(decomposeUsage.OutputTokens, in.Usage.OutputTokens, "output tokens, want the decomposition's")
}

// TestFleet_Await_GraceBoundsTheUnwind: a store that stalls past the run's
// cancellation cannot hold Await beyond its grace; Result then reports the
// run still in progress, and no spend is logged because none was recorded.
// Once the store answers, the cancelled run dispatches no worker and
// concludes.
func TestFleet_Await_GraceBoundsTheUnwind(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fm := &fleetModel{
		t:         t,
		decompose: decomposeReply(decompositionJSON(t, testBriefs(3))),
		worker: func(int, int) string {
			check.Fail("a worker ran after the run was cancelled")
			return ""
		},
	}
	modelSrv := httptest.NewServer(fm)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	store := newSessionStore()
	store.putGate = make(chan struct{})
	var logs bytes.Buffer
	f := newTestFleet(t, modelSrv.URL, searchSrv, store, func(d *FleetDeps) {
		d.unwindGraceOverride = 50 * time.Millisecond
		d.Worker.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	})

	id, err := f.Start(context.Background(), researcher.Task{Query: testQuery})
	must.NoError(err, "Start")
	waitFor(t, "the decomposition", func() bool { return len(fm.requestsOf(kindDecompose)) == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	begun := time.Now()
	must.ErrorIs(f.Await(ctx, id), context.DeadlineExceeded, "Await")
	check.LessOrEqual(time.Since(begun), 5*time.Second, "Await's wait, want it bounded by the grace")
	in, _ := f.Result(context.Background(), id)
	check.Equal(types.StatusInProgress, in.Status, "Result before the run unwound")

	close(store.putGate)
	must.NoError(f.Await(context.Background(), id), "Await after the store answered")
	in, _ = f.Result(context.Background(), id)
	check.Equal(types.StatusFailed, in.Status, "Result after the unwind")
	requests := len(fm.requestsOf(kindWorker)) + len(fm.requestsOf(kindSynthesise)) + len(fm.requestsOf(kindCite))
	check.Zero(requests, "model requests after the decomposition")
	check.Empty(logs.String(), "Await logged with no outcome recorded when its context ended")
}

// TestFleet_Await_ReturnsNilOnceConcluded: a context that ends after the lead
// has concluded, while the session close is still running, does not turn a
// paid, complete run into an error, and the run's spend is logged once
// because its report can no longer be emitted.
func TestFleet_Await_ReturnsNilOnceConcluded(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	modelSrv := httptest.NewServer(syntheticFleetModel(t))
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(syntheticSearchResults)
	defer searchSrv.Close()
	store := newSessionStore()
	store.closeGate = make(chan struct{})
	var logs bytes.Buffer
	f := newTestFleet(t, modelSrv.URL, searchSrv, store, func(d *FleetDeps) {
		d.Worker.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	})

	id, err := f.Start(context.Background(), researcher.Task{Query: testQuery})
	must.NoError(err, "Start")
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
		must.NoError(err, "Await once the run has concluded")
	case <-time.After(10 * time.Second):
		must.Fail("Await did not return")
	}
	close(store.closeGate)
	must.NoError(f.Await(context.Background(), id), "Await after the session closed")
	in, _ := f.Result(context.Background(), id)
	check.Equal(types.StatusCompleted, in.Status, "status, want completed (status detail %q)", in.StatusDetail)
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	check.Len(lines, 1, "log = %q, want one spend record for the completed run", logs.String())
	check.Contains(lines[0], `"interaction_id":"`+id+`"`, "spend record names the run")
	check.Contains(lines[0], `"status":"completed"`, "spend record reports completion")
}

// TestFleet_Await_LogsSpendWhenContextEnds: when Await's context ends while
// the synthesis call is in flight, after every worker finished, Await
// cancels the run, waits for it to conclude on the stitched findings and
// returns the context error. It logs the run's rolled-up spend once, at
// Warn, with no report or finding text, and no model request follows it.
func TestFleet_Await_LogsSpendWhenContextEnds(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fm := syntheticFleetModel(t)
	var synthesisCalls atomic.Int32
	synthesisHeld := make(chan struct{})
	modelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			check.NoError(err, "read model request")
			return
		}
		if !bytes.Contains(body, []byte(`"response_format"`)) {
			if synthesisCalls.Add(1) == 1 {
				close(synthesisHeld)
			}
			<-r.Context().Done()
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fm.ServeHTTP(w, r)
	}))
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(syntheticSearchResults)
	defer searchSrv.Close()
	var logs bytes.Buffer
	f := newTestFleet(t, modelSrv.URL, searchSrv, newSessionStore(), func(d *FleetDeps) {
		d.Worker.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
		d.Worker.Caps.InputGBPPerMTok, d.Worker.Caps.OutputGBPPerMTok = 2, 10
	})

	id, err := f.Start(context.Background(), researcher.Task{Query: testQuery})
	must.NoError(err, "Start")
	ctx, cancel := context.WithCancel(context.Background())
	awaitErr := make(chan error, 1)
	go func() { awaitErr <- f.Await(ctx, id) }()
	select {
	case <-synthesisHeld:
	case <-time.After(10 * time.Second):
		must.Fail("the synthesis call never arrived")
	}
	cancel()
	select {
	case err = <-awaitErr:
	case <-time.After(10 * time.Second):
		must.Fail("Await did not return after its context was cancelled")
	}
	must.ErrorIs(err, context.Canceled, "Await")
	select {
	case <-runState(t, f, id).done:
	default:
		check.Fail("Await returned before the run unwound, so a paid call could outlive it")
	}
	check.Len(fm.requestsOf(kindWorker), 6, "worker requests, want every worker's two turns before synthesis")
	check.EqualValues(1, synthesisCalls.Load(), "synthesis requests")
	check.Empty(fm.requestsOf(kindCite), "citation requests")
	logged := logs.String()
	must.NoError(f.Await(ctx, id), "a second Await on the concluded run")
	check.Equal(logged, logs.String(), "a second Await logged again")

	in, err := f.Result(context.Background(), id)
	must.NoError(err, "Result")
	wantIn, wantOut := 3*2*10+decomposeUsage.InputTokens, 3*2*5+decomposeUsage.OutputTokens
	must.Equal(types.StatusIncomplete, in.Status, "status")
	must.Equal(wantIn, in.Usage.InputTokens, "input tokens")
	must.Equal(wantOut, in.Usage.OutputTokens, "output tokens")
	must.Equal(3, in.Usage.SearchCount, "searches")

	must.NotEmpty(logged, "no spend record was logged when Await returned")
	lines := strings.Split(strings.TrimSpace(logged), "\n")
	must.Len(lines, 1, "spend records logged when Await returned:\n%s", logged)
	var rec struct {
		Level         string  `json:"level"`
		InteractionID string  `json:"interaction_id"`
		Status        string  `json:"status"`
		InputTokens   int     `json:"input_tokens"`
		OutputTokens  int     `json:"output_tokens"`
		SearchCount   int     `json:"search_count"`
		CostGBP       float64 `json:"estimated_cost_gbp"`
	}
	err = json.Unmarshal([]byte(lines[0]), &rec)
	must.NoError(err, "decode log record:\n%s", lines[0])
	check.Equal("WARN", rec.Level, "log level")
	check.Equal(id, rec.InteractionID, "log interaction id")
	check.Equal(string(types.StatusIncomplete), rec.Status, "log status")
	check.Equal(wantIn, rec.InputTokens, "logged input tokens")
	check.Equal(wantOut, rec.OutputTokens, "logged output tokens")
	check.Equal(3, rec.SearchCount, "logged searches")
	check.InDelta(in.Usage.EstimatedCostGBP, rec.CostGBP, 1e-12, "logged cost, want the run's rollup")
	check.NotZero(rec.CostGBP, "logged cost")

	for _, tt := range []string{"three units of heat", "nine-tenths of its fuel", "Grants cover part"} {
		t.Run("log omits "+tt, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(logs.String(), tt)
		})
	}
}

// TestFleet_Decompose_FailureFailsRun: a refused decomposition dispatches
// no worker; the run is Failed with a bounded detail and the refused call's
// usage, priced, and its session is still closed.
func TestFleet_Decompose_FailureFailsRun(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fm := &fleetModel{
		t:         t,
		decompose: decomposeReply(decompositionJSON(t, testBriefs(2))),
		worker: func(int, int) string {
			check.Fail("a worker ran without a plan")
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
	must.NoError(err, "Start")
	must.NoError(f.Await(ctx, id), "Await")
	in, err := f.Result(ctx, id)
	must.NoError(err, "Result")

	check.Equal(types.StatusFailed, in.Status, "status")
	check.Contains(in.StatusDetail, "the reply has 2 briefs", "status detail")
	check.LessOrEqual(len(in.StatusDetail), maxDetailBytes, "status detail length, want it bounded")
	check.Empty(in.Outputs, "outputs")
	check.Empty(in.Citations, "citations")
	wantCost := costGBP(decomposeUsage.InputTokens, decomposeUsage.OutputTokens, 2, 10)
	check.Equal(decomposeUsage.InputTokens, in.Usage.InputTokens, "input tokens, want the decomposition's")
	check.Equal(decomposeUsage.OutputTokens, in.Usage.OutputTokens, "output tokens, want the decomposition's")
	check.InDelta(wantCost, in.Usage.EstimatedCostGBP, 1e-12, "estimated cost, want the decomposition priced at the Caps rates")
	check.Zero(searchSrv.CallCount(), "search requests")
	_, closed := store.sessions()
	check.Len(closed, 1, "sessions closed, want the run's")
}

// skipPanickingTracer's spans panic when the pool records why it skipped a
// brief, standing in for a fault in the pool's dispatch loop rather than in
// a worker.
type skipPanickingTracer struct{ trace.Noop }

func (p skipPanickingTracer) StartSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	ctx, span := p.Noop.StartSpan(ctx, name)
	return ctx, skipPanickingSpan{span}
}

type skipPanickingSpan struct{ trace.Span }

func (s skipPanickingSpan) SetAttr(key string, value any) {
	if key == "reason" {
		panic("tracer failure for key " + panicValueKey)
	}
	s.Span.SetAttr(key, value)
}

// TestFleet_Await_RecoversLeadPanic: a panic while the lead decomposes, or while
// its pool dispatches with a worker already running, never reaches the
// process. The run is Failed under the fixed lead panic detail, never the
// recovered value, with the decomposition's usage when that call returned.
// A worker already running is cancelled and has stored its finding before
// the session closes, and Await returns nil once the session is closed.
func TestFleet_Await_RecoversLeadPanic(t *testing.T) {
	for _, tt := range []struct {
		name       string
		tracer     trace.Tracer
		decomposed bool
		// wantPuts counts the plan and each stored finding.
		wantPuts int
	}{
		{name: "decompose", tracer: panickingTracer{panicOn: trace.SpanDecompose}},
		{name: "pool dispatch", tracer: skipPanickingTracer{}, decomposed: true, wantPuts: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			release := make(chan struct{})
			fm := &fleetModel{
				t:         t,
				decompose: decomposeReply(decompositionJSON(t, testBriefs(3))),
				worker: func(int, int) string {
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
			f := newTestFleet(t, modelSrv.URL, searchSrv, store, func(d *FleetDeps) {
				d.Worker.Tracer = tt.tracer
				d.MaxWorkers, d.Concurrency = 1, 1
			})

			ctx := context.Background()
			id, err := f.Start(ctx, researcher.Task{Query: testQuery})
			must.NoError(err, "Start")
			must.NoError(f.Await(ctx, id), "Await")

			_, closed := store.sessions()
			must.Len(closed, 1, "sessions closed, want the run's")
			// The started worker's model call is held until the test ends,
			// so its finding is stored before the close only if the panic
			// cancelled it and the pool waited it out.
			must.Equal([]int{tt.wantPuts}, store.putCountsAtClose(), "puts when the session closed")

			in, err := f.Result(ctx, id)
			must.NoError(err, "Result")
			must.Equal(types.StatusFailed, in.Status, "status")
			must.Equal(leadPanicDetail, in.StatusDetail, "status detail")
			must.Empty(in.Outputs, "outputs")
			must.Empty(in.Citations, "citations")

			wantIn, wantOut, wantDecompose := 0, 0, 0
			if tt.decomposed {
				wantIn, wantOut, wantDecompose = decomposeUsage.InputTokens, decomposeUsage.OutputTokens, 1
			}
			must.Equal(wantIn, in.Usage.InputTokens, "input tokens")
			must.Equal(wantOut, in.Usage.OutputTokens, "output tokens")
			must.Len(fm.requestsOf(kindDecompose), wantDecompose, "decompose requests")
			must.LessOrEqual(len(fm.requestsOf(kindWorker)), 1, "worker requests, want at most the one started worker's first turn")
			must.Empty(fm.requestsOf(kindSynthesise), "synthesis requests")
			must.Empty(fm.requestsOf(kindCite), "citation requests")
		})
	}
}

// TestFleet_Progress_HeldUntilAwait: every worker's progress is held while
// no Await has been entered for the run, then delivered stamped with its
// worker id.
func TestFleet_Progress_HeldUntilAwait(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

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
	must.NoError(err, "Start")
	waitFor(t, "each worker's first turn", func() bool { return len(fm.requestsOf(kindWorker)) == 3 })
	time.Sleep(50 * time.Millisecond)
	must.Empty(log.snapshot(), "deliveries before Await")
	check.Len(fm.requestsOf(kindWorker), 3, "worker requests before Await, want each worker held at its first report")

	must.NoError(f.Await(context.Background(), id), "Await")
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
	check.Equal(want, got, "deliveries, want each worker's search and final")
}

// TestFleet_Tools_HasNoWriteSurface: the tool list names only the read-only tools,
// with knowledge_recall only when a knowledge store is configured and never
// knowledge_remember; a Fleet refuses a Rememberer outright, and a run's
// pool hands its workers neither the Rememberer nor the template.
func TestFleet_Tools_HasNoWriteSurface(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	recall := recallFunc(func(context.Context, memory.Namespace, memory.Query) ([]memory.Recalled, error) { return nil, nil })
	remember := rememberFunc(func(context.Context, memory.Namespace, memory.Memory) (memory.Reference, error) {
		check.Fail("a fleet saved to the knowledge store")
		return memory.Reference{}, nil
	})

	withRemember := FleetDeps{
		Worker:      poolWorkerDeps(t, newModelClientAt(t, modelSrv.URL()), searchSrv),
		Store:       newSessionStore(),
		MaxWorkers:  3,
		Concurrency: 3,
	}
	withRemember.Worker.Remember = remember
	_, err := NewFleet(withRemember)
	must.ErrorContains(err, "Remember must be nil", "NewFleet with a Rememberer")

	for _, tt := range []struct {
		name      string
		knowledge memory.Recaller
		want      []string
	}{
		{"web only", nil, []string{"web_search", "web_fetch"}},
		{"with recall", recall, []string{"web_search", "web_fetch", "knowledge_recall"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			f := newTestFleet(t, modelSrv.URL(), searchSrv, newSessionStore(), func(d *FleetDeps) {
				d.Worker.Knowledge = tt.knowledge
				d.Worker.ReportTemplate = mustLoadTemplate(t, "Sections.")
			})
			must.Equal(tt.want, fleetTools(f.deps.Worker), "tools")

			_, p, err := f.components("session/test")
			must.NoError(err, "components")
			must.Nil(p.deps.Worker.Remember, "a fleet worker must receive no Rememberer")
			must.Nil(p.deps.Worker.ReportTemplate, "a fleet worker must receive no report template")
		})
	}
}

// TestNewFleet_Validate_RejectsBadDeps: a store, the worker collaborators and
// positive fan-out caps within each other are required at construction.
func TestNewFleet_Validate_RejectsBadDeps(t *testing.T) {
	must := require.New(t)

	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	good := func(t *testing.T) FleetDeps {
		return FleetDeps{
			Worker:      poolWorkerDeps(t, newModelClientAt(t, modelSrv.URL()), searchSrv),
			Store:       newSessionStore(),
			MaxWorkers:  3,
			Concurrency: 2,
		}
	}
	_, err := NewFleet(good(t))
	must.NoError(err, "NewFleet(valid)")

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
			must := require.New(t)

			deps := good(t)
			tt.edit(&deps)
			_, err := NewFleet(deps)
			must.ErrorContains(err, tt.wantErr, "NewFleet")
		})
	}
}

// TestFleet_Lookup_RejectsUnknownID: Await and Result on an id this Fleet never
// issued are programming faults reported as errors.
func TestFleet_Lookup_RejectsUnknownID(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	f := newTestFleet(t, modelSrv.URL(), searchSrv, newSessionStore(), nil)

	must.ErrorContains(f.Await(context.Background(), "flt_not_issued"), "no fleet run", "Await(unknown)")
	in, err := f.Result(context.Background(), "flt_not_issued")
	must.Error(err, "Result(unknown) should return an error")
	check.Nil(in, "Result(unknown) should return no interaction")
}
