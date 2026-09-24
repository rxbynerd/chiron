package search_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/mcpclient"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
)

const testKey = "sk-search-0123456789abcdefABCDEF"

// newClient builds a search.Client pointed at an endpoint, failing the test
// on a construction error.
func newClient(t *testing.T, endpoint string, mutate ...func(*search.Options)) *search.Client {
	t.Helper()
	opts := search.Options{Endpoint: endpoint, APIKey: testKey}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := search.New(opts)
	if err != nil {
		t.Fatalf("search.New: %v", err)
	}
	return c
}

func TestSearchHappyPath(t *testing.T) {
	want := []search.Result{
		{Title: "Rayleigh scattering", URL: "https://example.com/rayleigh", Snippet: "why the sky is blue"},
		{Title: "Atmospheric optics", URL: "https://example.com/optics", Snippet: "scattering of sunlight"},
	}
	fake := searchtest.NewFakeServer(want)
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := c.Search(context.Background(), "why is the sky blue")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The flow is initialize -> notifications/initialized -> tools/call, in
	// order, with no DELETE for a stateless server, and the key rides only the
	// Authorization header.
	reqs := fake.Requests()
	if len(reqs) != 3 {
		t.Fatalf("request count = %d, want 3 (initialize, initialized, tools/call)", len(reqs))
	}
	wantMethods := []string{"initialize", "notifications/initialized", "tools/call"}
	for i, m := range wantMethods {
		if reqs[i].HTTPMethod != http.MethodPost || reqs[i].Method != m {
			t.Errorf("request[%d] = %s %q, want POST %q", i, reqs[i].HTTPMethod, reqs[i].Method, m)
		}
		if reqs[i].Authorization != "Bearer "+testKey {
			t.Errorf("request[%d].Authorization = %q, want the bearer key header", i, reqs[i].Authorization)
		}
	}
	// tools/call carried the configured tool name and query argument.
	call := reqs[2]
	if call.ToolName != "search" {
		t.Errorf("tool name = %q, want search", call.ToolName)
	}
	if call.Arguments["query"] != "why is the sky blue" {
		t.Errorf("arguments = %+v, want query set", call.Arguments)
	}
}

func TestSearchCustomToolAndArgKey(t *testing.T) {
	fake := searchtest.NewFakeServer([]search.Result{{Title: "t", URL: "https://e.com", Snippet: "s"}})
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *search.Options) {
		o.ToolName = "web_search"
		o.QueryArgKey = "q"
	})
	if _, err := c.Search(context.Background(), "hello"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	call := fake.Requests()[2]
	if call.ToolName != "web_search" {
		t.Errorf("tool name = %q, want web_search", call.ToolName)
	}
	if call.Arguments["q"] != "hello" {
		t.Errorf("arguments = %+v, want q set", call.Arguments)
	}
}

func TestSearchSessionEchoedAndEnded(t *testing.T) {
	// A stateful server assigns a session on initialize and requires it on
	// tools/call; the client echoes it, then ends it with a DELETE.
	fake := searchtest.NewFakeServer(
		[]search.Result{{Title: "t", URL: "https://e.com", Snippet: "s"}},
		searchtest.WithSessionID("sess-abc-123"),
	)
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search with a session server: %v", err)
	}
	reqs := fake.Requests()
	if len(reqs) != 4 {
		t.Fatalf("request count = %d, want 4 (initialize, initialized, tools/call, DELETE)", len(reqs))
	}
	// initialize is sent without a session; every later request carries the
	// assigned one.
	if reqs[0].SessionID != "" {
		t.Errorf("initialize carried a session header %q, want none", reqs[0].SessionID)
	}
	for i := 1; i < len(reqs); i++ {
		if reqs[i].SessionID != "sess-abc-123" {
			t.Errorf("request[%d] session = %q, want the assigned session echoed", i, reqs[i].SessionID)
		}
	}
	end := reqs[3]
	if end.HTTPMethod != http.MethodDelete {
		t.Fatalf("last request = %s %q, want the session DELETE", end.HTTPMethod, end.Method)
	}
	if end.Authorization != "Bearer "+testKey {
		t.Errorf("DELETE Authorization = %q, want the bearer key header", end.Authorization)
	}
	if end.ProtocolVersion != mcpclient.ProtocolVersion {
		t.Errorf("DELETE MCP-Protocol-Version = %q, want %q", end.ProtocolVersion, mcpclient.ProtocolVersion)
	}
}

func TestSearchSessionEndedAfterToolError(t *testing.T) {
	// The session is ended even when the search itself fails.
	fake := searchtest.NewFakeServer(nil,
		searchtest.WithSessionID("sess-err"),
		searchtest.WithRawToolResult(`{"content":[{"type":"text","text":"upstream quota exceeded"}],"isError":true}`),
	)
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.Search(context.Background(), "q"); err == nil {
		t.Fatal("Search should fail when the tool reports isError")
	}
	reqs := fake.Requests()
	if last := reqs[len(reqs)-1]; last.HTTPMethod != http.MethodDelete || last.SessionID != "sess-err" {
		t.Errorf("last request = %s with session %q, want a DELETE of sess-err", last.HTTPMethod, last.SessionID)
	}
}

func TestSearchProtocolVersionHeader(t *testing.T) {
	// Every request after initialize carries the revision the server chose.
	fake := searchtest.NewFakeServer(
		[]search.Result{{Title: "t", URL: "https://e.com", Snippet: "s"}},
		searchtest.WithProtocolVersion("2025-03-26"),
	)
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	reqs := fake.Requests()
	if reqs[0].ProtocolVersion != "" {
		t.Errorf("initialize carried MCP-Protocol-Version %q, want none", reqs[0].ProtocolVersion)
	}
	for i := 1; i < len(reqs); i++ {
		if reqs[i].ProtocolVersion != "2025-03-26" {
			t.Errorf("request[%d] (%s) MCP-Protocol-Version = %q, want the negotiated 2025-03-26", i, reqs[i].Method, reqs[i].ProtocolVersion)
		}
	}
}

func TestSearchOverSSE(t *testing.T) {
	// The tools/call reply may be a text/event-stream frame carrying the
	// JSON-RPC response; the client reads it under the same bound.
	want := []search.Result{{Title: "SSE result", URL: "https://e.com/sse", Snippet: "streamed"}}
	fake := searchtest.NewFakeServer(want, searchtest.WithSSE())
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := c.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search over SSE: %v", err)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSearchStructuredContent(t *testing.T) {
	// The results document may arrive as structuredContent rather than inside
	// a text block; the client prefers it.
	raw := `{"content":[],"structuredContent":{"results":[{"title":"S","url":"https://e.com/s","snippet":"struct"}]},"isError":false}`
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult(raw))
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := c.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].URL != "https://e.com/s" || got[0].Snippet != "struct" {
		t.Fatalf("structuredContent not parsed: %+v", got)
	}
}

func TestSearchGracefulDegradationOnUnexpectedShape(t *testing.T) {
	// A tool result that carries prose but not the expected results document
	// degrades to a single snippet rather than erroring.
	raw := `{"content":[{"type":"text","text":"I could not find structured results but here is a summary."}],"isError":false}`
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult(raw))
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := c.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search should degrade gracefully, got error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1 degraded snippet: %+v", len(got), got)
	}
	if got[0].Snippet != "I could not find structured results but here is a summary." {
		t.Errorf("degraded snippet = %q, want the text content surfaced", got[0].Snippet)
	}
	if got[0].URL != "" {
		t.Errorf("degraded result URL = %q, want empty", got[0].URL)
	}
}

func TestSearchEmptyResultsIsSuccess(t *testing.T) {
	// A results document with an empty array is a valid zero-hit success, not
	// a degradation to a text snippet.
	raw := `{"content":[{"type":"text","text":"{\"results\":[]}"}],"isError":false}`
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult(raw))
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := c.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d results, want 0 for an empty results document: %+v", len(got), got)
	}
}

func TestSearchToolError(t *testing.T) {
	// isError:true is a tool-level failure the client surfaces as an error.
	raw := `{"content":[{"type":"text","text":"upstream search quota exceeded"}],"isError":true}`
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult(raw))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search should fail when the tool reports isError")
	}
	if !strings.Contains(err.Error(), "quota") {
		t.Errorf("error = %v, want the tool error surfaced", err)
	}
}

func TestSearchToolCallNotRetried(t *testing.T) {
	// tools/call may invoke a billable upstream search; it is single-attempt.
	// A tools/call reply that cannot be decoded must not be retried.
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult("this is not json"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, _ = c.Search(context.Background(), "q")
	if n := fake.ToolCallCount(); n != 1 {
		t.Errorf("tools/call count = %d, want exactly 1 — the billable call must not be retried", n)
	}
}

func TestSearchOversizedBodyBounded(t *testing.T) {
	// A tools/call result over the bound is an error, not a truncation.
	big := strings.Repeat("x", 4096)
	raw := fmt.Sprintf(`{"content":[{"type":"text","text":%q}],"isError":false}`, big)
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult(raw))
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *search.Options) { o.MaxBodyBytes = 512 })
	_, err := c.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search should fail when the response exceeds MaxBodyBytes")
	}
	if !strings.Contains(err.Error(), "exceed") && !strings.Contains(err.Error(), "bound") {
		t.Errorf("error = %v, want a body-bound error", err)
	}
}

func TestSearchOversizedSSEBounded(t *testing.T) {
	// The SSE read path is bounded too: a large streamed frame is rejected.
	big := strings.Repeat("y", 4096)
	raw := fmt.Sprintf(`{"content":[{"type":"text","text":%q}],"isError":false}`, big)
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult(raw), searchtest.WithSSE())
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *search.Options) { o.MaxBodyBytes = 512 })
	_, err := c.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search over SSE should fail when the stream exceeds MaxBodyBytes")
	}
	if !strings.Contains(err.Error(), "exceed") && !strings.Contains(err.Error(), "bound") {
		t.Errorf("error = %v, want a body-bound error", err)
	}
}

func TestSearchEndpointValidation(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{"https ok", "https://search.example.com/mcp", false},
		{"http loopback ip ok", "http://127.0.0.1:8080/mcp", false},
		{"http loopback ipv6 ok", "http://[::1]:8080/mcp", false},
		{"http localhost ok", "http://localhost:8080/mcp", false},
		{"http non-loopback rejected", "http://search.example.com/mcp", true},
		{"non-absolute rejected", "/mcp", true},
		{"empty rejected", "", true},
		{"garbage rejected", "://not a url", true},
		{"ftp scheme rejected", "ftp://example.com", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := search.New(search.Options{Endpoint: tt.endpoint, APIKey: testKey})
			if tt.wantErr && err == nil {
				t.Errorf("search.New(%q) succeeded, want an error", tt.endpoint)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("search.New(%q) = %v, want success", tt.endpoint, err)
			}
			if err != nil && !strings.HasPrefix(err.Error(), "search: ") {
				t.Errorf("error = %v, want the search: prefix", err)
			}
		})
	}
}

func TestSearchErrorNeverLeaksKey(t *testing.T) {
	// A failing request must not surface the API key anywhere in the error,
	// even when the server echoes request context in its error body.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key ` + testKey + `"}`))
	}))
	defer fake.Close()

	c := newClient(t, fake.URL)
	_, err := c.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search should fail on a 401")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("error leaked the API key: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "search: ") || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("error = %v, want the transport failure under the search: prefix", err)
	}
}

func TestSearchEmptyQueryRejected(t *testing.T) {
	fake := searchtest.NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.Search(context.Background(), ""); err == nil {
		t.Fatal("Search with an empty query should fail")
	}
	if fake.CallCount() != 0 {
		t.Errorf("call count = %d, want 0 — validation must precede any request", fake.CallCount())
	}
}

func TestSearchKeylessServer(t *testing.T) {
	// A server needing no key: no Authorization header is sent.
	fake := searchtest.NewFakeServer([]search.Result{{Title: "t", URL: "https://e.com", Snippet: "s"}})
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *search.Options) { o.APIKey = "" })
	if _, err := c.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	for i, req := range fake.Requests() {
		if req.Authorization != "" {
			t.Errorf("request[%d] carried Authorization %q on a keyless client", i, req.Authorization)
		}
	}
}
