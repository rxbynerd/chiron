package search_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/mcpclient"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
)

const testKey = "sk-search-0123456789abcdefABCDEF"

// newClient builds a search.Client pointed at an endpoint, failing the test
// on a construction error.
func newClient(t *testing.T, endpoint string, mutate ...func(*search.Options)) *search.Client {
	t.Helper()
	must := require.New(t)
	opts := search.Options{Endpoint: endpoint, APIKey: testKey}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := search.New(opts)
	must.NoError(err)
	return c
}

// searchText runs one search for text and returns its results.
func searchText(c *search.Client, text string) ([]search.Result, error) {
	resp, err := c.Search(context.Background(), web.Query{Text: text})
	return resp.Results, err
}

func TestSearch_HappyPath_ReturnsResultsAndCallFlow(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	want := []search.Result{
		{Title: "Rayleigh scattering", URL: "https://example.com/rayleigh", Snippet: "why the sky is blue"},
		{Title: "Atmospheric optics", URL: "https://example.com/optics", Snippet: "scattering of sunlight"},
	}
	fake := searchtest.NewFakeServer(want)
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := searchText(c, "why is the sky blue")
	must.NoError(err)
	must.Len(got, len(want), "results: %+v", got)
	for i := range want {
		check.Equal(want[i], got[i], "result[%d]", i)
	}

	// The flow is initialize -> notifications/initialized -> tools/call, in
	// order, with no DELETE for a stateless server, and the key rides only the
	// Authorization header.
	reqs := fake.Requests()
	must.Len(reqs, 3, "initialize, initialized, tools/call")
	wantMethods := []string{"initialize", "notifications/initialized", "tools/call"}
	for i, m := range wantMethods {
		check.Equal(http.MethodPost, reqs[i].HTTPMethod, "request[%d]", i)
		check.Equal(m, reqs[i].Method, "request[%d]", i)
		check.Equal("Bearer "+testKey, reqs[i].Authorization, "request[%d]", i)
	}
	// tools/call carried the configured tool name and query argument.
	call := reqs[2]
	check.Equal("search", call.ToolName)
	check.Equal("why is the sky blue", call.Arguments["query"], "arguments: %+v", call.Arguments)
}

// TestSearch_NeverSendsObjective: the tool takes one query argument, so the
// objective stays local, and an MCP search reports no notes and no spend.
func TestSearch_NeverSendsObjective(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	fake := searchtest.NewFakeServer([]search.Result{{Title: "t", URL: "https://e.com", Snippet: "s"}})
	defer fake.Close()

	c := newClient(t, fake.URL())
	resp, err := c.Search(context.Background(), web.Query{Text: "hello", Objective: "the whole research question"})
	must.NoError(err)
	call := fake.Requests()[2]
	check.Len(call.Arguments, 1, "arguments: %+v", call.Arguments)
	check.Equal("hello", call.Arguments["query"])
	check.Nil(resp.Notes)
	check.Equal(web.Spend{}, resp.Spend)
}

func TestSearch_CustomToolAndQueryArgKey(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	fake := searchtest.NewFakeServer([]search.Result{{Title: "t", URL: "https://e.com", Snippet: "s"}})
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *search.Options) {
		o.ToolName = "web_search"
		o.QueryArgKey = "q"
	})
	_, err := searchText(c, "hello")
	must.NoError(err)
	call := fake.Requests()[2]
	check.Equal("web_search", call.ToolName)
	check.Equal("hello", call.Arguments["q"], "arguments: %+v", call.Arguments)
}

func TestSearch_WrongQueryArgKey_IsToolError(t *testing.T) {
	// A server that recognises the tool name but not the query argument key
	// fails as a tool-level error, distinct from an unknown-tool JSON-RPC
	// error: the fake's tool-name check passes ("search" matches the
	// client's default) but its argument-key check does not.
	must := require.New(t)
	fake := searchtest.NewFakeServer(nil, searchtest.WithExpectedTool("search", "notquery"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := searchText(c, "q")
	must.Error(err, "Search should fail when the tool rejects the query argument key")
	must.ErrorContains(err, `missing required argument "notquery"`)
}

func TestSearch_SessionReusedAcrossSearches(t *testing.T) {
	// A stateful server assigns a session on initialize and requires it on
	// tools/call; the client echoes it on every later request and reuses it
	// for the next search, whether or not the previous one failed.
	must := require.New(t)
	check := assert.New(t)
	fake := searchtest.NewFakeServer(nil,
		searchtest.WithSessionID("sess-abc-123"),
		searchtest.WithRawToolResult(`{"content":[{"type":"text","text":"upstream quota exceeded"}],"isError":true}`),
	)
	defer fake.Close()

	c := newClient(t, fake.URL())
	for range 2 {
		_, err := searchText(c, "q")
		must.Error(err, "Search should fail when the tool reports isError")
	}
	reqs := fake.Requests()
	must.Len(reqs, 4, "initialize, initialized, two tools/call")
	// initialize is sent without a session; every later request carries the
	// assigned one.
	check.Empty(reqs[0].SessionID, "initialize session header")
	for i := 1; i < len(reqs); i++ {
		check.Equal(http.MethodPost, reqs[i].HTTPMethod, "request[%d]", i)
		check.Equal("sess-abc-123", reqs[i].SessionID, "request[%d] session", i)
	}
}

func TestSearch_ProtocolVersionHeaderAfterInitialize(t *testing.T) {
	// Every request after initialize carries the revision the server chose.
	must := require.New(t)
	check := assert.New(t)
	fake := searchtest.NewFakeServer(
		[]search.Result{{Title: "t", URL: "https://e.com", Snippet: "s"}},
		searchtest.WithProtocolVersion("2025-03-26"),
	)
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := searchText(c, "q")
	must.NoError(err)
	reqs := fake.Requests()
	check.Empty(reqs[0].ProtocolVersion, "initialize MCP-Protocol-Version header")
	for i := 1; i < len(reqs); i++ {
		check.Equal("2025-03-26", reqs[i].ProtocolVersion, "request[%d] (%s) MCP-Protocol-Version", i, reqs[i].Method)
	}
}

func TestSearch_SessionExpiry_Reinitialises(t *testing.T) {
	// One session serves every search; when the server drops it, the next
	// search re-initialises once and re-sends its call once.
	must := require.New(t)
	check := assert.New(t)
	want := []search.Result{{Title: "t", URL: "https://e.com", Snippet: "s"}}
	fake := searchtest.NewFakeServer(want, searchtest.WithSessionID("sess-s"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	for range 2 {
		_, err := searchText(c, "q")
		must.NoError(err)
	}
	must.Equal(1, fake.InitializeCount(), "initialize count after two searches")
	fake.ExpireSession()
	got, err := searchText(c, "q")
	must.NoError(err, "Search after the session expired")
	check.Equal(want, got, "results")
	// initialize, initialized, two tools/call; the expired tools/call;
	// initialize, initialized and the re-sent tools/call.
	check.Equal(8, fake.CallCount(), "request count")
	check.Equal(2, fake.InitializeCount(), "initialize count")
	check.Equal(4, fake.ToolCallCount(), "tools/call count")
}

func TestSearch_Close_DeletesSessionAndRefusesSearch(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	fake := searchtest.NewFakeServer(nil, searchtest.WithSessionID("sess-end"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := searchText(c, "q")
	must.NoError(err)
	for range 2 {
		must.NoError(c.Close())
	}
	must.Equal(1, fake.DeleteCount(), "DELETE count")
	reqs := fake.Requests()
	end := reqs[len(reqs)-1]
	check.Equal(http.MethodDelete, end.HTTPMethod, "last request method")
	check.Equal("sess-end", end.SessionID, "last request session")
	check.Equal("Bearer "+testKey, end.Authorization, "DELETE authorization")
	check.Equal(mcpclient.ProtocolVersion, end.ProtocolVersion, "DELETE protocol version")
	_, err = searchText(c, "q")
	must.ErrorIs(err, mcpclient.ErrClosed)
	check.True(strings.HasPrefix(err.Error(), "search: "), "error = %v, want the search: prefix", err)
}

func TestSearch_UnsupportedProtocolVersion_RefusesBeforeToolCall(t *testing.T) {
	// A server choosing a revision the client does not implement fails the
	// search before any billable tools/call.
	must := require.New(t)
	check := assert.New(t)
	fake := searchtest.NewFakeServer(
		[]search.Result{{Title: "t", URL: "https://e.com", Snippet: "s"}},
		searchtest.WithProtocolVersion("2024-11-05"),
	)
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := searchText(c, "q")
	must.ErrorIs(err, mcpclient.ErrUnsupportedProtocolVersion)
	check.True(strings.HasPrefix(err.Error(), "search: "), "error = %v, want the search: prefix", err)
	must.ErrorContains(err, `"2024-11-05"`)
	check.Equal(0, fake.ToolCallCount(), "tools/call count")
}

func TestSearch_OverSSE_ReturnsResults(t *testing.T) {
	// The tools/call reply may be a text/event-stream frame carrying the
	// JSON-RPC response; the client reads it under the same bound.
	must := require.New(t)
	want := []search.Result{{Title: "SSE result", URL: "https://e.com/sse", Snippet: "streamed"}}
	fake := searchtest.NewFakeServer(want, searchtest.WithSSE())
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := searchText(c, "q")
	must.NoError(err)
	must.Equal(want, got)
}

func TestSearch_StructuredContent_IsParsed(t *testing.T) {
	// The results document may arrive as structuredContent rather than inside
	// a text block; the client prefers it.
	must := require.New(t)
	raw := `{"content":[],"structuredContent":{"results":[{"title":"S","url":"https://e.com/s","snippet":"struct"}]},"isError":false}`
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult(raw))
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := searchText(c, "q")
	must.NoError(err)
	must.Len(got, 1, "structuredContent not parsed: %+v", got)
	must.Equal("https://e.com/s", got[0].URL)
	must.Equal("struct", got[0].Snippet)
}

func TestSearch_UnexpectedShape_DegradesToSnippet(t *testing.T) {
	// A tool result that carries prose but not the expected results document
	// degrades to a single snippet rather than erroring.
	must := require.New(t)
	check := assert.New(t)
	raw := `{"content":[{"type":"text","text":"I could not find structured results but here is a summary."}],"isError":false}`
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult(raw))
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := searchText(c, "q")
	must.NoError(err, "Search should degrade gracefully")
	must.Len(got, 1, "want 1 degraded snippet: %+v", got)
	check.Equal("I could not find structured results but here is a summary.", got[0].Snippet, "degraded snippet")
	check.Empty(got[0].URL, "degraded result URL")
}

func TestSearch_EmptyResultsDocument_IsSuccess(t *testing.T) {
	// A results document with an empty array is a valid zero-hit success, not
	// a degradation to a text snippet.
	must := require.New(t)
	raw := `{"content":[{"type":"text","text":"{\"results\":[]}"}],"isError":false}`
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult(raw))
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := searchText(c, "q")
	must.NoError(err)
	must.Empty(got, "want no results for an empty results document: %+v", got)
}

func TestSearch_ToolError_IsSurfaced(t *testing.T) {
	// isError:true is a tool-level failure the client surfaces as an error.
	must := require.New(t)
	raw := `{"content":[{"type":"text","text":"upstream search quota exceeded"}],"isError":true}`
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult(raw))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := searchText(c, "q")
	must.Error(err, "Search should fail when the tool reports isError")
	must.ErrorContains(err, "quota")
}

func TestSearchToolCallNotRetried(t *testing.T) {
	// tools/call may invoke a billable upstream search; it is single-attempt.
	// A tools/call reply that cannot be decoded must not be retried.
	must := require.New(t)
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult("this is not json"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, _ = searchText(c, "q")
	must.Equal(1, fake.ToolCallCount(), "the billable tools/call must not be retried")
}

func TestSearch_OversizedBody_IsBounded(t *testing.T) {
	// A tools/call result over the bound is an error, not a truncation.
	must := require.New(t)
	big := strings.Repeat("x", 4096)
	raw := fmt.Sprintf(`{"content":[{"type":"text","text":%q}],"isError":false}`, big)
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult(raw))
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *search.Options) { o.MaxBodyBytes = 512 })
	_, err := searchText(c, "q")
	must.Error(err, "Search should fail when the response exceeds MaxBodyBytes")
	msg := err.Error()
	must.True(strings.Contains(msg, "exceed") || strings.Contains(msg, "bound"), "error = %v, want a body-bound error", err)
}

func TestSearch_OversizedSSE_IsBounded(t *testing.T) {
	// The SSE read path is bounded too: a large streamed frame is rejected.
	must := require.New(t)
	big := strings.Repeat("y", 4096)
	raw := fmt.Sprintf(`{"content":[{"type":"text","text":%q}],"isError":false}`, big)
	fake := searchtest.NewFakeServer(nil, searchtest.WithRawToolResult(raw), searchtest.WithSSE())
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *search.Options) { o.MaxBodyBytes = 512 })
	_, err := searchText(c, "q")
	must.Error(err, "Search over SSE should fail when the stream exceeds MaxBodyBytes")
	msg := err.Error()
	must.True(strings.Contains(msg, "exceed") || strings.Contains(msg, "bound"), "error = %v, want a body-bound error", err)
}

func TestSearch_EndpointValidation(t *testing.T) {
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
		{"query rejected", "https://search.example.com/mcp?key=x", true},
		{"fragment rejected", "https://search.example.com/mcp#frag", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			_, err := search.New(search.Options{Endpoint: tt.endpoint, APIKey: testKey})
			if tt.wantErr {
				must.Error(err, "search.New(%q) should fail", tt.endpoint)
			} else {
				must.NoError(err, "search.New(%q)", tt.endpoint)
			}
			if err != nil {
				must.True(strings.HasPrefix(err.Error(), "search: "), "error = %v, want the search: prefix", err)
			}
		})
	}
}

func TestSearchErrorNeverLeaksKey(t *testing.T) {
	// A failing request must not surface the API key anywhere in the error,
	// even when the server echoes request context in its error body.
	must := require.New(t)
	check := assert.New(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key ` + testKey + `"}`))
	}))
	defer fake.Close()

	c := newClient(t, fake.URL)
	_, err := searchText(c, "q")
	must.Error(err, "Search should fail on a 401")
	must.NotContains(err.Error(), testKey, "error leaked the API key")
	check.True(strings.HasPrefix(err.Error(), "search: "), "error = %v, want the search: prefix", err)
	check.ErrorContains(err, "HTTP 401")
}

func TestSearch_EmptyQuery_IsRejectedBeforeRequest(t *testing.T) {
	must := require.New(t)
	fake := searchtest.NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := searchText(c, "")
	must.Error(err, "Search with an empty query should fail")
	must.Equal(0, fake.CallCount(), "validation must precede any request")
}

func TestSearch_KeylessServer_SendsNoAuthorization(t *testing.T) {
	// A server needing no key: no Authorization header is sent.
	must := require.New(t)
	check := assert.New(t)
	fake := searchtest.NewFakeServer([]search.Result{{Title: "t", URL: "https://e.com", Snippet: "s"}})
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *search.Options) { o.APIKey = "" })
	_, err := searchText(c, "q")
	must.NoError(err)
	for i, req := range fake.Requests() {
		check.Empty(req.Authorization, "request[%d] authorization on a keyless client", i)
	}
}
