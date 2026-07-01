package search

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testKey = "sk-search-0123456789abcdefABCDEF"

// newClient builds a Client pointed at an endpoint, failing the test on a
// construction error.
func newClient(t *testing.T, endpoint string, mutate ...func(*Options)) *Client {
	t.Helper()
	opts := Options{Endpoint: endpoint, APIKey: testKey}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestSearchHappyPath(t *testing.T) {
	want := []Result{
		{Title: "Rayleigh scattering", URL: "https://example.com/rayleigh", Snippet: "why the sky is blue"},
		{Title: "Atmospheric optics", URL: "https://example.com/optics", Snippet: "scattering of sunlight"},
	}
	fake := NewFakeServer(want)
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
	// order, and the key rides only the Authorization header.
	reqs := fake.Requests()
	if len(reqs) != 3 {
		t.Fatalf("request count = %d, want 3 (initialize, initialized, tools/call)", len(reqs))
	}
	wantMethods := []string{"initialize", "notifications/initialized", "tools/call"}
	for i, m := range wantMethods {
		if reqs[i].Method != m {
			t.Errorf("request[%d].Method = %q, want %q", i, reqs[i].Method, m)
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
	fake := NewFakeServer([]Result{{Title: "t", URL: "https://e.com", Snippet: "s"}})
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *Options) {
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

func TestSearchSessionEchoed(t *testing.T) {
	// A stateful server assigns a session on initialize and requires it on
	// tools/call; the client must echo it.
	fake := NewFakeServer(
		[]Result{{Title: "t", URL: "https://e.com", Snippet: "s"}},
		WithSessionID("sess-abc-123"),
	)
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search with a session server: %v", err)
	}
	reqs := fake.Requests()
	// initialize is sent without a session; initialized and tools/call carry
	// the assigned one.
	if reqs[0].SessionID != "" {
		t.Errorf("initialize carried a session header %q, want none", reqs[0].SessionID)
	}
	for _, i := range []int{1, 2} {
		if reqs[i].SessionID != "sess-abc-123" {
			t.Errorf("request[%d] session = %q, want the assigned session echoed", i, reqs[i].SessionID)
		}
	}
}

func TestSearchOverSSE(t *testing.T) {
	// The tools/call reply may be a text/event-stream frame carrying the
	// JSON-RPC response; the client reads it under the same bound.
	want := []Result{{Title: "SSE result", URL: "https://e.com/sse", Snippet: "streamed"}}
	fake := NewFakeServer(want, WithSSE())
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
	fake := NewFakeServer(nil, WithRawToolResult(raw))
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
	fake := NewFakeServer(nil, WithRawToolResult(raw))
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
	fake := NewFakeServer(nil, WithRawToolResult(raw))
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
	fake := NewFakeServer(nil, WithRawToolResult(raw))
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
	// A server error on tools/call must not be retried.
	fake := NewFakeServer(nil, WithRawToolResult("this is not json"))
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
	fake := NewFakeServer(nil, WithRawToolResult(raw))
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *Options) { o.MaxBodyBytes = 512 })
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
	fake := NewFakeServer(nil, WithRawToolResult(raw), WithSSE())
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *Options) { o.MaxBodyBytes = 512 })
	_, err := c.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search over SSE should fail when the stream exceeds MaxBodyBytes")
	}
	if !strings.Contains(err.Error(), "exceed") && !strings.Contains(err.Error(), "bound") {
		t.Errorf("error = %v, want a body-bound error", err)
	}
}

func TestSearchCrossHostRedirectRefused(t *testing.T) {
	// A credential-bearing client must not follow a redirect to another host —
	// that would hand the Authorization header to the target.
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("redirect target was called — the key would have leaked to %s", r.Host)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer elsewhere.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	c := newClient(t, redirector.URL)
	_, err := c.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search should fail rather than follow a cross-host redirect")
	}
	if !strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "cross-origin") {
		t.Errorf("error = %v, want a cross-host redirect refusal", err)
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
			_, err := New(Options{Endpoint: tt.endpoint, APIKey: testKey})
			if tt.wantErr && err == nil {
				t.Errorf("New(%q) succeeded, want an error", tt.endpoint)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("New(%q) = %v, want success", tt.endpoint, err)
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
}

func TestSearchEmptyQueryRejected(t *testing.T) {
	fake := NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.Search(context.Background(), ""); err == nil {
		t.Fatal("Search with an empty query should fail")
	}
	if fake.CallCount() != 0 {
		t.Errorf("call count = %d, want 0 — validation must precede any request", fake.CallCount())
	}
}

func TestSearchCallerContextDeadlineWins(t *testing.T) {
	// A tighter caller deadline must fire even though RequestTimeout is
	// generous — WithTimeout keeps whichever is sooner.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()

	c := newClient(t, server.URL, func(o *Options) { o.RequestTimeout = 10 * time.Second })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Search(ctx, "q")
	if err == nil {
		t.Fatal("Search should fail when the caller deadline fires")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("Search took %s — the caller's 100ms deadline should have won over the 10s RequestTimeout", elapsed)
	}
}

func TestSearchKeylessServer(t *testing.T) {
	// A server needing no key: no Authorization header is sent.
	fake := NewFakeServer([]Result{{Title: "t", URL: "https://e.com", Snippet: "s"}})
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *Options) { o.APIKey = "" })
	if _, err := c.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	for i, req := range fake.Requests() {
		if req.Authorization != "" {
			t.Errorf("request[%d] carried Authorization %q on a keyless client", i, req.Authorization)
		}
	}
}
