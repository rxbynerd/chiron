package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	opts := Options{APIKey: testKey, BaseURL: server.URL}
	fastPoll(&opts)
	for _, m := range mutate {
		m(&opts)
	}
	r, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestStartBuildsCreateRequest(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/v1beta/interactions" {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decoding create body: %v", err)
		}
		w.Write([]byte(`{"id":"v1_new","status":"in_progress"}`))
	}))
	defer server.Close()

	r := newResearcher(t, server, func(o *Options) {
		o.Visualise = true
		o.MCP = map[string]string{"docs": "https://mcp.example/docs"}
		o.FileSearch = []string{"fileSearchStores/corpus-1"}
	})
	id, err := r.Start(context.Background(), researcher.Task{Query: "Why is the sky blue?"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id != "v1_new" {
		t.Errorf("id = %q, want v1_new", id)
	}

	if got := body["agent"]; got != interactions.AgentDeepResearch {
		t.Errorf("agent = %v, want the deep-research wire id", got)
	}
	if got, want := body["background"], true; got != want {
		t.Errorf("background = %v, want true", got)
	}
	if got, want := body["store"], true; got != want {
		t.Errorf("store = %v, want true — background requires it", got)
	}
	if got, want := body["stream"], false; got != want {
		t.Errorf("stream = %v, want false in polling mode", got)
	}

	cfg, _ := body["agent_config"].(map[string]any)
	if cfg == nil {
		t.Fatal("agent_config missing")
	}
	if cfg["type"] != "deep-research" {
		t.Errorf("agent_config.type = %v", cfg["type"])
	}
	if cfg["thinking_summaries"] != "none" {
		t.Errorf("thinking_summaries = %v, want none for a polling run", cfg["thinking_summaries"])
	}
	if cfg["visualization"] != "auto" {
		t.Errorf("visualization = %v, want auto with --visualise", cfg["visualization"])
	}
	if cfg["collaborative_planning"] != false {
		t.Errorf("collaborative_planning = %v, want false (M4 owns planning)", cfg["collaborative_planning"])
	}

	// Default tool trio, then MCP, then file_search — explicit and ordered.
	tools, _ := body["tools"].([]any)
	if len(tools) != 5 {
		t.Fatalf("tools = %v, want 5 entries", tools)
	}
	for i, want := range []string{"google_search", "url_context", "code_execution", "mcp_server", "file_search"} {
		entry := tools[i].(map[string]any)
		if entry["type"] != want {
			t.Errorf("tools[%d].type = %v, want %s", i, entry["type"], want)
		}
	}
	mcp := tools[3].(map[string]any)
	if mcp["name"] != "docs" || mcp["url"] != "https://mcp.example/docs" {
		t.Errorf("mcp tool = %v", mcp)
	}

	// The prompt is templated and carries the query plus the visualise nudge.
	prompt, _ := body["input"].(string)
	if !strings.Contains(prompt, "Why is the sky blue?") {
		t.Errorf("input does not contain the query: %q", prompt)
	}
	if !strings.Contains(prompt, "## Conclusion") {
		t.Errorf("input does not show the built-in template's steering: %q", prompt)
	}
	if !strings.Contains(prompt, "visualisations") {
		t.Errorf("input lacks the --visualise nudge: %q", prompt)
	}
}

func TestEndToEndCompletedMapsWireToDomain(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Await(ctx, id); err != nil {
		t.Fatalf("Await: %v", err)
	}
	in, err := r.Result(ctx, id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}

	if in.ID != "v1_e2e" || in.Status != types.StatusCompleted {
		t.Errorf("interaction = %s/%s, want completed v1_e2e", in.ID, in.Status)
	}
	if in.Agent != "deep-research-preview-04-2026" {
		t.Errorf("agent = %q", in.Agent)
	}
	if in.Query != "q" {
		t.Errorf("query = %q, want the raw query recorded at Start", in.Query)
	}
	if in.StatusDetail != "" {
		t.Errorf("status detail = %q, want empty for completed", in.StatusDetail)
	}
	if in.CreatedAt.IsZero() || in.CompletedAt.Hour() != 12 || in.CompletedAt.Minute() != 30 {
		t.Errorf("timestamps = %v / %v", in.CreatedAt, in.CompletedAt)
	}

	wantTools := []string{"google_search", "url_context", "code_execution"}
	if !reflect.DeepEqual(in.Tools, wantTools) {
		t.Errorf("tools = %v, want %v", in.Tools, wantTools)
	}

	// Outputs: the decoded chart image plus the final text, thoughts excluded.
	if len(in.Outputs) != 2 {
		t.Fatalf("outputs = %+v, want image + final text", in.Outputs)
	}
	if in.Outputs[0].Type != types.OutputImage || string(in.Outputs[0].Data) != "chart-bytes" {
		t.Errorf("image output = %+v, want decoded chart bytes", in.Outputs[0])
	}
	if in.Outputs[1].Type != types.OutputText || in.Outputs[1].Text != "# Report body" {
		t.Errorf("text output = %+v", in.Outputs[1])
	}

	// Citations deduplicated by URI in first-seen order.
	wantCites := []types.Citation{
		{URI: "https://example.org/a", Title: "Source A"},
		{URI: "https://example.org/b", Title: "Source B"},
	}
	if !reflect.DeepEqual(in.Citations, wantCites) {
		t.Errorf("citations = %+v, want %+v", in.Citations, wantCites)
	}

	// Usage: token totals, google_search count only, poll count, estimate.
	u := in.Usage
	if u.InputTokens != 250000 || u.CachedTokens != 150000 || u.OutputTokens != 60000 ||
		u.ToolUseTokens != 1000 || u.ThoughtTokens != 9000 {
		t.Errorf("token usage = %+v", u)
	}
	if u.SearchCount != 80 {
		t.Errorf("search count = %d, want 80 (google_search only, not url_context)", u.SearchCount)
	}
	if u.PollCount != 2 {
		t.Errorf("poll count = %d, want 2", u.PollCount)
	}
	if want := 2.00 * 0.79; u.EstimatedCostGBP != want {
		t.Errorf("estimated cost = %v, want %v", u.EstimatedCostGBP, want)
	}
}

func TestResultWithoutStartRecordsNoToolsAndDerivesEstimate(t *testing.T) {
	// chiron get resumes an interaction this adapter never started: the
	// query and tool set of the original create are unknowable (the API
	// does not echo them), so the domain model must not claim them —
	// the recorded set must be the used set. The estimate falls back to
	// the wire agent id, here the max tier despite the adapter being
	// configured for the default tier.
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
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Tools != nil {
		t.Errorf("tools = %v, want none recorded for a resumed interaction", in.Tools)
	}
	if in.Query != "" {
		t.Errorf("query = %q, want empty for a resumed interaction", in.Query)
	}
	if want := estimatedCostGBP(TierDeepResearchMax); in.Usage.EstimatedCostGBP != want {
		t.Errorf("estimate = %v, want %v derived from the wire agent id", in.Usage.EstimatedCostGBP, want)
	}
}

func TestFailedInteractionCarriesDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte(`{"id":"v1_fail","status":"failed","created":"2026-06-07T12:00:00Z","updated":"2026-06-07T12:05:00Z"}`))
	}))
	defer server.Close()

	r := newResearcher(t, server)
	ctx := context.Background()
	if err := r.Await(ctx, "v1_fail"); err != nil {
		t.Fatalf("Await: a failed status is terminal, not an await error: %v", err)
	}
	in, err := r.Result(ctx, "v1_fail")
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Status != types.StatusFailed {
		t.Errorf("status = %s, want failed", in.Status)
	}
	if in.StatusDetail == "" {
		t.Error("a failure variant must carry a human-readable detail")
	}
	if in.CompletedAt.IsZero() {
		t.Error("a terminal interaction must carry its completion stamp")
	}
}

func TestRequiresActionSurfacesTypedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte(`{"id":"v1_ra","status":"requires_action"}`))
	}))
	defer server.Close()

	err := newResearcher(t, server).Await(context.Background(), "v1_ra")
	if !errors.Is(err, interactions.ErrRequiresAction) {
		t.Errorf("error = %v, want interactions.ErrRequiresAction", err)
	}
}

func TestCreateIsNeverRetried(t *testing.T) {
	// A 5xx on create is ambiguous: the server may have accepted — and
	// started paying for — the task. The create client must surface the
	// failure after exactly one attempt.
	var creates atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		creates.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := newResearcher(t, server).Start(context.Background(), researcher.Task{Query: "q"})
	if err == nil {
		t.Fatal("Start must fail on a 5xx create")
	}
	if got := creates.Load(); got != 1 {
		t.Errorf("create attempts = %d, want exactly 1 — a retry risks duplicate spend", got)
	}
}

func TestGetIsRetried(t *testing.T) {
	// GET is idempotent: transient failures retry within the client.
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
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Status != types.StatusCompleted || gets.Load() != 2 {
		t.Errorf("status %s after %d attempts, want completed after a retried 503", in.Status, gets.Load())
	}
}

func TestMultimodalInputParts(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "photo.png")
	if err := os.WriteFile(imgPath, []byte("png-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	var body struct {
		Input []interactions.Content `json:"input"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decoding create body: %v", err)
		}
		w.Write([]byte(`{"id":"v1_mm","status":"in_progress"}`))
	}))
	defer server.Close()

	r := newResearcher(t, server, func(o *Options) {
		o.Inputs = []string{imgPath, "https://example.org/spec.pdf"}
	})
	if _, err := r.Start(context.Background(), researcher.Task{Query: "q"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if len(body.Input) != 3 {
		t.Fatalf("input parts = %+v, want prompt + 2 grounding parts", body.Input)
	}
	if body.Input[0].Type != interactions.ContentText || !strings.Contains(body.Input[0].Text, "q") {
		t.Errorf("first part = %+v, want the prompt text", body.Input[0])
	}
	img := body.Input[1]
	if img.Type != interactions.ContentImage || img.MIMEType != "image/png" || string(img.Data) != "png-data" || img.URI != "" {
		t.Errorf("local file part = %+v, want base64 image data", img)
	}
	doc := body.Input[2]
	if doc.Type != interactions.ContentDocument || doc.MIMEType != "application/pdf" || doc.URI != "https://example.org/spec.pdf" || doc.Data != nil {
		t.Errorf("url part = %+v, want a document uri reference", doc)
	}
}

func TestCustomTemplate(t *testing.T) {
	dir := t.TempDir()
	tmplPath := filepath.Join(dir, "tmpl.md")
	if err := os.WriteFile(tmplPath, []byte("Custom steering.\n\nQ: {{.Query}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		json.NewDecoder(req.Body).Decode(&body)
		w.Write([]byte(`{"id":"v1_tmpl","status":"in_progress"}`))
	}))
	defer server.Close()

	r := newResearcher(t, server, func(o *Options) { o.TemplatePath = tmplPath })
	if _, err := r.Start(context.Background(), researcher.Task{Query: "the question"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	prompt, _ := body["input"].(string)
	if !strings.HasPrefix(prompt, "Custom steering.") || !strings.Contains(prompt, "Q: the question") {
		t.Errorf("prompt = %q, want the custom template applied", prompt)
	}
}

func TestTemplateWithoutQueryRejected(t *testing.T) {
	dir := t.TempDir()
	tmplPath := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(tmplPath, []byte("No query placeholder here.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{APIKey: testKey, TemplatePath: tmplPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.Start(context.Background(), researcher.Task{Query: "q"}); err == nil {
		t.Error("a template that drops the query must be rejected before money is spent")
	}
}

func TestConstructionValidation(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"empty api key", Options{}},
		{"unknown tier", Options{APIKey: testKey, Tier: "deep-research-ultra"}},
		{"unsupported tool", Options{APIKey: testKey, Tools: []string{"function"}}},
		{"mcp via --tools", Options{APIKey: testKey, Tools: []string{"mcp_server"}}},
		{"missing template", Options{APIKey: testKey, TemplatePath: "/does/not/exist.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); err == nil {
				t.Error("New must reject the configuration before any request is built")
			}
		})
	}
}

func TestTierMapping(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		json.NewDecoder(req.Body).Decode(&body)
		w.Write([]byte(`{"id":"v1_max","status":"in_progress"}`))
	}))
	defer server.Close()

	r := newResearcher(t, server, func(o *Options) { o.Tier = TierDeepResearchMax })
	if _, err := r.Start(context.Background(), researcher.Task{Query: "q"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if body["agent"] != interactions.AgentDeepResearchMax {
		t.Errorf("agent = %v, want the max-tier wire id", body["agent"])
	}
	if r.estimate != 5.00*0.79 {
		t.Errorf("estimate = %v, want the max-tier planning figure", r.estimate)
	}
}

func TestEmptyQueryRejected(t *testing.T) {
	r, err := New(Options{APIKey: testKey})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.Start(context.Background(), researcher.Task{}); err == nil {
		t.Error("an empty query must be rejected locally")
	}
}
