package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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

// answerHandshake reads one request in full and answers it when it belongs to
// the MCP handshake, as a minimal stateless server whose initialize result
// names no protocol version. It returns the decoded request and whether it
// was answered; the caller answers anything else (tools/call).
func answerHandshake(t *testing.T, w http.ResponseWriter, r *http.Request) (rpcRequest, bool) {
	t.Helper()
	var req rpcRequest
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("reading request body: %v", err)
		return req, true
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Errorf("decoding request body: %v", err)
		return req, true
	}
	switch req.Method {
	case "initialize":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"capabilities":{},"serverInfo":{"name":"test","version":"0"}}}`, deref(req.ID))
		return req, true
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return req, true
	}
	return req, false
}

// sseWrite writes frames as a text/event-stream reply and flushes them.
func sseWrite(t *testing.T, w http.ResponseWriter, frames ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, f := range frames {
		if _, err := fmt.Fprint(w, f); err != nil {
			t.Errorf("writing SSE frame: %v", err)
		}
	}
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
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

func TestSearchSessionEchoedAndEnded(t *testing.T) {
	// A stateful server assigns a session on initialize and requires it on
	// tools/call; the client echoes it, then ends it with a DELETE.
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
	if end.ProtocolVersion != mcpProtocolVersion {
		t.Errorf("DELETE MCP-Protocol-Version = %q, want %q", end.ProtocolVersion, mcpProtocolVersion)
	}
}

func TestSearchSessionEndedAfterToolError(t *testing.T) {
	// The session is ended even when the search itself fails.
	fake := NewFakeServer(nil,
		WithSessionID("sess-err"),
		WithRawToolResult(`{"content":[{"type":"text","text":"upstream quota exceeded"}],"isError":true}`),
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

func TestSearchFailedSessionDeleteIgnored(t *testing.T) {
	// A server may refuse or fail the DELETE; the search result stands.
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set(mcpSessionHeader, "sess-1")
		req, answered := answerHandshake(t, w, r)
		if answered {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"t\",\"url\":\"https://e.com\",\"snippet\":\"s\"}]}"}]}}`, deref(req.ID))
	}))
	defer server.Close()

	c := newClient(t, server.URL)
	got, err := c.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search should ignore a failed session DELETE, got: %v", err)
	}
	if len(got) != 1 || got[0].URL != "https://e.com" {
		t.Errorf("results = %+v, want the one scripted result", got)
	}
	if n := deletes.Load(); n != 1 {
		t.Errorf("DELETE count = %d, want 1", n)
	}
}

func TestSearchProtocolVersionHeader(t *testing.T) {
	// Every request after initialize carries the revision the server chose.
	fake := NewFakeServer(
		[]Result{{Title: "t", URL: "https://e.com", Snippet: "s"}},
		WithProtocolVersion("2025-03-26"),
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

func TestSearchProtocolVersionDefaultsWhenUnnamed(t *testing.T) {
	// An initialize result naming no revision falls back to the client's own.
	var toolCallVersion atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, answered := answerHandshake(t, w, r)
		if answered {
			return
		}
		toolCallVersion.Store(r.Header.Get(mcpProtocolVersionHeader))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[]}}`, deref(req.ID))
	}))
	defer server.Close()

	c := newClient(t, server.URL)
	if _, err := c.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got, _ := toolCallVersion.Load().(string); got != mcpProtocolVersion {
		t.Errorf("tools/call MCP-Protocol-Version = %q, want the client default %q", got, mcpProtocolVersion)
	}
}

func TestSearchToolCallRPCError(t *testing.T) {
	// A JSON-RPC error on tools/call is surfaced, scrubbed, and not retried.
	// A null id is how JSON-RPC reports an error it could not attribute.
	tests := []struct {
		name    string
		reply   string
		wantErr string
	}{
		{"error echoing the key", `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"auth failed for key ` + testKey + `"}}`, "rpc error -32000: auth failed for key"},
		{"error with null id", `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request"}}`, "rpc error -32600: invalid request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var toolCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, answered := answerHandshake(t, w, r); answered {
					return
				}
				toolCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.reply)
			}))
			defer server.Close()

			c := newClient(t, server.URL)
			_, err := c.Search(context.Background(), "q")
			if err == nil {
				t.Fatal("Search should fail on a JSON-RPC error reply")
			}
			if !strings.Contains(err.Error(), "tools/call rejected") || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want a tools/call rejection containing %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("error leaked the API key: %v", err)
			}
			if n := toolCalls.Load(); n != 1 {
				t.Errorf("tools/call count = %d, want exactly 1", n)
			}
		})
	}
}

func TestSearchMismatchedResponseID(t *testing.T) {
	// A reply that does not carry the request's id is rejected, whichever
	// framing carries it.
	const result = `"result":{"content":[{"type":"text","text":"{\"results\":[]}"}]}`
	tests := []struct {
		name  string
		sse   bool
		reply string
	}{
		{"json wrong id", false, `{"jsonrpc":"2.0","id":99,` + result + `}`},
		{"json string id", false, `{"jsonrpc":"2.0","id":"2",` + result + `}`},
		{"json missing id on a result", false, `{"jsonrpc":"2.0",` + result + `}`},
		{"sse wrong id", true, `{"jsonrpc":"2.0","id":7,` + result + `}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, answered := answerHandshake(t, w, r); answered {
					return
				}
				if tt.sse {
					sseWrite(t, w, "data: "+tt.reply+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.reply)
			}))
			defer server.Close()

			c := newClient(t, server.URL)
			_, err := c.Search(context.Background(), "q")
			if err == nil {
				t.Fatal("Search should reject a reply with a mismatched id")
			}
			if !strings.Contains(err.Error(), "does not answer request id 2") {
				t.Errorf("error = %v, want an id-mismatch error", err)
			}
		})
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

func TestSearchSSEMultiLineData(t *testing.T) {
	// One JSON-RPC message split across several data: lines is rejoined with
	// newlines before decoding; event: and id: fields are ignored.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, answered := answerHandshake(t, w, r); answered {
			return
		}
		sseWrite(t, w,
			"event: message\n"+
				"id: evt-1\n"+
				`data: {"jsonrpc":"2.0",`+"\n"+
				`data: "id":2,`+"\n"+
				`data: "result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"t\",\"url\":\"https://e.com/multi\",\"snippet\":\"s\"}]}"}]}}`+"\n"+
				"\n",
		)
	}))
	defer server.Close()

	c := newClient(t, server.URL)
	got, err := c.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search over multi-line SSE data: %v", err)
	}
	if len(got) != 1 || got[0].URL != "https://e.com/multi" {
		t.Fatalf("results = %+v, want the one result from the rejoined message", got)
	}
}

func TestSearchSSESkipsServerMessagesBeforeReply(t *testing.T) {
	// A keep-alive comment, a notification and a server request with its own
	// id may precede the reply. They are skipped, and the reply is returned
	// without waiting for the server to end the stream.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, answered := answerHandshake(t, w, r); answered {
			return
		}
		sseWrite(t, w,
			": keep-alive\n\n",
			`data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"p","progress":1}}`+"\n\n",
			`data: {"jsonrpc":"2.0","id":"srv-1","method":"ping"}`+"\n\n",
			`data: {"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"t\",\"url\":\"https://e.com/after\",\"snippet\":\"s\"}]}"}]}}`+"\n\n",
		)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()

	c := newClient(t, server.URL)
	start := time.Now()
	got, err := c.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Search took %s — it should return on the reply frame, not at end of stream", elapsed)
	}
	if len(got) != 1 || got[0].URL != "https://e.com/after" {
		t.Fatalf("results = %+v, want the reply after the server messages", got)
	}
}

func TestSearchSSEStreamBounded(t *testing.T) {
	// Many frames each under the bound still count against it in aggregate.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, answered := answerHandshake(t, w, r); answered {
			return
		}
		var frames []string
		for i := range 20 {
			frames = append(frames, fmt.Sprintf(`data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":%d}}`+"\n\n", i))
		}
		frames = append(frames, `data: {"jsonrpc":"2.0","id":2,"result":{"content":[]}}`+"\n\n")
		sseWrite(t, w, frames...)
	}))
	defer server.Close()

	c := newClient(t, server.URL, func(o *Options) { o.MaxBodyBytes = 512 })
	_, err := c.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search should fail when the SSE stream exceeds MaxBodyBytes")
	}
	if !strings.Contains(err.Error(), "SSE stream exceeds 512-byte bound") {
		t.Errorf("error = %v, want the aggregate stream bound", err)
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
	// A tools/call reply that cannot be decoded must not be retried.
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

func TestSearchHTTPSDowngradeRedirectRefused(t *testing.T) {
	// net/http re-sends Authorization to the same hostname on any scheme, so
	// an https endpoint redirecting to http on the same host must be refused
	// before a second connection is opened.
	var hits, conns atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://"+r.Host+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()

	c := newClient(t, server.URL, func(o *Options) { o.HTTPClient = server.Client() })
	_, err := c.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search should fail rather than follow an https-to-http redirect")
	}
	if !strings.Contains(err.Error(), "downgrade") {
		t.Errorf("error = %v, want a downgrade refusal", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("handler hits = %d, want 1", got)
	}
	if got := conns.Load(); got != 1 {
		t.Errorf("connections = %d, want 1 — the cleartext redirect target must never be dialled", got)
	}
}

func TestSearchRedirectPolicy(t *testing.T) {
	request := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", raw, err)
		}
		return &http.Request{URL: u}
	}
	tests := []struct {
		name    string
		via     []string
		target  string
		wantErr string
	}{
		{"same-host https path change followed", []string{"https://search.example.com/mcp"}, "https://search.example.com/v2/mcp", ""},
		{"same-host loopback http followed", []string{"http://127.0.0.1:8080/mcp"}, "http://127.0.0.1:8080/v2", ""},
		{"same-host upgrade to https followed", []string{"http://127.0.0.1:8080/mcp"}, "https://127.0.0.1:8080/mcp", ""},
		{"cross-host refused", []string{"https://search.example.com/mcp"}, "https://evil.example/mcp", "cross-origin"},
		{"https to http on the same host refused", []string{"https://search.example.com/mcp"}, "http://search.example.com/mcp", "downgrade"},
		{"downgrade on a later hop refused", []string{"https://search.example.com/a", "https://search.example.com/b"}, "http://search.example.com/c", "downgrade"},
		{"third hop refused", []string{"https://search.example.com/a", "https://search.example.com/b", "https://search.example.com/c"}, "https://search.example.com/d", "too many redirects"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var via []*http.Request
			for _, v := range tt.via {
				via = append(via, request(v))
			}
			err := refuseUnsafeRedirects(request(tt.target), via)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("refuseUnsafeRedirects = %v, want the redirect followed", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("refuseUnsafeRedirects = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestSearchEndpointCredentialsRejectedWithoutEcho(t *testing.T) {
	// An endpoint carrying credentials is rejected, and the error never
	// repeats them, even when a typo stops url.Parse recognising userinfo.
	const (
		username = "alice"
		password = "Winter2026!"
	)
	tests := []struct {
		name     string
		endpoint string
		wantErr  string
	}{
		{"user and password", "https://" + username + ":" + password + "@search.example.com/mcp", "userinfo"},
		{"key as username", "https://" + password + "@search.example.com/mcp", "userinfo"},
		{"deceptive host", "https://search.example.com:" + password + "@evil.example/mcp", "userinfo"},
		{"loopback with userinfo", "http://" + username + ":" + password + "@127.0.0.1:8080/mcp", "userinfo"},
		{"mistyped scheme", "htps://" + username + ":" + password + "@gateway.corp/mcp", "userinfo"},
		{"single slash", "https:/" + username + ":" + password + "@gateway.corp/mcp", "withheld"},
		{"unparseable", "://" + username + ":" + password + "@gateway.corp/mcp", "withheld"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(Options{Endpoint: tt.endpoint, APIKey: testKey})
			if err == nil {
				t.Fatalf("New(%q) succeeded, want an error", tt.endpoint)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), username) {
				t.Errorf("error echoed the credentials: %v", err)
			}
		})
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
	// The caller's tighter deadline must beat the generous RequestTimeout. The
	// handler drains the body because net/http only cancels r.Context() on
	// client disconnect once it is read; the 2s backstop keeps Close unblocked.
	aborted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			close(aborted)
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
	select {
	case <-aborted:
	case <-time.After(time.Second):
		t.Error("the server never observed the request being aborted")
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
