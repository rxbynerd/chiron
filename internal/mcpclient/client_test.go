package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "sk-mcp-0123456789abcdefABCDEF"

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

// call runs one CallTool with a fixed tool and argument set.
func call(c *Client) (ToolResult, error) {
	return c.CallTool(context.Background(), "lookup", map[string]any{"query": "q"})
}

// textResult is a handler answering every call with one text block.
func textResult(text string) FakeHandler {
	return func(string, map[string]any) (ToolResult, error) {
		return ToolResult{Content: []ContentBlock{{Type: "text", Text: text}}}, nil
	}
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

func TestCallToolHappyPath(t *testing.T) {
	var gotTool string
	var gotArgs map[string]any
	fake := NewFakeServer(func(tool string, args map[string]any) (ToolResult, error) {
		gotTool, gotArgs = tool, args
		return ToolResult{
			Content:           []ContentBlock{{Type: "text", Text: "hello"}},
			StructuredContent: json.RawMessage(`{"answer":42}`),
		}, nil
	})
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := c.CallTool(context.Background(), "lookup", map[string]any{"query": "sky", "limit": 3})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got.IsError || FirstText(got.Content) != "hello" || string(got.StructuredContent) != `{"answer":42}` {
		t.Errorf("result = %+v, want the scripted text and structuredContent", got)
	}
	if gotTool != "lookup" || gotArgs["query"] != "sky" || gotArgs["limit"] != float64(3) {
		t.Errorf("handler saw tool %q args %+v, want lookup with query and limit", gotTool, gotArgs)
	}

	reqs := fake.Requests()
	if len(reqs) != 3 {
		t.Fatalf("request count = %d, want 3 (initialize, initialized, tools/call)", len(reqs))
	}
	for i, m := range []string{"initialize", "notifications/initialized", "tools/call"} {
		if reqs[i].HTTPMethod != http.MethodPost || reqs[i].Method != m {
			t.Errorf("request[%d] = %s %q, want POST %q", i, reqs[i].HTTPMethod, reqs[i].Method, m)
		}
		if reqs[i].Authorization != "Bearer "+testKey {
			t.Errorf("request[%d].Authorization = %q, want the bearer key header", i, reqs[i].Authorization)
		}
	}
}

func TestCallToolNilArgumentsSendsObject(t *testing.T) {
	fake := NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.CallTool(context.Background(), "ping", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if args := fake.Requests()[2].Arguments; args == nil {
		t.Error("tools/call arguments = null, want an empty object")
	}
}

func TestCallToolHeadersAndClientInfo(t *testing.T) {
	// Every POST names both reply framings in Accept, which strict servers
	// require; initialize advertises the configured clientInfo.
	tests := []struct {
		name        string
		mutate      func(*Options)
		wantName    string
		wantVersion string
	}{
		{"defaults", func(*Options) {}, "chiron", "v2"},
		{"custom", func(o *Options) { o.ClientName, o.ClientVersion = "probe", "1.2" }, "probe", "1.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var accepts, contentTypes []string
			var info clientInfo
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				accepts = append(accepts, r.Header.Get("Accept"))
				contentTypes = append(contentTypes, r.Header.Get("Content-Type"))
				mu.Unlock()
				body, _ := io.ReadAll(r.Body)
				var req struct {
					ID     *int   `json:"id"`
					Method string `json:"method"`
					Params struct {
						ClientInfo clientInfo `json:"clientInfo"`
					} `json:"params"`
				}
				_ = json.Unmarshal(body, &req)
				switch req.Method {
				case "initialize":
					mu.Lock()
					info = req.Params.ClientInfo
					mu.Unlock()
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, deref(req.ID))
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				default:
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[]}}`, deref(req.ID))
				}
			}))
			defer server.Close()

			c := newClient(t, server.URL, tt.mutate)
			if _, err := call(c); err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			for i := range accepts {
				if accepts[i] != "application/json, text/event-stream" {
					t.Errorf("request[%d] Accept = %q, want both framings", i, accepts[i])
				}
				if contentTypes[i] != "application/json" {
					t.Errorf("request[%d] Content-Type = %q, want application/json", i, contentTypes[i])
				}
			}
			if info.Name != tt.wantName || info.Version != tt.wantVersion {
				t.Errorf("clientInfo = %+v, want %s/%s", info, tt.wantName, tt.wantVersion)
			}
		})
	}
}

func TestCallToolSessionEchoedAndEnded(t *testing.T) {
	fake := NewFakeServer(textResult("ok"), WithSessionID("sess-abc-123"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := call(c); err != nil {
		t.Fatalf("CallTool with a session server: %v", err)
	}
	reqs := fake.Requests()
	if len(reqs) != 4 {
		t.Fatalf("request count = %d, want 4 (initialize, initialized, tools/call, DELETE)", len(reqs))
	}
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
	if end.ProtocolVersion != ProtocolVersion {
		t.Errorf("DELETE MCP-Protocol-Version = %q, want %q", end.ProtocolVersion, ProtocolVersion)
	}
}

func TestCallToolSessionEndedAfterToolError(t *testing.T) {
	fake := NewFakeServer(nil,
		WithSessionID("sess-err"),
		WithRawResult(`{"content":[{"type":"text","text":"upstream quota exceeded"}],"isError":true}`),
	)
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := call(c)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !got.IsError || FirstText(got.Content) != "upstream quota exceeded" {
		t.Errorf("result = %+v, want the tool error surfaced as IsError", got)
	}
	reqs := fake.Requests()
	if last := reqs[len(reqs)-1]; last.HTTPMethod != http.MethodDelete || last.SessionID != "sess-err" {
		t.Errorf("last request = %s with session %q, want a DELETE of sess-err", last.HTTPMethod, last.SessionID)
	}
}

func TestCallToolFailedSessionDeleteIgnored(t *testing.T) {
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
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"done"}]}}`, deref(req.ID))
	}))
	defer server.Close()

	c := newClient(t, server.URL)
	got, err := call(c)
	if err != nil {
		t.Fatalf("CallTool should ignore a failed session DELETE, got: %v", err)
	}
	if FirstText(got.Content) != "done" {
		t.Errorf("result = %+v, want the scripted text", got)
	}
	if n := deletes.Load(); n != 1 {
		t.Errorf("DELETE count = %d, want 1", n)
	}
}

func TestCallToolProtocolVersionHeader(t *testing.T) {
	fake := NewFakeServer(textResult("ok"), WithProtocolVersion("2025-03-26"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := call(c); err != nil {
		t.Fatalf("CallTool: %v", err)
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

func TestCallToolProtocolVersionDefaultsWhenUnnamed(t *testing.T) {
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
	if _, err := call(c); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got, _ := toolCallVersion.Load().(string); got != ProtocolVersion {
		t.Errorf("tools/call MCP-Protocol-Version = %q, want the client default %q", got, ProtocolVersion)
	}
}

func TestCallToolRPCError(t *testing.T) {
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
			_, err := call(c)
			if err == nil {
				t.Fatal("CallTool should fail on a JSON-RPC error reply")
			}
			if !strings.HasPrefix(err.Error(), "mcp: tools/call rejected") || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want an mcp tools/call rejection containing %q", err, tt.wantErr)
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

func TestFakeHandlerErrorIsRPCError(t *testing.T) {
	fake := NewFakeServer(func(string, map[string]any) (ToolResult, error) {
		return ToolResult{}, errors.New("unknown tool")
	})
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := call(c)
	if err == nil || !strings.Contains(err.Error(), "tools/call rejected: rpc error -32000: unknown tool") {
		t.Errorf("error = %v, want the handler error as a JSON-RPC rejection", err)
	}
}

func TestCallToolIsErrorTextScrubbed(t *testing.T) {
	// A tool error echoing the key is returned with the key redacted, so a
	// caller can put the text in an error safely.
	fake := NewFakeServer(textErrorHandler("backend rejected key " + testKey))
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := call(c)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !got.IsError {
		t.Fatal("IsError = false, want the tool error flagged")
	}
	text := FirstText(got.Content)
	if strings.Contains(text, testKey) || !strings.Contains(text, "backend rejected key") {
		t.Errorf("tool error text = %q, want it kept with the key redacted", text)
	}
}

func textErrorHandler(text string) FakeHandler {
	return func(string, map[string]any) (ToolResult, error) {
		return ToolResult{Content: []ContentBlock{{Type: "text", Text: text}}, IsError: true}, nil
	}
}

func TestCallToolMismatchedResponseID(t *testing.T) {
	const result = `"result":{"content":[]}`
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
			_, err := call(c)
			if err == nil {
				t.Fatal("CallTool should reject a reply with a mismatched id")
			}
			if !strings.Contains(err.Error(), "does not answer request id 2") {
				t.Errorf("error = %v, want an id-mismatch error", err)
			}
		})
	}
}

func TestCallToolOverSSE(t *testing.T) {
	fake := NewFakeServer(textResult("streamed"), WithSSE())
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := call(c)
	if err != nil {
		t.Fatalf("CallTool over SSE: %v", err)
	}
	if FirstText(got.Content) != "streamed" {
		t.Fatalf("result = %+v, want the streamed text", got)
	}
}

func TestCallToolSSEMultiLineData(t *testing.T) {
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
				`data: "result":{"content":[{"type":"text","text":"multi"}]}}`+"\n"+
				"\n",
		)
	}))
	defer server.Close()

	c := newClient(t, server.URL)
	got, err := call(c)
	if err != nil {
		t.Fatalf("CallTool over multi-line SSE data: %v", err)
	}
	if FirstText(got.Content) != "multi" {
		t.Fatalf("result = %+v, want the text from the rejoined message", got)
	}
}

func TestCallToolSSESkipsServerMessagesBeforeReply(t *testing.T) {
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
			`data: {"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"after"}]}}`+"\n\n",
		)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()

	c := newClient(t, server.URL)
	start := time.Now()
	got, err := call(c)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("CallTool took %s; it should return on the reply frame, not at end of stream", elapsed)
	}
	if FirstText(got.Content) != "after" {
		t.Fatalf("result = %+v, want the reply after the server messages", got)
	}
}

func TestCallToolSSEStreamBounded(t *testing.T) {
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
	_, err := call(c)
	if err == nil {
		t.Fatal("CallTool should fail when the SSE stream exceeds MaxBodyBytes")
	}
	if !strings.Contains(err.Error(), "mcp: SSE stream exceeds 512-byte bound") {
		t.Errorf("error = %v, want the aggregate stream bound", err)
	}
}

func TestCallToolNotRetried(t *testing.T) {
	// A tools/call reply that cannot be decoded must not be retried.
	fake := NewFakeServer(nil, WithRawResult("this is not json"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := call(c); err == nil {
		t.Fatal("CallTool should fail on an undecodable reply")
	}
	if n := fake.ToolCallCount(); n != 1 {
		t.Errorf("tools/call count = %d, want exactly 1; the call must not be retried", n)
	}
}

func TestCallToolOversizedBodyBounded(t *testing.T) {
	big := strings.Repeat("x", 4096)
	raw := fmt.Sprintf(`{"content":[{"type":"text","text":%q}],"isError":false}`, big)
	tests := []struct {
		name string
		opts []FakeOption
	}{
		{"json", []FakeOption{WithRawResult(raw)}},
		{"sse", []FakeOption{WithRawResult(raw), WithSSE()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := NewFakeServer(nil, tt.opts...)
			defer fake.Close()

			c := newClient(t, fake.URL(), func(o *Options) { o.MaxBodyBytes = 512 })
			_, err := call(c)
			if err == nil {
				t.Fatal("CallTool should fail when the response exceeds MaxBodyBytes")
			}
			if !strings.Contains(err.Error(), "512-byte bound") {
				t.Errorf("error = %v, want a body-bound error", err)
			}
		})
	}
}

func TestCallToolCrossHostRedirectRefused(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("redirect target was called; the key would have leaked to %s", r.Host)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer elsewhere.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	c := newClient(t, redirector.URL)
	_, err := call(c)
	if err == nil {
		t.Fatal("CallTool should fail rather than follow a cross-host redirect")
	}
	if !strings.Contains(err.Error(), "cross-origin") {
		t.Errorf("error = %v, want a cross-host redirect refusal", err)
	}
}

func TestCallToolHTTPSDowngradeRedirectRefused(t *testing.T) {
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
	_, err := call(c)
	if err == nil {
		t.Fatal("CallTool should fail rather than follow an https-to-http redirect")
	}
	if !strings.Contains(err.Error(), "downgrade") {
		t.Errorf("error = %v, want a downgrade refusal", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("handler hits = %d, want 1", got)
	}
	if got := conns.Load(); got != 1 {
		t.Errorf("connections = %d, want 1; the cleartext redirect target must never be dialled", got)
	}
}

func TestCallerHTTPClientNotMutated(t *testing.T) {
	hc := &http.Client{}
	_ = newClient(t, "https://mcp.example.com/", func(o *Options) { o.HTTPClient = hc })
	if hc.CheckRedirect != nil {
		t.Error("New set CheckRedirect on the caller's client; it must work on a copy")
	}
}

func TestRedirectPolicy(t *testing.T) {
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
		{"same-host https path change followed", []string{"https://mcp.example.com/mcp"}, "https://mcp.example.com/v2/mcp", ""},
		{"same-host loopback http followed", []string{"http://127.0.0.1:8080/mcp"}, "http://127.0.0.1:8080/v2", ""},
		{"same-host upgrade to https followed", []string{"http://127.0.0.1:8080/mcp"}, "https://127.0.0.1:8080/mcp", ""},
		{"cross-host refused", []string{"https://mcp.example.com/mcp"}, "https://evil.example/mcp", "cross-origin"},
		{"https to http on the same host refused", []string{"https://mcp.example.com/mcp"}, "http://mcp.example.com/mcp", "downgrade"},
		{"downgrade on a later hop refused", []string{"https://mcp.example.com/a", "https://mcp.example.com/b"}, "http://mcp.example.com/c", "downgrade"},
		{"third hop refused", []string{"https://mcp.example.com/a", "https://mcp.example.com/b", "https://mcp.example.com/c"}, "https://mcp.example.com/d", "too many redirects"},
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

func TestEndpointCredentialsRejectedWithoutEcho(t *testing.T) {
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
		{"user and password", "https://" + username + ":" + password + "@mcp.example.com/mcp", "userinfo"},
		{"key as username", "https://" + password + "@mcp.example.com/mcp", "userinfo"},
		{"deceptive host", "https://mcp.example.com:" + password + "@evil.example/mcp", "userinfo"},
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

func TestEndpointValidation(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{"https ok", "https://mcp.example.com/mcp", false},
		{"https root ok", "https://billet.internal/", false},
		{"http loopback ip ok", "http://127.0.0.1:8080/mcp", false},
		{"http loopback ipv6 ok", "http://[::1]:8080/mcp", false},
		{"http localhost ok", "http://localhost:8080/mcp", false},
		{"http non-loopback rejected", "http://mcp.example.com/mcp", true},
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
			if err != nil && !strings.HasPrefix(err.Error(), "mcp: ") {
				t.Errorf("error = %v, want the mcp: prefix", err)
			}
		})
	}
}

func TestCallToolErrorNeverLeaksKey(t *testing.T) {
	// A failing request must not surface the API key anywhere in the error,
	// even when the server echoes request context in its error body.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key ` + testKey + `"}`))
	}))
	defer server.Close()

	c := newClient(t, server.URL)
	_, err := call(c)
	if err == nil {
		t.Fatal("CallTool should fail on a 401")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("error leaked the API key: %v", err)
	}
	if !strings.Contains(err.Error(), "mcp: initialize failed: HTTP 401") {
		t.Errorf("error = %v, want the initialize status surfaced", err)
	}
}

func TestCallToolEmptyNameRejected(t *testing.T) {
	fake := NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.CallTool(context.Background(), "", nil); err == nil {
		t.Fatal("CallTool with an empty tool name should fail")
	}
	if fake.CallCount() != 0 {
		t.Errorf("call count = %d, want 0; validation must precede any request", fake.CallCount())
	}
}

func TestCallToolCallerContextDeadlineWins(t *testing.T) {
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
	_, err := c.CallTool(ctx, "lookup", nil)
	if err == nil {
		t.Fatal("CallTool should fail when the caller deadline fires")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("CallTool took %s; the caller's 100ms deadline should have won over the 10s RequestTimeout", elapsed)
	}
	select {
	case <-aborted:
	case <-time.After(time.Second):
		t.Error("the server never observed the request being aborted")
	}
}

func TestCallToolKeylessServer(t *testing.T) {
	fake := NewFakeServer(textResult("ok"))
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *Options) { o.APIKey = "" })
	if _, err := call(c); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	for i, req := range fake.Requests() {
		if req.Authorization != "" {
			t.Errorf("request[%d] carried Authorization %q on a keyless client", i, req.Authorization)
		}
	}
}

func TestFirstText(t *testing.T) {
	tests := []struct {
		name   string
		blocks []ContentBlock
		want   string
	}{
		{"none", nil, ""},
		{"skips non-text and empty", []ContentBlock{{Type: "image"}, {Type: "text"}, {Type: "text", Text: "b"}}, "b"},
		{"first wins", []ContentBlock{{Type: "text", Text: "a"}, {Type: "text", Text: "b"}}, "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FirstText(tt.blocks); got != tt.want {
				t.Errorf("FirstText = %q, want %q", got, tt.want)
			}
		})
	}
}
