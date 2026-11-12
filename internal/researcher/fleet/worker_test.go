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

	"github.com/rxbynerd/chiron/internal/researcher/fleet/fetch"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/types"
)

// newModelClient builds a model.Client pointed at a fake server, failing the
// test on construction error. The fake serves loopback, which the model
// client's endpoint validation admits for http.
func newModelClient(t *testing.T, srv *modeltest.FakeServer) *model.Client {
	t.Helper()
	must := require.New(t)
	c, err := model.New(model.Options{
		Endpoint: srv.URL(),
		Model:    "test-model",
		APIKey:   "test-model-key",
	})
	must.NoError(err, "model.New")
	return c
}

// newSearchClient builds a search.Client pointed at a fake MCP server.
func newSearchClient(t *testing.T, srv *searchtest.FakeServer) *search.Client {
	t.Helper()
	must := require.New(t)
	c, err := search.New(search.Options{
		Endpoint: srv.URL(),
		APIKey:   "test-search-key",
	})
	must.NoError(err, "search.New")
	return c
}

// newFetchClient builds a fetch.Client that may reach loopback fakes.
func newFetchClient(t *testing.T) *fetch.Client {
	t.Helper()
	must := require.New(t)
	c, err := fetch.New(fetch.Options{AllowLoopback: true})
	must.NoError(err, "fetch.New")
	return c
}

// finalReply scripts a model turn that ends the loop with a final answer and
// the given citation URLs.
func finalReply(answer string, urls ...string) modeltest.FakeReply {
	var cites strings.Builder
	for i, u := range urls {
		if i > 0 {
			cites.WriteString(",")
		}
		cites.WriteString(`{"url":"` + u + `","title":"src"}`)
	}
	content := `{"action":"final","answer":` + jsonString(answer) + `,"citations":[` + cites.String() + `]}`
	return modeltest.FakeReply{Content: content, FinishReason: "stop", Usage: model.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}}
}

// jsonString quotes s as a JSON string literal for embedding in a scripted
// reply.
func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

// caps returns a permissive Caps for tests that are not exercising a limit.
func caps() Caps {
	return Caps{MaxTurns: 8, MaxTokens: 0, CeilingGBP: 0, Timeout: 30 * time.Second}
}

// TestRunWorkerSearchFetchFinal drives the full loop: the model searches,
// fetches a result URL, then delivers a final answer citing it. The finding
// is Completed, carries the answer text, and deduplicates the citation.
func TestRunWorkerSearchFetchFinal(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("Rayleigh scattering makes the sky appear blue."))
	}))
	defer page.Close()

	searchSrv := searchtest.NewFakeServer([]search.Result{
		{Title: "Why the sky is blue", URL: page.URL, Snippet: "scattering"},
	})
	defer searchSrv.Close()

	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"why is the sky blue"}`, FinishReason: "stop", Usage: model.Usage{InputTokens: 30, OutputTokens: 8, TotalTokens: 38}},
		modeltest.FakeReply{Content: `{"action":"fetch","url":"` + page.URL + `"}`, FinishReason: "stop", Usage: model.Usage{InputTokens: 60, OutputTokens: 6, TotalTokens: 66}},
		finalReply("# Answer\n\nThe sky is blue due to Rayleigh scattering.", page.URL, page.URL),
	)
	defer modelSrv.Close()

	deps := WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "why is the sky blue"})

	must.Equal(types.StatusCompleted, finding.Status, "detail: %s", finding.Detail)
	check.Contains(finding.Text, "Rayleigh scattering")
	must.Len(finding.Citations, 1)
	check.Equal(page.URL, finding.Citations[0].URI)
	check.Equal(1, finding.Usage.SearchCount)
	check.NotZero(finding.Usage.InputTokens, "usage %+v", finding.Usage)
	check.NotZero(finding.Usage.OutputTokens, "usage %+v", finding.Usage)
	check.Equal(3, modelSrv.CallCount(), "one model call per scripted turn")
}

// TestRunWorkerNoRetryOnPaidTurn pins that the worker adds no retry around a
// model turn: a 5xx surfaces as a Failed finding and the model is called
// exactly once, never re-attempted — a paid turn may already be billed.
func TestRunWorkerNoRetryOnPaidTurn(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Status: http.StatusInternalServerError, StatusBody: `{"error":"boom"}`},
	)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()

	deps := WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})

	must.Equal(types.StatusFailed, finding.Status)
	check.Equal(1, modelSrv.CallCount(), "a paid turn must not be retried")
	check.Contains(finding.Detail, "model turn failed")
}

// TestRunWorkerRefusesSideEffectingActions is the closed-vocabulary guarantee:
// a model action of shell, write, exec, or any unknown kind fails the worker
// with no side effect. There is no dispatch path that could run one; the run
// ends Failed after the single model turn that emitted the action.
func TestRunWorkerRefusesSideEffectingActions(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
	}{
		{"shell", `{"action":"shell","query":"rm -rf /"}`},
		{"write", `{"action":"write","url":"file:///etc/passwd"}`},
		{"exec", `{"action":"exec"}`},
		{"unknown", `{"action":"teleport"}`},
		{"missing discriminator", `{"query":"no action field"}`},
		{"extra field smuggling", `{"action":"final","answer":"x","exec":"rm -rf /"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)

			modelSrv := modeltest.NewFakeServer(
				modeltest.FakeReply{Content: tt.content, FinishReason: "stop"},
			)
			defer modelSrv.Close()
			// Tool calls are counted on this server and asserted to be zero below.
			searchSrv := searchtest.NewFakeServer([]search.Result{{URL: "https://example.org"}})
			defer searchSrv.Close()
			// Any request to this server is a failure: a refused action must not fetch.
			fetchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				check.Fail("fetch server reached — a refused action must not perform any tool call")
			}))
			defer fetchSrv.Close()

			deps := WorkerDeps{
				Model:  newModelClient(t, modelSrv),
				Search: newSearchClient(t, searchSrv),
				Fetch:  newFetchClient(t),
				Caps:   caps(),
			}
			finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})

			must.Equal(types.StatusFailed, finding.Status)
			must.Contains(finding.Detail, "invalid model action")
			must.Equal(1, modelSrv.CallCount(), "the loop must stop on the bad action")
			must.Zero(searchSrv.ToolCallCount(), "no tool runs for a refused action")
		})
	}
}

// TestRunWorkerCapsStopDeterministically: a model that always returns search
// exhausts the turn cap, so the loop ends Incomplete with a diagnostic that
// names the cap.
func TestRunWorkerCapsStopDeterministically(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	// Every turn searches, so the model never reaches a final answer.
	searchSrv := searchtest.NewFakeServer([]search.Result{
		{Title: "R", URL: "https://example.org/a", Snippet: "s"},
	})
	defer searchSrv.Close()

	// Script more turns than the cap allows; the loop must stop at the cap.
	var replies []modeltest.FakeReply
	for range 10 {
		replies = append(replies, modeltest.FakeReply{
			Content:      `{"action":"search","query":"again"}`,
			FinishReason: "stop",
			Usage:        model.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
		})
	}
	modelSrv := modeltest.NewFakeServer(replies...)
	defer modelSrv.Close()

	c := caps()
	c.MaxTurns = 3
	deps := WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   c,
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})

	must.Equal(types.StatusIncomplete, finding.Status, "detail: %s", finding.Detail)
	check.Contains(finding.Detail, "3-turn cap")
	// Exactly MaxTurns model turns were taken — no more.
	check.Equal(3, modelSrv.CallCount(), "exactly the 3-turn cap")
	check.Equal(3, finding.Usage.SearchCount)
}

// TestRunWorker_FailurePreservesUsage: a run that fails on its second turn (a
// model error) keeps the work already done on turn 1 — the search count and
// the accumulated tokens are preserved on the Failed finding, never discarded
// (docs/V2-RESEARCH-AGENT §8).
func TestRunWorker_FailurePreservesUsage(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	searchSrv := searchtest.NewFakeServer([]search.Result{{URL: "https://example.org/a"}})
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"x"}`, FinishReason: "stop", Usage: model.Usage{InputTokens: 20, OutputTokens: 4, TotalTokens: 24}},
		modeltest.FakeReply{Status: http.StatusBadGateway, StatusBody: "upstream down"},
	)
	defer modelSrv.Close()

	deps := WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})

	must.Equal(types.StatusFailed, finding.Status)
	// The search on turn 1 counted before the turn-2 model failure — a failure
	// must not discard the work already done.
	check.Equal(1, finding.Usage.SearchCount, "search count preserved through the failure")
	check.NotZero(finding.Usage.InputTokens, "usage %+v", finding.Usage)
}

// TestRunWorkerFinalCitationsRestrictedToSeenURLs: citations on a final
// answer are deduplicated and restricted to URLs the worker saw in a search
// result; a URL it never encountered is dropped rather than reported as a
// source.
func TestRunWorkerFinalCitationsRestrictedToSeenURLs(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	searchSrv := searchtest.NewFakeServer([]search.Result{
		{Title: "A", URL: "https://a.example"},
		{Title: "B", URL: "https://b.example"},
	})
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"x"}`, FinishReason: "stop"},
		finalReply("answer", "https://a.example", "https://b.example", "https://a.example", "https://invented.example/never-seen"),
	)
	defer modelSrv.Close()

	deps := WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})

	must.Equal(types.StatusCompleted, finding.Status, "detail: %s", finding.Detail)
	must.Len(finding.Citations, 2)
	for _, c := range finding.Citations {
		check.NotContains(c.URI, "invented", "an unseen URL was cited: %+v", c)
	}
}

// TestRunWorkerRefusesUnseenFetchURL: a fetch of a URL that never appeared in a
// search result is not performed; the model is steered back and the loop
// continues rather than failing. Defence in depth over the fetch SSRF guard.
func TestRunWorkerRefusesUnseenFetchURL(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fetchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Fail("fetch server reached — an unseen URL must not be fetched")
	}))
	defer fetchSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()

	modelSrv := modeltest.NewFakeServer(
		// Turn 1: fetch a URL that was never in a search result.
		modeltest.FakeReply{Content: `{"action":"fetch","url":"` + fetchSrv.URL + `"}`, FinishReason: "stop"},
		// Turn 2: give up and answer.
		finalReply("done"),
	)
	defer modelSrv.Close()

	deps := WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})

	must.Equal(types.StatusCompleted, finding.Status, "an unseen fetch is recoverable, not fatal: %s", finding.Detail)
	// Both turns ran: the refused fetch fed guidance back, the model then
	// answered.
	must.Equal(2, modelSrv.CallCount())
}

// TestRunWorkerTimeoutIsIncomplete: an already-cancelled context ends the loop
// Incomplete before any model turn, with a diagnostic — the wall-clock bound
// stops the run deterministically without an error.
func TestRunWorkerTimeoutIsIncomplete(t *testing.T) {
	must := require.New(t)

	modelSrv := modeltest.NewFakeServer() // never reached.
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the loop starts.

	deps := WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	}
	finding := RunWorker(ctx, deps, Brief{Objective: "q"})

	must.Equal(types.StatusIncomplete, finding.Status)
	must.Zero(modelSrv.CallCount(), "a cancelled context stops before spending")
}

// TestWorkerRun_AddCitation_DedupesAndCleansTitles: citations are deduplicated
// by URI, the first non-empty title wins, and every title is stripped of
// markup, flattened and bounded.
func TestWorkerRun_AddCitation_DedupesAndCleansTitles(t *testing.T) {
	type add struct{ uri, title string }
	long := strings.Repeat("ü", 10<<10)
	for _, tt := range []struct {
		name string
		adds []add
		want []types.Citation
	}{
		{"empty URI dropped", []add{{"", "t"}}, nil},
		{"first title wins", []add{{"https://a", "one"}, {"https://a", "two"}}, []types.Citation{{URI: "https://a", Title: "one"}}},
		{"empty title replaced", []add{{"https://a", ""}, {"https://a", "later"}}, []types.Citation{{URI: "https://a", Title: "later"}}},
		{"markup-only title replaced", []add{{"https://a", "<b></b>"}, {"https://a", "later"}}, []types.Citation{{URI: "https://a", Title: "later"}}},
		{"markup stripped", []add{{"https://a", `Policy <img src="https://beacon.example/p.gif"> [x]`}}, []types.Citation{{URI: "https://a", Title: "Policy [x]"}}},
		{"multi-line tag stripped", []add{{"https://a", "a<img\nsrc=x>b"}}, []types.Citation{{URI: "https://a", Title: "ab"}}},
		{"flattened", []add{{"https://a", "line one\n\tline two"}}, []types.Citation{{URI: "https://a", Title: "line one line two"}}},
		{"bounded", []add{{"https://a", "<img src=x>" + long}}, []types.Citation{{URI: "https://a", Title: long[:maxCitationTitleRunes*len("ü")]}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			w := &workerRun{}
			for _, a := range tt.adds {
				w.addCitation(a.uri, a.title)
			}
			must.Equal(tt.want, w.citations)
		})
	}
}
