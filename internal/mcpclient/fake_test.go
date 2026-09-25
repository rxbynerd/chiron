package mcpclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rxbynerd/chiron/internal/mcpclient"
	"github.com/rxbynerd/chiron/internal/mcpclient/mcpclienttest"
)

const testKey = "sk-mcp-0123456789abcdefABCDEF"

// newClient builds a Client pointed at an endpoint, failing the test on a
// construction error.
func newClient(t *testing.T, endpoint string, mutate ...func(*mcpclient.Options)) *mcpclient.Client {
	t.Helper()
	opts := mcpclient.Options{Endpoint: endpoint, APIKey: testKey}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := mcpclient.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// call runs one CallTool with a fixed tool and argument set.
func call(c *mcpclient.Client) (mcpclient.ToolResult, error) {
	return c.CallTool(context.Background(), "lookup", map[string]any{"query": "q"})
}

// rpcRequest mirrors the JSON-RPC request shape the Client sends, so
// answerHandshake can decode a raw httptest request without reaching into
// mcpclient's unexported wire types.
type rpcRequest struct {
	ID     *int   `json:"id"`
	Method string `json:"method"`
}

// deref dereferences an rpcRequest.ID, defaulting to 0.
func deref(id *int) int {
	if id == nil {
		return 0
	}
	return *id
}

// answerHandshake reads one request in full and answers it when it belongs to
// the MCP handshake, as a minimal stateless server that accepts the client's
// protocol version. It returns the decoded request and whether it was
// answered; the caller answers anything else (tools/call).
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
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q,"capabilities":{},"serverInfo":{"name":"test","version":"0"}}}`, deref(req.ID), mcpclient.ProtocolVersion)
		return req, true
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return req, true
	}
	return req, false
}

// textResult is a handler answering every call with one text block.
func textResult(text string) mcpclienttest.FakeHandler {
	return func(string, map[string]any) (mcpclient.ToolResult, error) {
		return mcpclient.ToolResult{Content: []mcpclient.ContentBlock{{Type: "text", Text: text}}}, nil
	}
}

func TestCallToolHappyPath(t *testing.T) {
	var gotTool string
	var gotArgs map[string]any
	fake := mcpclienttest.NewFakeServer(func(tool string, args map[string]any) (mcpclient.ToolResult, error) {
		gotTool, gotArgs = tool, args
		return mcpclient.ToolResult{
			Content:           []mcpclient.ContentBlock{{Type: "text", Text: "hello"}},
			StructuredContent: json.RawMessage(`{"answer":42}`),
		}, nil
	})
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := c.CallTool(context.Background(), "lookup", map[string]any{"query": "sky", "limit": 3})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got.IsError || mcpclient.FirstText(got.Content) != "hello" || string(got.StructuredContent) != `{"answer":42}` {
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
	fake := mcpclienttest.NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.CallTool(context.Background(), "ping", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if args := fake.Requests()[2].Arguments; args == nil {
		t.Error("tools/call arguments = null, want an empty object")
	}
}

func TestCallToolSessionEchoedAndEnded(t *testing.T) {
	fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithSessionID("sess-abc-123"))
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
	if end.ProtocolVersion != mcpclient.ProtocolVersion {
		t.Errorf("DELETE MCP-Protocol-Version = %q, want %q", end.ProtocolVersion, mcpclient.ProtocolVersion)
	}
}

func TestCallToolSessionEndedAfterToolError(t *testing.T) {
	fake := mcpclienttest.NewFakeServer(nil,
		mcpclienttest.WithSessionID("sess-err"),
		mcpclienttest.WithRawResult(`{"content":[{"type":"text","text":"upstream quota exceeded"}],"isError":true}`),
	)
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := call(c)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !got.IsError || mcpclient.FirstText(got.Content) != "upstream quota exceeded" {
		t.Errorf("result = %+v, want the tool error surfaced as IsError", got)
	}
	reqs := fake.Requests()
	if last := reqs[len(reqs)-1]; last.HTTPMethod != http.MethodDelete || last.SessionID != "sess-err" {
		t.Errorf("last request = %s with session %q, want a DELETE of sess-err", last.HTTPMethod, last.SessionID)
	}
}

func TestCallToolProtocolVersionHeader(t *testing.T) {
	fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithProtocolVersion("2025-03-26"))
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

func TestCallToolUnsupportedProtocolVersion(t *testing.T) {
	// A reply naming a version outside the supported set, or none, fails the
	// call before notifications/initialized or tools/call. A session the
	// server issued is ended, with no version header since none was agreed.
	tests := []struct {
		name        string
		opts        []mcpclienttest.FakeOption
		wantVersion string
		wantDeletes int
	}{
		{"older revision, stateful", []mcpclienttest.FakeOption{mcpclienttest.WithProtocolVersion("2024-11-05"), mcpclienttest.WithSessionID("sess-v1")}, "2024-11-05", 1},
		{"newer revision, stateless", []mcpclienttest.FakeOption{mcpclienttest.WithProtocolVersion("2099-01-01")}, "2099-01-01", 0},
		{"missing, stateful", []mcpclienttest.FakeOption{mcpclienttest.WithoutProtocolVersion(), mcpclienttest.WithSessionID("sess-v2")}, "", 1},
		{"empty, stateless", []mcpclienttest.FakeOption{mcpclienttest.WithProtocolVersion("")}, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := mcpclienttest.NewFakeServer(textResult("ok"), tt.opts...)
			defer fake.Close()

			_, err := call(newClient(t, fake.URL()))
			if !errors.Is(err, mcpclient.ErrUnsupportedProtocolVersion) {
				t.Fatalf("error = %v, want ErrUnsupportedProtocolVersion", err)
			}
			var pvErr *mcpclient.ProtocolVersionError
			if !errors.As(err, &pvErr) || pvErr.Version != tt.wantVersion {
				t.Errorf("ProtocolVersionError = %+v, want Version %q", pvErr, tt.wantVersion)
			}
			if strings.Contains(err.Error(), "sess-") {
				t.Errorf("error carries the session id: %v", err)
			}
			var posts []string
			deletes := 0
			for _, r := range fake.Requests() {
				if r.HTTPMethod != http.MethodDelete {
					posts = append(posts, r.Method)
					continue
				}
				deletes++
				if r.SessionID == "" || r.ProtocolVersion != "" {
					t.Errorf("DELETE session %q version %q, want the issued session and no version", r.SessionID, r.ProtocolVersion)
				}
			}
			if strings.Join(posts, ",") != "initialize" {
				t.Errorf("POSTs = %v, want only initialize", posts)
			}
			if deletes != tt.wantDeletes {
				t.Errorf("DELETE count = %d, want %d", deletes, tt.wantDeletes)
			}
		})
	}
}

func TestCallToolUnsupportedProtocolVersionBounded(t *testing.T) {
	// The refused version is server-controlled text: it reaches the error
	// scrubbed, cut to 32 bytes and quoted, so it cannot carry the key, a raw
	// line break or bulk.
	hostile := "\n" + testKey + strings.Repeat("A", 4096)
	fake := mcpclienttest.NewFakeServer(nil, mcpclienttest.WithProtocolVersion(hostile))
	defer fake.Close()

	_, err := call(newClient(t, fake.URL()))
	var pvErr *mcpclient.ProtocolVersionError
	if !errors.As(err, &pvErr) {
		t.Fatalf("error = %v, want a ProtocolVersionError", err)
	}
	if len(pvErr.Version) > 32 || strings.Contains(pvErr.Version, testKey[:12]) {
		t.Errorf("Version = %q, want at most 32 scrubbed bytes", pvErr.Version)
	}
	msg := err.Error()
	if strings.Contains(msg, "\n") || strings.Contains(msg, testKey[:12]) || len(msg) > 200 {
		t.Errorf("error = %q, want a short quoted excerpt without the key", msg)
	}
}

func TestFakeHandlerErrorIsRPCError(t *testing.T) {
	fake := mcpclienttest.NewFakeServer(func(string, map[string]any) (mcpclient.ToolResult, error) {
		return mcpclient.ToolResult{}, errors.New("unknown tool")
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
	fake := mcpclienttest.NewFakeServer(textErrorHandler("backend rejected key " + testKey))
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := call(c)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !got.IsError {
		t.Fatal("IsError = false, want the tool error flagged")
	}
	text := mcpclient.FirstText(got.Content)
	if strings.Contains(text, testKey) || !strings.Contains(text, "backend rejected key") {
		t.Errorf("tool error text = %q, want it kept with the key redacted", text)
	}
}

func textErrorHandler(text string) mcpclienttest.FakeHandler {
	return func(string, map[string]any) (mcpclient.ToolResult, error) {
		return mcpclient.ToolResult{Content: []mcpclient.ContentBlock{{Type: "text", Text: text}}, IsError: true}, nil
	}
}

func TestCallToolOverSSE(t *testing.T) {
	fake := mcpclienttest.NewFakeServer(textResult("streamed"), mcpclienttest.WithSSE())
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := call(c)
	if err != nil {
		t.Fatalf("CallTool over SSE: %v", err)
	}
	if mcpclient.FirstText(got.Content) != "streamed" {
		t.Fatalf("result = %+v, want the streamed text", got)
	}
}

func TestCallToolNotRetried(t *testing.T) {
	// A tools/call reply that cannot be decoded must not be retried.
	fake := mcpclienttest.NewFakeServer(nil, mcpclienttest.WithRawResult("this is not json"))
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
		opts []mcpclienttest.FakeOption
	}{
		{"json", []mcpclienttest.FakeOption{mcpclienttest.WithRawResult(raw)}},
		{"sse", []mcpclienttest.FakeOption{mcpclienttest.WithRawResult(raw), mcpclienttest.WithSSE()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := mcpclienttest.NewFakeServer(nil, tt.opts...)
			defer fake.Close()

			c := newClient(t, fake.URL(), func(o *mcpclient.Options) { o.MaxBodyBytes = 512 })
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

func TestCallToolEmptyNameRejected(t *testing.T) {
	fake := mcpclienttest.NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.CallTool(context.Background(), "", nil); err == nil {
		t.Fatal("CallTool with an empty tool name should fail")
	}
	if fake.CallCount() != 0 {
		t.Errorf("call count = %d, want 0; validation must precede any request", fake.CallCount())
	}
}

func TestCallToolKeylessServer(t *testing.T) {
	fake := mcpclienttest.NewFakeServer(textResult("ok"))
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *mcpclient.Options) { o.APIKey = "" })
	if _, err := call(c); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	for i, req := range fake.Requests() {
		if req.Authorization != "" {
			t.Errorf("request[%d] carried Authorization %q on a keyless client", i, req.Authorization)
		}
	}
}

func TestCallToolErrorTextBounded(t *testing.T) {
	// Every server-supplied error text is scrubbed and cut to 4 KiB, so an
	// adapter's error never carries megabytes into a caller's transcript.
	huge := "backend failed for key " + testKey + "\n" + strings.Repeat("é", 200<<10)
	tests := []struct {
		name       string
		status     int
		body       string
		wantErr    string
		wantDetail bool
	}{
		{"tools/call rejection", http.StatusOK, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":%q}}`, huge), "tools/call rejected", true},
		{"error body", http.StatusBadGateway, huge, "tools/call failed: HTTP 502: backend failed", true},
		{"non-JSON error body over the read bound", http.StatusBadGateway, strings.Repeat("<html>", (1<<20)/5), "tools/call failed: HTTP 502", false},
		{"empty error body", http.StatusBadGateway, "", "tools/call failed: HTTP 502", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, answered := answerHandshake(t, w, r); answered {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			_, err := call(newClient(t, server.URL))
			if err == nil {
				t.Fatal("CallTool should fail")
			}
			msg := err.Error()
			if !strings.Contains(msg, tt.wantErr) || strings.Contains(msg, testKey) {
				t.Errorf("error = %.200q, want %q with the key redacted", msg, tt.wantErr)
			}
			if len(msg) > (4<<10)+64 || !utf8.ValidString(msg) {
				t.Errorf("error is %d bytes (valid UTF-8 %v), want at most the %d-byte bound plus its prefix", len(msg), utf8.ValidString(msg), (4 << 10))
			}
			if got := strings.HasSuffix(msg, " [truncated]"); got != tt.wantDetail {
				t.Errorf("error truncation marker = %v, want %v: %.200q", got, tt.wantDetail, msg)
			}
		})
	}

	fake := mcpclienttest.NewFakeServer(textErrorHandler(huge))
	defer fake.Close()
	got, err := call(newClient(t, fake.URL()))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text := mcpclient.FirstText(got.Content)
	if !got.IsError || len(text) > (4<<10) || !strings.HasSuffix(text, " [truncated]") || strings.Contains(text, testKey) {
		t.Errorf("IsError text is %d bytes, want a scrubbed excerpt of at most %d ending in the marker", len(text), (4 << 10))
	}
}
