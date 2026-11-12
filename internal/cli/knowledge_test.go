package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/memory/alexandria"
	"github.com/rxbynerd/chiron/internal/memory/alexandria/alexandriatest"
	"github.com/rxbynerd/chiron/internal/memory/billet/billettest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
)

// TestWorker_RecallSearchFetchFinal_CitesPageAndMemory drives a worker that recalls
// from Billet, searches, fetches and cites both a page and a memory: the
// recall reaches Billet as search_memory, its content reaches the model
// inside the fence, and the report lists the memory beside the web source.
func TestWorker_RecallSearchFetchFinal_CitesPageAndMemory(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>Rayleigh scattering.</p></body></html>"))
	}))
	defer page.Close()
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "Sky article", URL: page.URL, Snippet: "scattering"}})
	defer searchSrv.Close()
	kbSrv := billettest.NewFakeServer([]billettest.Record{{
		MemoryID:  "mem-0a1b",
		Content:   "Prior finding: the team settled on Rayleigh scattering as the accepted explanation.",
		Score:     0.9,
		CreatedAt: "2026-09-01T00:00:00Z",
	}})
	defer kbSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"recall","query":"sky colour"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"fetch","url":"` + page.URL + `"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"final","answer":"# Answer\n\nRayleigh scattering.","citations":[` +
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
	must.NoError(err, "research --agent worker, stderr: %s", stderr)

	check.Equal(4, modelSrv.CallCount(), "model call count")
	check.Equal(1, kbSrv.ToolCallCount(), "Billet calls: one recall, no save-back")
	var sawRecall bool
	for _, req := range kbSrv.Requests() {
		if req.ToolName != "search_memory" {
			continue
		}
		sawRecall = true
		check.Equal("sky colour", req.Arguments["query"], "search_memory arguments: %v", req.Arguments)
		check.InDelta(3, req.Arguments["limit"], 0, "search_memory arguments: %v", req.Arguments)
	}
	check.True(sawRecall, "Billet never received search_memory")
	check.Contains(stdout, "[Sky article]("+page.URL+")", "report lacks the fetched source")
	// The recall's own title (the memory's first line) wins over the title
	// the model attached to its citation.
	check.Contains(stdout, "Prior finding: the team settled on Rayleigh scattering as the accepted explanation. `billet://memory/mem-0a1b`",
		"report lacks the recalled memory as an unlinked source")
	check.NotContains(stdout, "(billet://", "a billet locator was rendered as a link")
}

// TestWorker_Remember_SavesCompletedFindingOnce: with knowledge_remember, a completed
// finding is saved back to Billet once, carrying the provenance header, the
// objective, the answer and the sources, and the interaction lists the
// remember tool.
func TestWorker_Remember_SavesCompletedFindingOnce(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	kbSrv := billettest.NewFakeServer(nil)
	defer kbSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"final","answer":"The sky is blue because of Rayleigh scattering.","citations":[]}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")

	stdout, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "text",
		"--fleet-knowledge-provider", "billet",
		"--fleet-knowledge-endpoint", kbSrv.URL(),
		"--fleet-knowledge-remember",
	)...)
	must.NoError(err, "research --agent worker, stderr: %s", stderr)

	saved := kbSrv.SavedContents()
	must.Len(saved, 1, "expected exactly one save-back")
	check.True(strings.HasPrefix(saved[0], "why is the sky blue\n(Chiron worker finding; interaction wkr_"),
		"saved content lacks the objective line and the provenance line:\n%s", saved[0])
	for _, tt := range []struct{ name, want string }{
		{name: "objective", want: "why is the sky blue"},
		{name: "answer", want: "Rayleigh scattering"},
	} {
		t.Run("saved content has "+tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Contains(saved[0], tt.want)
		})
	}
	check.NotContains(saved[0], "test-model-key", "the model key reached the knowledge store")
	check.Contains(stdout, "tools: [web_search, web_fetch, knowledge_recall, knowledge_remember]",
		"front matter lacks the knowledge tools")
	check.Contains(stderr, "saved the finding to the knowledge store", "stderr lacks the save-back log line")
}

// TestWorker_RecallAlexandria_CitesFragmentLocator: the Alexandria provider recalls over
// REST with the bearer and the configured space, and a fragment hit is
// citable by its web locator.
func TestWorker_RecallAlexandria_CitesFragmentLocator(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	kbSrv := alexandriatest.NewFakeServer(alexandria.SearchResponse{Results: []alexandria.SearchResult{{
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
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"recall","query":"sky colour"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"final","answer":"Rayleigh scattering.","citations":[{"url":"` + locator + `","title":"Sky colour decision"}]}`, FinishReason: "stop"},
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
	must.NoError(err, "research --agent worker, stderr: %s", stderr)

	reqs := kbSrv.Requests()
	must.Len(reqs, 1, "alexandria requests")
	check.Equal("Bearer alx_test-token", reqs[0].Authorization, "recall bearer")
	check.Equal("notes", reqs[0].Query.Get("space"), "recall space")
	check.Equal("sky colour", reqs[0].Query.Get("q"), "recall query")
	check.Contains(stdout, "[Sky colour decision]("+locator+")", "report lacks the fragment locator as a source")
	check.NotContains(stdout+stderr, "alx_test-token", "the knowledge token leaked into the output")
}
