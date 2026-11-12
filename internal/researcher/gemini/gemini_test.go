package gemini

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/interactions"
	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/types"
)

const testKey = "test-key"

// fastPoll keeps tests quick.
func fastPoll(o *Options) {
	o.PollInterval = time.Millisecond
	o.PollMaxInterval = time.Millisecond
}

func newResearcher(t *testing.T, server *httptest.Server, mutate ...func(*Options)) *Researcher {
	t.Helper()
	must := require.New(t)
	opts := Options{APIKey: testKey, BaseURL: server.URL}
	fastPoll(&opts)
	for _, m := range mutate {
		m(&opts)
	}
	r, err := New(opts)
	must.NoError(err)
	return r
}

func TestStartBuildsCreateRequest(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		check.True(req.Method == http.MethodPost && req.URL.Path == "/v1beta/interactions",
			"unexpected request: %s %s", req.Method, req.URL.Path)
		decodeJSONBody(t, req, &body)
		w.Write([]byte(`{"id":"v1_new","status":"in_progress"}`))
	}))
	defer server.Close()

	r := newResearcher(t, server, func(o *Options) {
		o.Visualise = true
		o.MCP = map[string]string{"docs": "https://mcp.example/docs"}
		o.FileSearch = []string{"fileSearchStores/corpus-1"}
	})
	id, err := r.Start(context.Background(), researcher.Task{Query: "Why is the sky blue?"})
	must.NoError(err)
	check.Equal("v1_new", id)

	check.Equal(interactions.AgentDeepResearch, body["agent"], "agent is the deep-research wire id")
	check.Equal(true, body["background"])
	check.Equal(true, body["store"], "background requires store")
	check.Equal(false, body["stream"], "polling mode is non-streaming")

	cfg, _ := body["agent_config"].(map[string]any)
	must.NotNil(cfg, "agent_config missing")
	check.Equal("deep-research", cfg["type"])
	check.Equal("none", cfg["thinking_summaries"], "a polling run takes no thinking summaries")
	check.Equal("auto", cfg["visualization"], "visualisation is auto with --visualise")
	check.Equal(false, cfg["collaborative_planning"])

	// The default tool trio comes first, then MCP, then file_search.
	tools, _ := body["tools"].([]any)
	must.Len(tools, 5, "tools = %v", tools)
	wantTypes := []string{"google_search", "url_context", "code_execution", "mcp_server", "file_search"}
	for i, want := range wantTypes {
		t.Run(want, func(t *testing.T) {
			must := require.New(t)
			entry := tools[i].(map[string]any)
			must.Equal(want, entry["type"])
		})
	}
	mcp := tools[3].(map[string]any)
	check.Equal("docs", mcp["name"])
	check.Equal("https://mcp.example/docs", mcp["url"])

	// The prompt is templated and carries the query plus the visualise nudge.
	prompt, _ := body["input"].(string)
	check.Contains(prompt, "Why is the sky blue?", "input must carry the query")
	check.Contains(prompt, "## Conclusion", "input must carry the built-in template's steering")
	check.Contains(prompt, "visualisations", "input must carry the --visualise nudge")
}

func TestEndToEndCompletedMapsWireToDomain(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var gets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			w.Write([]byte(`{"id":"v1_e2e","status":"in_progress","created":"2026-06-07T12:00:00Z"}`))
			return
		}
		if gets.Add(1) == 1 {
			w.Write([]byte(`{"id":"v1_e2e","status":"in_progress","created":"2026-06-07T12:00:00Z"}`))
			return
		}
		w.Write([]byte(`{
			"id": "v1_e2e",
			"agent": "deep-research-preview-04-2026",
			"status": "completed",
			"created": "2026-06-07T12:00:00Z",
			"updated": "2026-06-07T12:30:00Z",
			"steps": [
				{"type": "user_input", "content": [{"type": "text", "text": "prompt"}]},
				{"type": "model_output", "content": [
					{"type": "thought", "text": "thinking..."},
					{"type": "image", "mime_type": "image/png", "data": "` + base64.StdEncoding.EncodeToString([]byte("chart-bytes")) + `"},
					{"type": "text", "text": "# Report body", "annotations": [
						{"type": "url_citation", "url": "https://example.org/a", "title": "Source A"},
						{"type": "url_citation", "url": "https://example.org/a", "title": "Source A duplicate"},
						{"type": "url_citation", "url": "https://example.org/b", "title": "Source B"}
					]}
				]}
			],
			"usage": {
				"total_input_tokens": 250000,
				"total_cached_tokens": 150000,
				"total_output_tokens": 60000,
				"total_tool_use_tokens": 1000,
				"total_thought_tokens": 9000,
				"grounding_tool_count": [
					{"type": "google_search", "count": 80},
					{"type": "url_context", "count": 12}
				]
			}
		}`))
	}))
	defer server.Close()

	r := newResearcher(t, server)
	ctx := context.Background()
	id, err := r.Start(ctx, researcher.Task{Query: "q"})
	must.NoError(err)
	must.NoError(r.Await(ctx, id))
	in, err := r.Result(ctx, id)
	must.NoError(err)

	check.Equal("v1_e2e", in.ID)
	check.Equal(types.StatusCompleted, in.Status)
	check.Equal("deep-research-preview-04-2026", in.Agent)
	check.Equal("q", in.Query, "the query recorded at Start")
	check.Empty(in.StatusDetail, "a completed interaction has no status detail")
	check.False(in.CreatedAt.IsZero())
	check.Equal(12, in.CompletedAt.Hour())
	check.Equal(30, in.CompletedAt.Minute())

	check.Equal([]string{"google_search", "url_context", "code_execution"}, in.Tools)

	// Outputs: thought summaries first, then the decoded chart image, then the final text.
	must.Len(in.Outputs, 3, "outputs = %+v, want thought + image + final text", in.Outputs)
	check.Equal(types.OutputThoughtSummary, in.Outputs[0].Type)
	check.Equal("thinking...", in.Outputs[0].Text)
	check.Equal(types.OutputImage, in.Outputs[1].Type)
	check.Equal("chart-bytes", string(in.Outputs[1].Data), "image data is decoded from base64")
	check.Equal(types.OutputText, in.Outputs[2].Type)
	check.Equal("# Report body", in.Outputs[2].Text)

	// Citations are deduplicated by URI in first-seen order.
	check.Equal([]types.Citation{
		{URI: "https://example.org/a", Title: "Source A"},
		{URI: "https://example.org/b", Title: "Source B"},
	}, in.Citations)

	// Usage: token totals, google_search count only, poll count, estimate.
	u := in.Usage
	check.Equal(250000, u.InputTokens)
	check.Equal(150000, u.CachedTokens)
	check.Equal(60000, u.OutputTokens)
	check.Equal(1000, u.ToolUseTokens)
	check.Equal(9000, u.ThoughtTokens)
	check.Equal(80, u.SearchCount, "google_search only, not url_context")
	check.Equal(2, u.PollCount)
	check.InDelta(2.00*0.79, u.EstimatedCostGBP, 0)
}

func TestResultWithoutStartRecordsNoToolsAndDerivesEstimate(t *testing.T) {
	// The API does not echo the query or tool set of an interaction this adapter
	// never started (chiron get), so neither is recorded. The estimate is derived
	// from the wire agent id instead.
	must := require.New(t)
	check := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte(`{
			"id": "v1_resumed",
			"agent": "deep-research-max-preview-04-2026",
			"status": "completed",
			"steps": [{"type": "model_output", "content": [{"type": "text", "text": "# Report"}]}]
		}`))
	}))
	defer server.Close()

	r := newResearcher(t, server)
	in, err := r.Result(context.Background(), "v1_resumed")
	must.NoError(err)
	check.Nil(in.Tools, "no tools recorded for a resumed interaction")
	check.Empty(in.Query, "no query recorded for a resumed interaction")
	check.InDelta(estimatedCostGBP(TierDeepResearchMax), in.Usage.EstimatedCostGBP, 0,
		"estimate derived from the max-tier wire agent id")
}

func TestFailedInteractionCarriesDetail(t *testing.T) {
	// Each failure status has its own detail text in the status_detail front
	// matter of its placeholder report. Pinning every text makes a transposition
	// fail, not only a missing detail.
	cases := []struct {
		name           string
		status         string
		domain         types.Status
		wantDetail     string
		requiresAction bool
	}{
		{"failed", "failed", types.StatusFailed, "failed server-side", false},
		{"cancelled", "cancelled", types.StatusCancelled, "was cancelled", false},
		{"incomplete", "incomplete", types.StatusIncomplete, "ended incomplete", false},
		{"budget exceeded", "budget_exceeded", types.StatusBudgetExceeded, "budget was exceeded", false},
		{"requires action", "requires_action", types.StatusRequiresAction, "requires client action", true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte(`{"id":"v1_fail","status":"` + tt.status + `","created":"2026-06-07T12:00:00Z","updated":"2026-06-07T12:05:00Z"}`))
			}))
			defer server.Close()

			r := newResearcher(t, server)
			ctx := context.Background()
			err := r.Await(ctx, "v1_fail")
			if tt.requiresAction {
				// requires_action concludes the await with the typed
				// error; the Result fetch still maps the detail.
				must.ErrorIs(err, interactions.ErrRequiresAction)
			} else {
				must.NoError(err, "%s is terminal, not an await error", tt.status)
			}

			in, err := r.Result(ctx, "v1_fail")
			must.NoError(err)
			must.Equal(tt.domain, in.Status)
			must.Contains(in.StatusDetail, tt.wantDetail)
			if tt.status != "failed" {
				must.NotContains(in.StatusDetail, "failed server-side",
					"detail carries the failed wording, so details are transposed")
			}
			if in.Status.Terminal() {
				must.False(in.CompletedAt.IsZero(), "a terminal interaction must carry its completion stamp")
			}
		})
	}
}

func TestAwait_RequiresAction_ReturnsTypedError(t *testing.T) {
	must := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte(`{"id":"v1_ra","status":"requires_action"}`))
	}))
	defer server.Close()

	err := newResearcher(t, server).Await(context.Background(), "v1_ra")
	must.ErrorIs(err, interactions.ErrRequiresAction)
}

func TestCreateIsNeverRetried(t *testing.T) {
	// A 5xx on create is ambiguous: the server may have accepted — and
	// started paying for — the task. The create client must surface the
	// failure after exactly one attempt.
	must := require.New(t)
	var creates atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		creates.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := newResearcher(t, server).Start(context.Background(), researcher.Task{Query: "q"})
	must.Error(err, "Start must fail on a 5xx create")
	must.EqualValues(1, creates.Load(), "a retry risks duplicate spend")
}

func TestResult_RetriesTransientFailure(t *testing.T) {
	// GET is idempotent: transient failures retry within the client.
	must := require.New(t)
	var gets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if gets.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"id":"v1_retry","status":"completed"}`))
	}))
	defer server.Close()

	in, err := newResearcher(t, server).Result(context.Background(), "v1_retry")
	must.NoError(err)
	must.Equal(types.StatusCompleted, in.Status, "after a retried 503")
	must.EqualValues(2, gets.Load(), "one 503, then one successful GET")
}

func TestMultimodalInputParts(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	dir := t.TempDir()
	imgPath := filepath.Join(dir, "photo.png")
	must.NoError(os.WriteFile(imgPath, []byte("png-data"), 0o644))

	var body struct {
		Input []interactions.Content `json:"input"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		decodeJSONBody(t, req, &body)
		w.Write([]byte(`{"id":"v1_mm","status":"in_progress"}`))
	}))
	defer server.Close()

	r := newResearcher(t, server, func(o *Options) {
		o.Inputs = []string{imgPath, "https://example.org/spec.pdf"}
	})
	_, err := r.Start(context.Background(), researcher.Task{Query: "q"})
	must.NoError(err)

	must.Len(body.Input, 3, "input parts = %+v, want prompt + 2 grounding parts", body.Input)
	check.Equal(interactions.ContentText, body.Input[0].Type)
	check.Contains(body.Input[0].Text, "q", "first part is the prompt text")

	img := body.Input[1]
	check.Equal(interactions.ContentImage, img.Type)
	check.Equal("image/png", img.MIMEType)
	check.Equal("png-data", string(img.Data), "a local file is sent as image data")
	check.Empty(img.URI)

	doc := body.Input[2]
	check.Equal(interactions.ContentDocument, doc.Type)
	check.Equal("application/pdf", doc.MIMEType)
	check.Equal("https://example.org/spec.pdf", doc.URI, "a URL is sent as a document reference")
	check.Nil(doc.Data)
}

func TestCustomTemplate(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	tmplPath := filepath.Join(dir, "tmpl.md")
	must.NoError(os.WriteFile(tmplPath, []byte("Custom steering.\n\nQ: {{.Query}}\n"), 0o644))

	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		decodeJSONBody(t, req, &body)
		w.Write([]byte(`{"id":"v1_tmpl","status":"in_progress"}`))
	}))
	defer server.Close()

	r := newResearcher(t, server, func(o *Options) { o.TemplatePath = tmplPath })
	_, err := r.Start(context.Background(), researcher.Task{Query: "the question"})
	must.NoError(err)
	prompt, _ := body["input"].(string)
	must.True(strings.HasPrefix(prompt, "Custom steering."), "prompt = %q, want the custom template applied", prompt)
	must.Contains(prompt, "Q: the question")
}

// TestNew_TemplateWithoutQuery_Rejected: a template that never prints the
// query is refused at construction, whatever query would be passed later and
// without any request reaching the server.
func TestNew_TemplateWithoutQuery_Rejected(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	tmplPath := filepath.Join(t.TempDir(), "bad.md")
	must.NoError(os.WriteFile(tmplPath, []byte("No query placeholder here.\n"), 0o644))

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		check.Fail("no request must be sent for a template that drops the query")
	}))
	defer server.Close()

	_, err := New(Options{APIKey: testKey, BaseURL: server.URL, TemplatePath: tmplPath})
	must.ErrorContains(err, "{{.Query}}")
	must.Zero(requests.Load(), "requests that reached the server")
}

// TestStart_TemplateUsingQueryConditionally_Rejected: the per-render check
// still refuses a prompt that lacks the query when only some queries make the
// template print it.
func TestStart_TemplateUsingQueryConditionally_Rejected(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	tmplPath := filepath.Join(t.TempDir(), "conditional.md")
	text := `{{if eq .Query "` + queryProbe + `"}}Q: {{.Query}}{{else}}Boilerplate only.{{end}}`
	must.NoError(os.WriteFile(tmplPath, []byte(text), 0o644))

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		check.Fail("no request must be sent for a prompt that drops the query")
	}))
	defer server.Close()

	r := newResearcher(t, server, func(o *Options) { o.TemplatePath = tmplPath })
	_, err := r.Start(context.Background(), researcher.Task{Query: "q"})
	must.ErrorContains(err, "{{.Query}}")
	must.Zero(requests.Load(), "requests that reached the server")
}

func TestConstructionValidation(t *testing.T) {
	must := require.New(t)
	noQueryTemplate := filepath.Join(t.TempDir(), "no-query.md")
	must.NoError(os.WriteFile(noQueryTemplate, []byte("No query placeholder here.\n"), 0o644))

	cases := []struct {
		name string
		opts Options
	}{
		{"empty api key", Options{}},
		{"unknown tier", Options{APIKey: testKey, Tier: "deep-research-ultra"}},
		{"unsupported tool", Options{APIKey: testKey, Tools: []string{"function"}}},
		{"mcp via --tools", Options{APIKey: testKey, Tools: []string{"mcp_server"}}},
		{"missing template", Options{APIKey: testKey, TemplatePath: "/does/not/exist.md"}},
		{"template without query", Options{APIKey: testKey, TemplatePath: noQueryTemplate}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			_, err := New(tt.opts)
			must.Error(err, "New must reject the configuration before any request is built")
		})
	}
}

func TestTierMapping(t *testing.T) {
	must := require.New(t)
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		decodeJSONBody(t, req, &body)
		w.Write([]byte(`{"id":"v1_max","status":"in_progress"}`))
	}))
	defer server.Close()

	r := newResearcher(t, server, func(o *Options) { o.Tier = TierDeepResearchMax })
	_, err := r.Start(context.Background(), researcher.Task{Query: "q"})
	must.NoError(err)
	must.Equal(interactions.AgentDeepResearchMax, body["agent"], "max tier maps to its wire id")
	must.InDelta(5.00*0.79, r.estimate, 0, "max tier uses its planning figure")
}

func TestStart_EmptyQuery_Rejected(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		check.Fail("no request must be sent for an empty query")
	}))
	defer server.Close()

	r := newResearcher(t, server)
	_, err := r.Start(context.Background(), researcher.Task{})
	must.ErrorContains(err, "query must not be empty")
	must.Zero(requests.Load(), "requests that reached the server")
}
