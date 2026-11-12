package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/types"
)

// newWorker builds a Worker over fakes, failing the test on construction
// error. The model fake is scripted by the caller; search returns the given
// results.
func newWorker(t *testing.T, modelSrv *modeltest.FakeServer, results []search.Result) *Worker {
	t.Helper()
	must := require.New(t)
	searchSrv := searchtest.NewFakeServer(results)
	t.Cleanup(searchSrv.Close)
	w, err := NewWorker(WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	})
	must.NoError(err, "NewWorker")
	return w
}

// TestWorker_Result_MapsFindingToInteraction drives the researcher seam: Start
// returns an opaque id immediately, Await blocks until the run finishes, and
// Result maps the Finding onto an Interaction with the worker's agent, tools,
// query, one text output, and the cited source.
func TestWorker_Result_MapsFindingToInteraction(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		finalReply("# Answer\n\nblue sky", "https://example.org/sky"),
	)
	defer modelSrv.Close()
	w := newWorker(t, modelSrv, []search.Result{{Title: "Sky", URL: "https://example.org/sky"}})

	ctx := context.Background()
	id, err := w.Start(ctx, researcher.Task{Query: "why is the sky blue"})
	must.NoError(err, "Start")
	check.True(strings.HasPrefix(id, "wkr_"), "id = %q, want a local wkr_ handle", id)

	must.NoError(w.Await(ctx, id), "Await")
	awaited := time.Now()
	in, err := w.Result(ctx, id)
	must.NoError(err, "Result")
	check.Equal(types.StatusCompleted, in.Status, "StatusDetail = %q", in.StatusDetail)
	check.False(in.CompletedAt.After(awaited), "CompletedAt = %v, want no later than %v: the loop's end, not Result's call time", in.CompletedAt, awaited)
	check.False(in.CompletedAt.Before(in.CreatedAt), "CompletedAt = %v, want no earlier than CreatedAt = %v", in.CompletedAt, in.CreatedAt)
	check.Equal("worker", in.Agent)
	check.Equal("why is the sky blue", in.Query)
	check.Equal([]string{"web_search", "web_fetch"}, in.Tools)
	must.Len(in.Outputs, 1, "outputs = %+v, want one text output with the answer", in.Outputs)
	check.Equal(types.OutputText, in.Outputs[0].Type)
	check.Contains(in.Outputs[0].Text, "blue sky")
	must.Len(in.Citations, 1, "citations = %+v, want the cited source", in.Citations)
	check.Equal("https://example.org/sky", in.Citations[0].URI)
	check.NotZero(in.CreatedAt, "CreatedAt must be set")
	check.NotZero(in.CompletedAt, "CompletedAt must be set")
}

// TestWorker_UnknownID_IsRejected: Await and Result on an id this Worker never
// issued are programming faults reported as errors, not panics or hangs.
func TestWorker_UnknownID_IsRejected(t *testing.T) {
	must := require.New(t)
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	w := newWorker(t, modelSrv, nil)

	ctx := context.Background()
	err := w.Await(ctx, "wkr_not_issued")
	must.ErrorContains(err, "no worker run")

	in, err := w.Result(ctx, "wkr_not_issued")
	must.Error(err, "Result(unknown) must fail")
	must.Nil(in, "Result(unknown) must return no interaction")
}

// TestWorker_Result_BeforeCompletionIsInProgress: Result on a live run reports
// in_progress with the run's identity rather than blocking or failing.
func TestWorker_Result_BeforeCompletionIsInProgress(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	release := make(chan struct{})
	blockingSrv := httptest.NewServer(blockingModel(release))
	defer blockingSrv.Close()

	mc, err := model.New(model.Options{Endpoint: blockingSrv.URL, Model: "m", APIKey: "k"})
	must.NoError(err, "model.New")
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	w, err := NewWorker(WorkerDeps{
		Model:  mc,
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	})
	must.NoError(err, "NewWorker")

	ctx := context.Background()
	id, err := w.Start(ctx, researcher.Task{Query: "q"})
	must.NoError(err, "Start")
	in, err := w.Result(ctx, id)
	must.NoError(err, "Result")
	check.Equal(types.StatusInProgress, in.Status)
	check.Equal(id, in.ID)
	check.Equal("q", in.Query)
	check.Equal("worker", in.Agent)
	check.Zero(in.CompletedAt, "an in-progress Result carries no completion time")
	check.Empty(in.Outputs, "an in-progress Result carries no outputs")

	close(release)
	must.NoError(w.Await(ctx, id), "Await")
	after, _ := w.Result(ctx, id)
	check.NotEqual(types.StatusInProgress, after.Status, "Result after Await = %+v", after)
}

// TestWorkerStartReturnsImmediately: Start must not block on the run (§3). A
// model fake that would block indefinitely on its turn must not stop Start
// from returning the resume handle.
func TestWorkerStartReturnsImmediately(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	release := make(chan struct{})
	// A raw server that hangs until released, so any synchronous await inside
	// Start would deadlock the test.
	blockingSrv := httptest.NewServer(blockingModel(release))
	defer blockingSrv.Close()
	defer close(release)

	mc, err := model.New(model.Options{Endpoint: blockingSrv.URL, Model: "m", APIKey: "k"})
	must.NoError(err, "model.New")
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	w, err := NewWorker(WorkerDeps{
		Model:  mc,
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	})
	must.NoError(err, "NewWorker")

	done := make(chan string, 1)
	go func() {
		id, startErr := w.Start(context.Background(), researcher.Task{Query: "q"})
		check.NoError(startErr, "Start")
		done <- id
	}()

	select {
	case id := <-done:
		must.NotEmpty(id, "Start returned an empty id")
	case <-time.After(2 * time.Second):
		must.Fail("Start did not return promptly — it must not block on the run")
	}
}

// TestWorkerAwaitRespectsContext: Await returns when its context is cancelled,
// even though the run is still going, rather than blocking forever.
func TestWorkerAwaitRespectsContext(t *testing.T) {
	must := require.New(t)
	release := make(chan struct{})
	blockingSrv := httptest.NewServer(blockingModel(release))
	defer blockingSrv.Close()
	defer close(release)

	mc, err := model.New(model.Options{Endpoint: blockingSrv.URL, Model: "m", APIKey: "k"})
	must.NoError(err, "model.New")
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	w, err := NewWorker(WorkerDeps{
		Model:  mc,
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   Caps{MaxTurns: 8, Timeout: 30 * time.Second},
	})
	must.NoError(err, "NewWorker")

	id, err := w.Start(context.Background(), researcher.Task{Query: "q"})
	must.NoError(err, "Start")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = w.Await(ctx, id)
	must.Error(err, "Await must return the context error when cancelled before the run finishes")
}

// TestWorker_Start_RejectsFollowUp: the in-process worker has no stored
// interaction chain, so a follow-up task is rejected at Start before any work.
func TestWorker_Start_RejectsFollowUp(t *testing.T) {
	must := require.New(t)
	modelSrv := modeltest.NewFakeServer(finalReply("x"))
	defer modelSrv.Close()
	w := newWorker(t, modelSrv, nil)

	_, err := w.Start(context.Background(), researcher.Task{Query: "q", PreviousInteractionID: "wkr_prev"})
	must.Error(err, "a follow-up must be rejected: the worker has no interaction chain")
}

// TestNewWorker_Validate_RejectsInvalidDeps: NewWorker fails when a required
// collaborator is missing, so a misconfigured worker never emits a resume
// handle for a run that cannot proceed.
func TestNewWorker_Validate_RejectsInvalidDeps(t *testing.T) {
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	good := WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	}
	for _, tt := range []struct {
		name   string
		mutate func(*WorkerDeps)
	}{
		{"nil model", func(d *WorkerDeps) { d.Model = nil }},
		{"nil search", func(d *WorkerDeps) { d.Search = nil }},
		{"nil fetch", func(d *WorkerDeps) { d.Fetch = nil }},
		{"zero turn cap", func(d *WorkerDeps) { d.Caps.MaxTurns = 0 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			d := good
			tt.mutate(&d)
			_, err := NewWorker(d)
			must.Error(err)
		})
	}
}

// TestWorkerScrubsModelCredential pins the credential-scrub guarantee
// (docs/V2-RESEARCH-AGENT §8): drive a failing model call whose 401 body echoes
// the resolved key (as a real provider 401 commonly does), then assert the key
// appears nowhere in the resulting Interaction — not the StatusDetail, not any
// output, not the citations.
func TestWorkerScrubsModelCredential(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// A high-entropy key-shaped literal that both the model client's exact
	// redaction and secret.Scrub's backstop will catch.
	const key = "sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0"
	// The fake echoes the key in its 401 body — the provider-echo path the
	// model adapter warns about.
	modelSrv := modeltest.NewFakeServer(modeltest.FakeReply{
		Status:     http.StatusUnauthorized,
		StatusBody: `{"error":{"message":"invalid api key ` + key + `"}}`,
	})
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()

	mc, err := model.New(model.Options{Endpoint: modelSrv.URL(), Model: "m", APIKey: key})
	must.NoError(err, "model.New")
	w, err := NewWorker(WorkerDeps{
		Model:  mc,
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	})
	must.NoError(err, "NewWorker")

	ctx := context.Background()
	id, err := w.Start(ctx, researcher.Task{Query: "q"})
	must.NoError(err, "Start")
	must.NoError(w.Await(ctx, id), "Await")
	in, err := w.Result(ctx, id)
	must.NoError(err, "Result")
	must.Equal(types.StatusFailed, in.Status)
	check.NotContains(in.StatusDetail, key, "the model key leaked into StatusDetail")
	for _, out := range in.Outputs {
		check.NotContains(out.Text, key, "the model key leaked into an output")
	}
}

// TestWorker_Scrub_SearchKeyNotLeaked is the search-path analogue: a search MCP
// that rejects the tools/call with a body echoing the key must not leak it
// into the failed Interaction's StatusDetail.
func TestWorker_Scrub_SearchKeyNotLeaked(t *testing.T) {
	must := require.New(t)
	const key = "sk-search-Z9y8X7w6V5u4T3s2R1q0P9o8N7m6L5k4J3i2H1g0"
	// A search MCP that answers initialize but rejects tools/call, echoing the
	// key in the error — the provider-echo path on the search side.
	searchSrv := httptest.NewServer(leakySearch(key))
	defer searchSrv.Close()

	sc, err := search.New(search.Options{Endpoint: searchSrv.URL, APIKey: key})
	must.NoError(err, "search.New")
	// The model searches on turn 1, which triggers the leaky tools/call.
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"x"}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()

	w, err := NewWorker(WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: sc,
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	})
	must.NoError(err, "NewWorker")

	ctx := context.Background()
	id, err := w.Start(ctx, researcher.Task{Query: "q"})
	must.NoError(err, "Start")
	must.NoError(w.Await(ctx, id), "Await")
	in, err := w.Result(ctx, id)
	must.NoError(err, "Result")
	must.Equal(types.StatusFailed, in.Status, "StatusDetail = %q", in.StatusDetail)
	must.NotContains(in.StatusDetail, key, "the search key leaked into StatusDetail")
}

// TestWorker_Run_RecoversPanic: a panic in the run's goroutine never reaches
// the process. Before the loop has recorded its Finding the run is Failed
// under the fixed detail, never the recovered value; during the save-back
// the recorded Finding stands. Await returns nil either way.
func TestWorker_Run_RecoversPanic(t *testing.T) {
	panickingRemember := rememberFunc(func(context.Context, memory.Namespace, memory.Memory) (memory.Reference, error) {
		panic("store failure for key " + panicValueKey)
	})
	for _, tt := range []struct {
		name       string
		tracer     trace.Tracer
		remember   memory.Rememberer
		replies    []modeltest.FakeReply
		wantStatus types.Status
		wantDetail string
		wantOutput bool
		wantCalls  int
	}{
		{
			name:       "before the finding",
			tracer:     panickingTracer{panicOn: trace.SpanWorker},
			wantStatus: types.StatusFailed,
			wantDetail: panicRecoveryDetail,
		},
		{
			name:       "during the save-back",
			remember:   panickingRemember,
			replies:    []modeltest.FakeReply{finalReply("answer")},
			wantStatus: types.StatusCompleted,
			wantOutput: true,
			wantCalls:  1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			modelSrv := modeltest.NewFakeServer(tt.replies...)
			defer modelSrv.Close()
			searchSrv := searchtest.NewFakeServer(nil)
			defer searchSrv.Close()
			w, err := NewWorker(WorkerDeps{
				Model:    newModelClient(t, modelSrv),
				Search:   newSearchClient(t, searchSrv),
				Fetch:    newFetchClient(t),
				Remember: tt.remember,
				Tracer:   tt.tracer,
				Caps:     caps(),
			})
			must.NoError(err, "NewWorker")

			ctx := context.Background()
			id, err := w.Start(ctx, researcher.Task{Query: "q"})
			must.NoError(err, "Start")
			must.NoError(w.Await(ctx, id), "Await")
			in, err := w.Result(ctx, id)
			must.NoError(err, "Result")
			must.Equal(tt.wantStatus, in.Status)
			must.Equal(tt.wantDetail, in.StatusDetail)
			must.Equal(tt.wantOutput, len(in.Outputs) == 1, "outputs = %+v, want the answer as its only output", in.Outputs)
			must.NotZero(in.CompletedAt, "CompletedAt should be the time the run recorded its Finding")
			must.Equal(tt.wantCalls, modelSrv.CallCount(), "model calls")
		})
	}
}
