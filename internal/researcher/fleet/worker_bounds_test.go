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

	if finding.Status != types.StatusIncomplete {
		t.Fatalf("status = %s (%s), want incomplete", finding.Status, finding.Detail)
	}
	if !strings.Contains(finding.Detail, "100-token cap") {
		t.Errorf("detail = %q, want the token-cap reason", finding.Detail)
	}
	// 60 tokens after turn 1 is under the cap; 120 after turn 2 is over it.
	if got := modelSrv.CallCount(); got != 2 {
		t.Errorf("model call count = %d, want 2", got)
	}
	if finding.Usage.InputTokens != 80 || finding.Usage.OutputTokens != 40 {
		t.Errorf("usage = %+v, want 80 in / 40 out", finding.Usage)
	}
}

// TestRunWorkerCostCeilingStops: with a price configured the estimated cost
// accumulates and the GBP ceiling stops the loop Incomplete.
func TestRunWorkerCostCeilingStops(t *testing.T) {
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

	if finding.Status != types.StatusIncomplete {
		t.Fatalf("status = %s (%s), want incomplete", finding.Status, finding.Detail)
	}
	if !strings.Contains(finding.Detail, "£3.00 cost ceiling") {
		t.Errorf("detail = %q, want the cost-ceiling reason", finding.Detail)
	}
	// £2 after turn 1 is under the ceiling; £4 after turn 2 is over it.
	if got := modelSrv.CallCount(); got != 2 {
		t.Errorf("model call count = %d, want 2", got)
	}
	if finding.Usage.EstimatedCostGBP != 4 {
		t.Errorf("estimated cost = %v, want 4", finding.Usage.EstimatedCostGBP)
	}
}

// TestRunWorkerTimeoutMidTurnIsIncomplete: the per-worker timeout firing while
// a model call is in flight ends the run Incomplete, not Failed.
func TestRunWorkerTimeoutMidTurnIsIncomplete(t *testing.T) {
	release := make(chan struct{})
	blockingModel := httptest.NewServer(blockingModel(release))
	defer blockingModel.Close()
	defer close(release)

	mc, err := model.New(model.Options{Endpoint: blockingModel.URL, Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatalf("model.New: %v", err)
	}
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

	if finding.Status != types.StatusIncomplete {
		t.Fatalf("status = %s (%s), want incomplete when the timeout fires mid-turn", finding.Status, finding.Detail)
	}
	if !strings.Contains(finding.Detail, "context ended") {
		t.Errorf("detail = %q, want the context reason", finding.Detail)
	}
}

// TestRunWorkerLengthFinishIsIncomplete: a reply cut off at the completion cap
// is a bounded stop, not an invalid action.
func TestRunWorkerLengthFinishIsIncomplete(t *testing.T) {
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

	if finding.Status != types.StatusIncomplete {
		t.Fatalf("status = %s (%s), want incomplete", finding.Status, finding.Detail)
	}
	if !strings.Contains(finding.Detail, "cut off") {
		t.Errorf("detail = %q, want the truncation reason", finding.Detail)
	}
}

// TestWorkerAwaitCancelStopsRun: when Await's context ends, the run is
// cancelled so no further paid turns are taken, and the run concludes
// Incomplete.
func TestWorkerAwaitCancelStopsRun(t *testing.T) {
	release := make(chan struct{})
	blockingModel := httptest.NewServer(blockingModel(release))
	defer blockingModel.Close()
	defer close(release)

	mc, err := model.New(model.Options{Endpoint: blockingModel.URL, Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatalf("model.New: %v", err)
	}
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	w, err := NewWorker(WorkerDeps{
		Model:  mc,
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   Caps{MaxTurns: 8, Timeout: 30 * time.Second},
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	id, err := w.Start(context.Background(), researcher.Task{Query: "q"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := w.Await(ctx, id); err == nil {
		t.Fatal("Await must return the context error")
	}

	st, err := w.state(id)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	select {
	case <-st.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not stop after Await's context ended")
	}
	in, err := w.Result(context.Background(), id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Status != types.StatusIncomplete {
		t.Errorf("status = %s (%s), want incomplete after cancellation", in.Status, in.StatusDetail)
	}
}

// TestRunWorkerTraceCarriesSpendAndNoCredential: the worker span carries the
// run's spend as attributes, emits no run-level metric of its own (the run
// core records those once), and a provider error body echoing the key never
// reaches a span attribute.
func TestRunWorkerTraceCarriesSpendAndNoCredential(t *testing.T) {
	const key = "sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0"
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "A", URL: "https://a.example"}})
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"x"}`, FinishReason: "stop", Usage: model.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}},
		modeltest.FakeReply{Status: http.StatusUnauthorized, StatusBody: `{"error":{"message":"invalid api key ` + key + `"}}`},
	)
	defer modelSrv.Close()
	mc, err := model.New(model.Options{Endpoint: modelSrv.URL(), Model: "m", APIKey: key})
	if err != nil {
		t.Fatalf("model.New: %v", err)
	}

	var out bytes.Buffer
	deps := WorkerDeps{
		Model:  mc,
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Tracer: trace.NewJSONL(&out),
		Caps:   caps(),
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})
	if finding.Status != types.StatusFailed {
		t.Fatalf("status = %s, want failed", finding.Status)
	}

	lines := out.String()
	if strings.Contains(lines, key) {
		t.Errorf("the model key leaked into the trace:\n%s", lines)
	}
	if !strings.Contains(lines, `"`+trace.SpanWorker+`"`) {
		t.Errorf("trace lacks the worker span:\n%s", lines)
	}
	spanLine := lineContaining(lines, `"`+trace.SpanWorker+`"`)
	for _, attr := range []string{`"input_tokens":7`, `"output_tokens":3`, `"search_count":1`, `"status":"failed"`} {
		if !strings.Contains(spanLine, attr) {
			t.Errorf("worker span lacks %s:\n%s", attr, spanLine)
		}
	}
	if strings.Contains(lines, `"type":"metric"`) {
		t.Errorf("the worker emitted a metric; run-level metrics belong to the run core:\n%s", lines)
	}
}

func lineContaining(s, needle string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	return ""
}

// TestRunWorkerFindingCountsTurns: every outcome carries the number of model
// turns the loop started, including a turn whose model call failed, and a
// run whose context ended before its first turn reports none.
func TestRunWorkerFindingCountsTurns(t *testing.T) {
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

			if finding.Status != tt.status || finding.Turns != tt.turns {
				t.Errorf("finding = %s after %d turns (%s), want %s after %d", finding.Status, finding.Turns, finding.Detail, tt.status, tt.turns)
			}
			if got := modelSrv.CallCount(); got != tt.turns {
				t.Errorf("model calls = %d, want one per counted turn (%d)", got, tt.turns)
			}
		})
	}
}
