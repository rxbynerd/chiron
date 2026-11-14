package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// cappedModel stands in for a provider whose model searches on every turn
// unless strict structured output restricts the action to final, as the
// final-only schema does. With ignoreFinalOnly set it searches even then,
// standing in for a provider that does not enforce the schema; with
// answerAtOnce set it answers on every turn. Every reply reports usage. It
// records each request's action enum and last message.
type cappedModel struct {
	t               *testing.T
	usage           model.Usage
	ignoreFinalOnly bool
	answerAtOnce    bool

	mu       sync.Mutex
	enums    [][]string
	lastMsgs []string
}

// cappedAnswer is the answer cappedModel gives on a final-only turn, citing
// cappedSourceURL.
const (
	cappedAnswer    = "The answer from the sources gathered so far."
	cappedSourceURL = "https://example.org/a"
)

func (m *cappedModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		ResponseFormat struct {
			JSONSchema struct {
				Schema struct {
					Properties struct {
						Action struct {
							Enum []string `json:"enum"`
						} `json:"action"`
					} `json:"properties"`
				} `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) == 0 {
		m.t.Errorf("decode model request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	enum := body.ResponseFormat.JSONSchema.Schema.Properties.Action.Enum
	m.mu.Lock()
	m.enums = append(m.enums, enum)
	m.lastMsgs = append(m.lastMsgs, body.Messages[len(body.Messages)-1].Content)
	m.mu.Unlock()

	content := `{"action":"search","query":"again","url":null,"answer":null,"citations":null}`
	if m.answerAtOnce || (slices.Equal(enum, []string{"final"}) && !m.ignoreFinalOnly) {
		content = `{"action":"final","query":null,"url":null,"answer":` + jsonString(cappedAnswer) +
			`,"citations":[{"url":"` + cappedSourceURL + `","title":"A"}]}`
	}
	writeChatReply(w, modeltest.FakeReply{Content: content, Usage: m.usage})
}

// requests returns the action enum and last message of every request, in
// arrival order.
func (m *cappedModel) requests() (enums [][]string, lastMsgs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.enums), slices.Clone(m.lastMsgs)
}

// runCapped runs one worker against a cappedModel under c, tracing to tracer
// when it is non-nil, with a search server returning cappedSourceURL, and
// returns the finding and the number of searches the search server ran.
func runCapped(t *testing.T, m *cappedModel, c Caps, tracer trace.Tracer) (Finding, int) {
	t.Helper()
	modelSrv := httptest.NewServer(m)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "A", URL: cappedSourceURL, Snippet: "s"}})
	defer searchSrv.Close()
	mc, err := model.New(model.Options{Endpoint: modelSrv.URL, Model: "m", APIKey: "k"})
	require.New(t).NoError(err, "model.New")
	finding := RunWorker(context.Background(), WorkerDeps{
		Model:  mc,
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Tracer: tracer,
		Caps:   c,
	}, Brief{Objective: "q"})
	return finding, searchSrv.ToolCallCount()
}

// Action enums the worker offers on an ordinary turn and on a final-only
// turn, without recall.
var (
	everyActionEnum = []string{"search", "fetch", "final"}
	finalOnlyEnum   = []string{"final"}
)

// TestRunWorkerTokenCapStops: when the tokens so far plus the previous
// turn's would reach MaxTokens, the coming turn is final-only. A model that
// answers it completes with text; one that searches instead ends Incomplete
// on the token cap with no search run. When a turn outgrows that prediction,
// the cap still stops the loop before the next paid turn.
func TestRunWorkerTokenCapStops(t *testing.T) {
	t.Run("final-only turn answered", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		c := caps()
		c.MaxTokens = 100
		m := &cappedModel{t: t, usage: model.Usage{InputTokens: 40, OutputTokens: 20}}
		finding, searches := runCapped(t, m, c, nil)

		must.Equal(types.StatusCompleted, finding.Status, "detail: %s", finding.Detail)
		check.Equal(cappedAnswer, finding.Text)
		enums, lastMsgs := m.requests()
		// 60 tokens after turn 1, plus turn 1's 60, reaches the cap, so turn 2 is final-only.
		check.Equal([][]string{everyActionEnum, finalOnlyEnum}, enums, "action enum per request")
		check.Contains(lastMsgs[1], turnNote(2, c.MaxTurns, true))
		check.Equal(1, searches)
		check.Equal(120, totalTokens(finding.Usage))
	})
	t.Run("final-only turn ignored", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		c := caps()
		c.MaxTokens = 100
		m := &cappedModel{t: t, usage: model.Usage{InputTokens: 40, OutputTokens: 20}, ignoreFinalOnly: true}
		finding, searches := runCapped(t, m, c, nil)

		must.Equal(types.StatusIncomplete, finding.Status, "detail: %s", finding.Detail)
		check.Contains(finding.Detail, "100-token cap")
		check.Empty(finding.Text)
		enums, _ := m.requests()
		check.Equal([][]string{everyActionEnum, finalOnlyEnum}, enums, "action enum per request")
		check.Equal(1, searches, "the search returned on the final-only turn must not run")
	})
	t.Run("cap stops a turn that outgrew the prediction", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		searchSrv := searchtest.NewFakeServer(nil)
		defer searchSrv.Close()
		searchTurn := func(in, out int) modeltest.FakeReply {
			return modeltest.FakeReply{Content: `{"action":"search","query":"again"}`, FinishReason: "stop", Usage: model.Usage{InputTokens: in, OutputTokens: out}}
		}
		modelSrv := modeltest.NewFakeServer(searchTurn(8, 2), searchTurn(80, 15), searchTurn(1, 1))
		defer modelSrv.Close()
		c := caps()
		c.MaxTokens = 100
		finding := RunWorker(context.Background(), WorkerDeps{
			Model:  newModelClient(t, modelSrv),
			Search: newSearchClient(t, searchSrv),
			Fetch:  newFetchClient(t),
			Caps:   c,
		}, Brief{Objective: "q"})

		must.Equal(types.StatusIncomplete, finding.Status, "detail: %s", finding.Detail)
		check.Contains(finding.Detail, "100-token cap")
		// 10 tokens plus turn 1's 10 is under the cap, so turn 2 is ordinary;
		// its 95 take the total to 105, which stops the loop before turn 3.
		check.Equal(2, modelSrv.CallCount())
		check.Equal(105, totalTokens(finding.Usage))
		for i, req := range modelSrv.Requests() {
			check.NotContains(string(req.Schema), `"enum":["final"]`, "request %d was final-only", i)
		}
	})
}

// TestRunWorker_CostCeilingStops: with a price configured, the estimated
// cost so far plus the previous turn's reaching the GBP ceiling makes the
// coming turn final-only, and the ceiling still stops the loop when a turn
// outgrows that prediction.
func TestRunWorker_CostCeilingStops(t *testing.T) {
	priced := func() Caps {
		c := caps()
		c.InputGBPPerMTok = 2
		c.OutputGBPPerMTok = 8
		c.CeilingGBP = 3
		return c
	}
	t.Run("final-only turn answered", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		c := priced()
		m := &cappedModel{t: t, usage: model.Usage{InputTokens: 1_000_000}}
		finding, searches := runCapped(t, m, c, nil)

		must.Equal(types.StatusCompleted, finding.Status, "detail: %s", finding.Detail)
		check.Equal(cappedAnswer, finding.Text)
		enums, lastMsgs := m.requests()
		// £2 after turn 1, plus turn 1's £2, reaches the £3 ceiling, so turn 2 is final-only.
		check.Equal([][]string{everyActionEnum, finalOnlyEnum}, enums, "action enum per request")
		check.Contains(lastMsgs[1], turnNote(2, c.MaxTurns, true))
		check.Equal(1, searches)
		// InDelta with zero delta is an exact comparison; the spend is exact in binary floating point.
		check.InDelta(4.0, finding.Usage.EstimatedCostGBP, 0)
	})
	t.Run("final-only turn ignored", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		m := &cappedModel{t: t, usage: model.Usage{InputTokens: 1_000_000}, ignoreFinalOnly: true}
		finding, searches := runCapped(t, m, priced(), nil)

		must.Equal(types.StatusIncomplete, finding.Status, "detail: %s", finding.Detail)
		check.Contains(finding.Detail, "£3.00 cost ceiling")
		enums, _ := m.requests()
		check.Equal([][]string{everyActionEnum, finalOnlyEnum}, enums, "action enum per request")
		check.Equal(1, searches, "the search returned on the final-only turn must not run")
	})
	t.Run("ceiling stops a turn that outgrew the prediction", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		searchSrv := searchtest.NewFakeServer(nil)
		defer searchSrv.Close()
		searchTurn := func(in int) modeltest.FakeReply {
			return modeltest.FakeReply{Content: `{"action":"search","query":"again"}`, FinishReason: "stop", Usage: model.Usage{InputTokens: in}}
		}
		modelSrv := modeltest.NewFakeServer(searchTurn(500_000), searchTurn(1_500_000), searchTurn(1))
		defer modelSrv.Close()
		finding := RunWorker(context.Background(), WorkerDeps{
			Model:  newModelClient(t, modelSrv),
			Search: newSearchClient(t, searchSrv),
			Fetch:  newFetchClient(t),
			Caps:   priced(),
		}, Brief{Objective: "q"})

		must.Equal(types.StatusIncomplete, finding.Status, "detail: %s", finding.Detail)
		check.Contains(finding.Detail, "£3.00 cost ceiling")
		// £1 plus turn 1's £1 is under the ceiling, so turn 2 is ordinary;
		// its £3 take the total to £4, which stops the loop before turn 3.
		check.Equal(2, modelSrv.CallCount())
		check.InDelta(4.0, finding.Usage.EstimatedCostGBP, 0)
		for i, req := range modelSrv.Requests() {
			check.NotContains(string(req.Schema), `"enum":["final"]`, "request %d was final-only", i)
		}
	})
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

// TestRunWorker_Trace_RecordsFinalOnlyTurn: the worker span records whether
// the run's last turn was final-only and, when it was, which cap made it so.
func TestRunWorker_Trace_RecordsFinalOnlyTurn(t *testing.T) {
	tokenCapped := caps()
	tokenCapped.MaxTokens = 100
	twoTurns := caps()
	twoTurns.MaxTurns = 2
	for _, tt := range []struct {
		name         string
		caps         Caps
		answerAtOnce bool
		want         []string
		notWant      string
	}{
		{"turn cap", twoTurns, false, []string{`"final_only_turn":true`, `"final_only_cap":"turns"`}, ""},
		{"token cap", tokenCapped, false, []string{`"final_only_turn":true`, `"final_only_cap":"tokens"`}, ""},
		{"no cap pressure", caps(), true, []string{`"final_only_turn":false`}, `"final_only_cap"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			check := assert.New(t)
			var out bytes.Buffer
			m := &cappedModel{t: t, usage: model.Usage{InputTokens: 40, OutputTokens: 20}, answerAtOnce: tt.answerAtOnce}
			runCapped(t, m, tt.caps, trace.NewJSONL(&out))

			spanLine := lineContaining(out.String(), `"`+trace.SpanWorker+`"`)
			for _, attr := range tt.want {
				check.Contains(spanLine, attr)
			}
			if tt.notWant != "" {
				check.NotContains(spanLine, tt.notWant)
			}
		})
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
