package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/memory/alexandria"
	"github.com/rxbynerd/chiron/internal/memory/billet"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
)

// TestWorkerRecallSearchFetchFinalThroughCLI drives a worker that recalls
// from Billet, searches, fetches and cites both a page and a memory: the
// recall reaches Billet as search_memory, its content reaches the model
// inside the fence, and the report lists the memory beside the web source.
func TestWorkerRecallSearchFetchFinalThroughCLI(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>Rayleigh scattering.</p></body></html>"))
	}))
	defer page.Close()
	searchSrv := search.NewFakeServer([]search.Result{{Title: "Sky article", URL: page.URL, Snippet: "scattering"}})
	defer searchSrv.Close()
	kbSrv := billet.NewFakeServer([]billet.Record{{
		MemoryID:  "mem-0a1b",
		Content:   "Prior finding: the team settled on Rayleigh scattering as the accepted explanation.",
		Score:     0.9,
		CreatedAt: "2026-09-01T00:00:00Z",
	}})
	defer kbSrv.Close()
	modelSrv := model.NewFakeServer(
		model.FakeReply{Content: `{"action":"recall","query":"sky colour"}`, FinishReason: "stop"},
		model.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		model.FakeReply{Content: `{"action":"fetch","url":"` + page.URL + `"}`, FinishReason: "stop"},
		model.FakeReply{Content: `{"action":"final","answer":"# Answer\n\nRayleigh scattering.","citations":[` +
			`{"url":"` + page.URL + `","title":"Sky article"},` +
			`{"url":"billet://memory/mem-0a1b","title":"Prior finding"}]}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("CHIRON_FETCH_ALLOW_LOOPBACK", "1")

	stdout, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "text",
		"--fleet-knowledge-provider", "billet",
		"--fleet-knowledge-endpoint", kbSrv.URL(),
		"--fleet-knowledge-limit", "3",
	)...)
	if err != nil {
		t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr)
	}
	if got := modelSrv.CallCount(); got != 4 {
		t.Errorf("model call count = %d, want 4", got)
	}
	if got := kbSrv.ToolCallCount(); got != 1 {
		t.Errorf("billet tool calls = %d, want 1 (a recall, no save-back)", got)
	}
	var sawRecall bool
	for _, req := range kbSrv.Requests() {
		if req.ToolName != "search_memory" {
			continue
		}
		sawRecall = true
		if req.Arguments["query"] != "sky colour" || req.Arguments["limit"] != float64(3) {
			t.Errorf("search_memory arguments = %v, want query and the configured limit", req.Arguments)
		}
	}
	if !sawRecall {
		t.Error("Billet never received search_memory")
	}
	if !strings.Contains(stdout, "[Sky article]("+page.URL+")") {
		t.Errorf("report lacks the fetched source:\n%s", stdout)
	}
	// The recall's own title (the memory's first line) wins over the title
	// the model attached to its citation.
	if !strings.Contains(stdout, "Prior finding: the team settled on Rayleigh scattering as the accepted explanation. `billet://memory/mem-0a1b`") {
		t.Errorf("report lacks the recalled memory as an unlinked source:\n%s", stdout)
	}
	if strings.Contains(stdout, "(billet://") {
		t.Errorf("a billet locator was rendered as a link:\n%s", stdout)
	}
}

// TestWorkerKnowledgeRememberThroughCLI: with knowledge_remember, a completed
// finding is saved back to Billet once, carrying the objective, the answer and
// the sources, and the interaction lists the remember tool.
func TestWorkerKnowledgeRememberThroughCLI(t *testing.T) {
	searchSrv := search.NewFakeServer(nil)
	defer searchSrv.Close()
	kbSrv := billet.NewFakeServer(nil)
	defer kbSrv.Close()
	modelSrv := model.NewFakeServer(
		model.FakeReply{Content: `{"action":"final","answer":"The sky is blue because of Rayleigh scattering.","citations":[]}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")

	stdout, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "text",
		"--fleet-knowledge-provider", "billet",
		"--fleet-knowledge-endpoint", kbSrv.URL(),
		"--fleet-knowledge-remember",
	)...)
	if err != nil {
		t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr)
	}
	saved := kbSrv.SavedContents()
	if len(saved) != 1 {
		t.Fatalf("saved contents = %d, want exactly one save-back", len(saved))
	}
	for _, want := range []string{"why is the sky blue", "Rayleigh scattering"} {
		if !strings.Contains(saved[0], want) {
			t.Errorf("saved content lacks %q:\n%s", want, saved[0])
		}
	}
	if strings.Contains(saved[0], "test-model-key") {
		t.Error("the model key reached the knowledge store")
	}
	if !strings.Contains(stdout, "tools: [web_search, web_fetch, knowledge_recall, knowledge_remember]") {
		t.Errorf("front matter lacks the knowledge tools:\n%s", stdout)
	}
	if !strings.Contains(stderr, "saved the finding to the knowledge store") {
		t.Errorf("stderr lacks the save-back log line:\n%s", stderr)
	}
}

// TestWorkerRecallAlexandriaThroughCLI: the Alexandria provider recalls over
// REST with the bearer and the configured space, and a fragment hit is
// citable by its web locator.
func TestWorkerRecallAlexandriaThroughCLI(t *testing.T) {
	searchSrv := search.NewFakeServer(nil)
	defer searchSrv.Close()
	kbSrv := alexandria.NewFakeServer(alexandria.SearchResponse{Results: []alexandria.SearchResult{{
		Ref:     "kb://fragment/0f2e4d6c-1111-2222-3333-444455556666",
		Unit:    "fragment",
		ID:      "0f2e4d6c-1111-2222-3333-444455556666",
		Space:   "notes",
		Title:   "Sky colour decision",
		Kind:    "decision",
		Snippet: "Rayleigh scattering explains the blue sky.",
		Score:   0.8,
	}}})
	defer kbSrv.Close()
	locator := kbSrv.URL() + "/f/0f2e4d6c-1111-2222-3333-444455556666"
	modelSrv := model.NewFakeServer(
		model.FakeReply{Content: `{"action":"recall","query":"sky colour"}`, FinishReason: "stop"},
		model.FakeReply{Content: `{"action":"final","answer":"Rayleigh scattering.","citations":[{"url":"` + locator + `","title":"Sky colour decision"}]}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("KB_KEY", "alx_test-token")

	stdout, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "text",
		"--fleet-knowledge-provider", "alexandria",
		"--fleet-knowledge-endpoint", kbSrv.URL(),
		"--fleet-knowledge-key-ref", "secret://KB_KEY",
		"--fleet-knowledge-space", "notes",
	)...)
	if err != nil {
		t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr)
	}
	reqs := kbSrv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("alexandria requests = %d, want 1", len(reqs))
	}
	if reqs[0].Authorization != "Bearer alx_test-token" || reqs[0].Query.Get("space") != "notes" || reqs[0].Query.Get("q") != "sky colour" {
		t.Errorf("recall request = %+v, want the bearer, the space and the query", reqs[0])
	}
	if !strings.Contains(stdout, "[Sky colour decision]("+locator+")") {
		t.Errorf("report lacks the fragment locator as a source:\n%s", stdout)
	}
	if strings.Contains(stdout+stderr, "alx_test-token") {
		t.Error("the knowledge token leaked into the output")
	}
}
