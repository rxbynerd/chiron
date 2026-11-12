package fleet

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/types"
)

// TestRunWorkerTokenCapStops: accumulated tokens at or over MaxTokens stop
// the loop Incomplete before the next paid turn.
func TestRunWorkerTokenCapStops(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	var replies []modeltest.FakeReply
	for i := 0; i < 5; i++ {
		replies = append(replies, modeltest.FakeReply{
			Content:      `{"action":"search","query":"again"}`,
			FinishReason: "stop",
			Usage:        model.Usage{InputTokens: 40, OutputTokens: 20, TotalTokens: 60},
		})
	}
	modelSrv := modeltest.NewFakeServer(replies...)
	defer modelSrv.Close()

	c := caps()
	c.MaxTokens = 100
	deps := WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   c,
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})

	must.Equal(types.StatusIncomplete, finding.Status, "detail: %s", finding.Detail)
	check.Contains(finding.Detail, "100-token cap")
	// 60 tokens after turn 1 is under the cap; 120 after turn 2 is over it.
	check.Equal(2, modelSrv.CallCount())
	check.Equal(80, finding.Usage.InputTokens)
	check.Equal(40, finding.Usage.OutputTokens)
}

// TestRunWorker_CostCeilingStops: with a price configured the estimated cost
// accumulates and the GBP ceiling stops the loop Incomplete.
func TestRunWorker_CostCeilingStops(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	var replies []modeltest.FakeReply
	for i := 0; i < 5; i++ {
		replies = append(replies, modeltest.FakeReply{
			Content:      `{"action":"search","query":"again"}`,
			FinishReason: "stop",
			Usage:        model.Usage{InputTokens: 1_000_000, OutputTokens: 0, TotalTokens: 1_000_000},
		})
	}
	modelSrv := modeltest.NewFakeServer(replies...)
	defer modelSrv.Close()

	c := caps()
	c.InputGBPPerMTok = 2
	c.OutputGBPPerMTok = 8
	c.CeilingGBP = 3
	deps := WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   c,
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})

	must.Equal(types.StatusIncomplete, finding.Status, "detail: %s", finding.Detail)
	check.Contains(finding.Detail, "£3.00 cost ceiling")
	// £2 after turn 1 is under the ceiling; £4 after turn 2 is over it.
	check.Equal(2, modelSrv.CallCount())
	// InDelta with zero delta is an exact comparison; the spend is exact in binary floating point.
	check.InDelta(4.0, finding.Usage.EstimatedCostGBP, 0)
}

// TestRunWorker_TimeoutMidTurnIsIncomplete: the per-worker timeout firing while
// a model call is in flight ends the run Incomplete, not Failed.
func TestRunWorker_TimeoutMidTurnIsIncomplete(t *testing.T) {
	must := require.New(t)

	release := make(chan struct{})
	blockingModel := httptest.NewServer(blockingModel(release))
	defer blockingModel.Close()
	defer close(release)

	mc, err := model.New(model.Options{Endpoint: blockingModel.URL, Model: "m", APIKey: "k"})
	must.NoError(err, "model.New")
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()

	c := caps()
	c.Timeout = 100 * time.Millisecond
	deps := WorkerDeps{
		Model:  mc,
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   c,
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})

	must.Equal(types.StatusIncomplete, finding.Status, "detail: %s", finding.Detail)
	must.Contains(finding.Detail, "context ended")
}

// TestRunWorker_LengthFinishIsIncomplete: a reply cut off at the completion cap
// is a bounded stop, not an invalid action.
func TestRunWorker_LengthFinishIsIncomplete(t *testing.T) {
	must := require.New(t)

	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"final","answer":"a very long ans`, FinishReason: "length"},
	)
	defer modelSrv.Close()

	deps := WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})

	must.Equal(types.StatusIncomplete, finding.Status, "detail: %s", finding.Detail)
	must.Contains(finding.Detail, "cut off")
}

// TestWorker_Await_CancelStopsRun: when Await's context ends, the run is
// cancelled so no further paid turns are taken, and the run concludes
// Incomplete.
func TestWorker_Await_CancelStopsRun(t *testing.T) {
	must := require.New(t)

	release := make(chan struct{})
	blockingModel := httptest.NewServer(blockingModel(release))
	defer blockingModel.Close()
	defer close(release)

	mc, err := model.New(model.Options{Endpoint: blockingModel.URL, Model: "m", APIKey: "k"})
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
	must.Error(err, "Await must return the context error")

	st, err := w.state(id)
	must.NoError(err, "state")
	select {
	case <-st.done:
	case <-time.After(5 * time.Second):
		must.Fail("the run did not stop after Await's context ended")
	}
	in, err := w.Result(context.Background(), id)
	must.NoError(err, "Result")
	must.Equal(types.StatusIncomplete, in.Status, "detail: %s", in.StatusDetail)
}

// TestRunWorker_Trace_CarriesSpendAndNoCredential: the worker span carries the
// run's spend as attributes, emits no run-level metric of its own (the run
// core records those once), and a provider error body echoing the key never
// reaches a span attribute.
func TestRunWorker_Trace_CarriesSpendAndNoCredential(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	const key = "sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0"
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "A", URL: "https://a.example"}})
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"x"}`, FinishReason: "stop", Usage: model.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}},
		modeltest.FakeReply{Status: http.StatusUnauthorized, StatusBody: `{"error":{"message":"invalid api key ` + key + `"}}`},
	)
	defer modelSrv.Close()
	mc, err := model.New(model.Options{Endpoint: modelSrv.URL(), Model: "m", APIKey: key})
	must.NoError(err, "model.New")

	var out bytes.Buffer
	deps := WorkerDeps{
		Model:  mc,
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Tracer: trace.NewJSONL(&out),
		Caps:   caps(),
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})
	must.Equal(types.StatusFailed, finding.Status)

	lines := out.String()
	check.NotContains(lines, key, "the model key leaked into the trace")
	check.Contains(lines, `"`+trace.SpanWorker+`"`, "trace lacks the worker span")
	spanLine := lineContaining(lines, `"`+trace.SpanWorker+`"`)
	for _, tt := range []struct {
		name string
		attr string
	}{
		{name: "input tokens", attr: `"input_tokens":7`},
		{name: "output tokens", attr: `"output_tokens":3`},
		{name: "search count", attr: `"search_count":1`},
		{name: "status", attr: `"status":"failed"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Contains(spanLine, tt.attr)
		})
	}
	check.NotContains(lines, `"type":"metric"`, "the worker emitted a metric; run-level metrics belong to the run core")
}

func lineContaining(s, needle string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	return ""
}

// TestRunWorker_FindingCountsTurns: every outcome carries the number of model
// turns the loop started, including a turn whose model call failed, and a
// run whose context ended before its first turn reports none.
func TestRunWorker_FindingCountsTurns(t *testing.T) {
	searchTurn := func(n int) modeltest.FakeReply {
		return modeltest.FakeReply{Content: `{"action":"search","query":"q` + strconv.Itoa(n) + `"}`, FinishReason: "stop"}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tt := range []struct {
		name     string
		ctx      context.Context
		maxTurns int
		replies  []modeltest.FakeReply
		status   types.Status
		turns    int
	}{
		{"completed", context.Background(), 8, []modeltest.FakeReply{searchTurn(1), searchTurn(2), finalReply("done")}, types.StatusCompleted, 3},
		{"turn cap", context.Background(), 2, []modeltest.FakeReply{searchTurn(1), searchTurn(2), searchTurn(3)}, types.StatusIncomplete, 2},
		{"failed call", context.Background(), 8, []modeltest.FakeReply{searchTurn(1), {Status: http.StatusInternalServerError}}, types.StatusFailed, 2},
		{"context ended first", cancelled, 8, nil, types.StatusIncomplete, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			searchSrv := searchtest.NewFakeServer(nil)
			defer searchSrv.Close()
			modelSrv := modeltest.NewFakeServer(tt.replies...)
			defer modelSrv.Close()

			c := caps()
			c.MaxTurns = tt.maxTurns
			finding := RunWorker(tt.ctx, WorkerDeps{
				Model:  newModelClient(t, modelSrv),
				Search: newSearchClient(t, searchSrv),
				Fetch:  newFetchClient(t),
				Caps:   c,
			}, Brief{Objective: "q"})

			must.Equal(tt.status, finding.Status, "detail: %s", finding.Detail)
			must.Equal(tt.turns, finding.Turns, "Turns")
			must.Equal(tt.turns, modelSrv.CallCount(), "model calls, one per counted turn")
		})
	}
}
