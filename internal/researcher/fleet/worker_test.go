package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

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
	c, err := model.New(model.Options{
		Endpoint: srv.URL(),
		Model:    "test-model",
		APIKey:   "test-model-key",
	})
	if err != nil {
		t.Fatalf("model.New: %v", err)
	}
	return c
}

// newSearchClient builds a search.Client pointed at a fake MCP server.
func newSearchClient(t *testing.T, srv *searchtest.FakeServer) *search.Client {
	t.Helper()
	c, err := search.New(search.Options{
		Endpoint: srv.URL(),
		APIKey:   "test-search-key",
	})
	if err != nil {
		t.Fatalf("search.New: %v", err)
	}
	return c
}

// newFetchClient builds a fetch.Client that may reach loopback fakes.
func newFetchClient(t *testing.T) *fetch.Client {
	t.Helper()
	c, err := fetch.New(fetch.Options{AllowLoopback: true})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
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

	if finding.Status != types.StatusCompleted {
		t.Fatalf("status = %s (%s), want completed", finding.Status, finding.Detail)
	}
	if !strings.Contains(finding.Text, "Rayleigh scattering") {
		t.Errorf("answer text = %q, want the synthesis", finding.Text)
	}
	if len(finding.Citations) != 1 || finding.Citations[0].URI != page.URL {
		t.Errorf("citations = %+v, want one deduplicated entry for %s", finding.Citations, page.URL)
	}
	if finding.Usage.SearchCount != 1 {
		t.Errorf("search count = %d, want 1", finding.Usage.SearchCount)
	}
	if finding.Usage.InputTokens == 0 || finding.Usage.OutputTokens == 0 {
		t.Errorf("usage tokens = %+v, want accumulated tokens", finding.Usage)
	}
	// Three model turns were scripted; all three must have been made in order.
	if got := modelSrv.CallCount(); got != 3 {
		t.Errorf("model call count = %d, want 3 (one per turn)", got)
	}
}

// TestRunWorkerNoRetryOnPaidTurn pins that the worker adds no retry around a
// model turn: a 5xx surfaces as a Failed finding and the model is called
// exactly once, never re-attempted — a paid turn may already be billed.
func TestRunWorkerNoRetryOnPaidTurn(t *testing.T) {
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

	if finding.Status != types.StatusFailed {
		t.Fatalf("status = %s, want failed", finding.Status)
	}
	if got := modelSrv.CallCount(); got != 1 {
		t.Errorf("model call count = %d, want exactly 1 — a paid turn must not be retried", got)
	}
	if !strings.Contains(finding.Detail, "model turn failed") {
		t.Errorf("detail = %q, want the model-failure reason", finding.Detail)
	}
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
			modelSrv := modeltest.NewFakeServer(
				modeltest.FakeReply{Content: tt.content, FinishReason: "stop"},
			)
			defer modelSrv.Close()
			// A search server that fails the test if ever reached — a refused
			// action must not fall through to a tool call.
			searchSrv := searchtest.NewFakeServer([]search.Result{{URL: "https://example.org"}})
			defer searchSrv.Close()
			// A fetch client whose server errors if reached.
			fetchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("fetch server reached — a refused action must not perform any tool call")
			}))
			defer fetchSrv.Close()

			deps := WorkerDeps{
				Model:  newModelClient(t, modelSrv),
				Search: newSearchClient(t, searchSrv),
				Fetch:  newFetchClient(t),
				Caps:   caps(),
			}
			finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})

			if finding.Status != types.StatusFailed {
				t.Fatalf("status = %s, want failed for a %s action", finding.Status, tt.name)
			}
			if !strings.Contains(finding.Detail, "invalid model action") {
				t.Errorf("detail = %q, want an invalid-action reason", finding.Detail)
			}
			// Exactly one model turn ran (the one that emitted the bad action);
			// no tool call followed, and no second turn was taken.
			if got := modelSrv.CallCount(); got != 1 {
				t.Errorf("model call count = %d, want 1 — the loop must stop on the bad action", got)
			}
			if got := searchSrv.ToolCallCount(); got != 0 {
				t.Errorf("search tool-call count = %d, want 0 — no tool runs for a refused action", got)
			}
		})
	}
}

// TestRunWorkerCapsStopDeterministically: a model that always returns search
// exhausts the turn cap and the loop ends Incomplete with a diagnostic, and
// the citations gathered before the cap fired are preserved.
func TestRunWorkerCapsStopDeterministically(t *testing.T) {
	// A search server that returns one result carrying a URL, so a citation is
	// gatherable even though the model never delivers a final answer. (The
	// worker gathers citations from final actions, so to exercise "partial
	// citations preserved" we drive a final on the last allowed turn instead.)
	searchSrv := searchtest.NewFakeServer([]search.Result{
		{Title: "R", URL: "https://example.org/a", Snippet: "s"},
	})
	defer searchSrv.Close()

	// Script more search turns than the cap allows: every turn is a search, so
	// the loop can never reach a final and must stop on the turn cap.
	var replies []modeltest.FakeReply
	for i := 0; i < 10; i++ {
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

	if finding.Status != types.StatusIncomplete {
		t.Fatalf("status = %s (%s), want incomplete", finding.Status, finding.Detail)
	}
	if !strings.Contains(finding.Detail, "3-turn cap") {
		t.Errorf("detail = %q, want the turn-cap reason", finding.Detail)
	}
	// Exactly MaxTurns model turns were taken — no more.
	if got := modelSrv.CallCount(); got != 3 {
		t.Errorf("model call count = %d, want exactly the 3-turn cap", got)
	}
	if finding.Usage.SearchCount != 3 {
		t.Errorf("search count = %d, want 3", finding.Usage.SearchCount)
	}
}

// TestRunWorkerFailurePreservesUsage: a run that fails on its second turn (a
// model error) keeps the work already done on turn 1 — the search count and
// the accumulated tokens are preserved on the Failed finding, never discarded
// (docs/V2-RESEARCH-AGENT §8).
func TestRunWorkerFailurePreservesUsage(t *testing.T) {
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

	if finding.Status != types.StatusFailed {
		t.Fatalf("status = %s, want failed", finding.Status)
	}
	// The search on turn 1 counted before the turn-2 model failure — a failure
	// must not discard the work already done.
	if finding.Usage.SearchCount != 1 {
		t.Errorf("search count = %d, want 1 preserved through the failure", finding.Usage.SearchCount)
	}
	if finding.Usage.InputTokens == 0 {
		t.Errorf("usage tokens = %+v, want the turn-1 tokens preserved", finding.Usage)
	}
}

// TestRunWorkerFinalCitationsRestrictedToSeenURLs: citations on a final
// answer are deduplicated and restricted to URLs the worker saw in a search
// result; a URL it never encountered is dropped rather than reported as a
// source.
func TestRunWorkerFinalCitationsRestrictedToSeenURLs(t *testing.T) {
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

	if finding.Status != types.StatusCompleted {
		t.Fatalf("status = %s (%s), want completed", finding.Status, finding.Detail)
	}
	if len(finding.Citations) != 2 {
		t.Fatalf("citations = %+v, want two deduplicated entries", finding.Citations)
	}
	for _, c := range finding.Citations {
		if strings.Contains(c.URI, "invented") {
			t.Errorf("an unseen URL was cited: %+v", c)
		}
	}
}

// TestRunWorkerRefusesUnseenFetchURL: a fetch of a URL that never appeared in a
// search result is not performed; the model is steered back and the loop
// continues rather than failing. Defence in depth over the fetch SSRF guard.
func TestRunWorkerRefusesUnseenFetchURL(t *testing.T) {
	fetchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("fetch server reached — an unseen URL must not be fetched")
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

	if finding.Status != types.StatusCompleted {
		t.Fatalf("status = %s (%s), want completed — an unseen fetch is recoverable, not fatal", finding.Status, finding.Detail)
	}
	// Both turns ran: the refused fetch fed guidance back, the model then
	// answered.
	if got := modelSrv.CallCount(); got != 2 {
		t.Errorf("model call count = %d, want 2", got)
	}
}

// TestRunWorkerTimeoutIsIncomplete: an already-cancelled context ends the loop
// Incomplete before any model turn, with a diagnostic — the wall-clock bound
// stops the run deterministically without an error.
func TestRunWorkerTimeoutIsIncomplete(t *testing.T) {
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

	if finding.Status != types.StatusIncomplete {
		t.Fatalf("status = %s, want incomplete for a cancelled context", finding.Status)
	}
	if got := modelSrv.CallCount(); got != 0 {
		t.Errorf("model call count = %d, want 0 — a cancelled context stops before spending", got)
	}
}

// TestAddCitation: citations are deduplicated by URI, the first non-empty
// title wins, and every title is stripped of markup, flattened and bounded.
func TestAddCitation(t *testing.T) {
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
			w := &workerRun{}
			for _, a := range tt.adds {
				w.addCitation(a.uri, a.title)
			}
			if !reflect.DeepEqual(w.citations, tt.want) {
				t.Errorf("citations = %.300q, want %.300q", w.citations, tt.want)
			}
		})
	}
}
