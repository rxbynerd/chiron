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
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/mcpclient"
	"github.com/rxbynerd/chiron/internal/mcpclient/mcpclienttest"
)

const testKey = "sk-mcp-0123456789abcdefABCDEF"

// newClient builds a Client pointed at an endpoint, failing the test on a
// construction error.
func newClient(t *testing.T, endpoint string, mutate ...func(*mcpclient.Options)) *mcpclient.Client {
	t.Helper()
	must := require.New(t)
	opts := mcpclient.Options{Endpoint: endpoint, APIKey: testKey}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := mcpclient.New(opts)
	must.NoError(err, "New")
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
// answered; the caller answers anything else (tools/call). It runs on the
// server's goroutine, so it reports through assert rather than require.
func answerHandshake(t *testing.T, w http.ResponseWriter, r *http.Request) (rpcRequest, bool) {
	t.Helper()
	check := assert.New(t)
	var req rpcRequest
	body, err := io.ReadAll(r.Body)
	if !check.NoError(err, "reading request body") {
		return req, true
	}
	if !check.NoError(json.Unmarshal(body, &req), "decoding request body") {
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

func TestCallTool_ReturnsScriptedResultAfterHandshake(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

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
	must.NoError(err, "CallTool")

	check.False(got.IsError)
	check.Equal("hello", mcpclient.FirstText(got.Content))
	check.Equal(`{"answer":42}`, string(got.StructuredContent))
	check.Equal("lookup", gotTool)
	check.Equal("sky", gotArgs["query"])
	check.InDelta(3, gotArgs["limit"], 0)

	reqs := fake.Requests()
	must.Len(reqs, 3, "want initialize, initialized, tools/call")
	for i, m := range []string{"initialize", "notifications/initialized", "tools/call"} {
		check.Equal(http.MethodPost, reqs[i].HTTPMethod, "request[%d] HTTP method", i)
		check.Equal(m, reqs[i].Method, "request[%d] JSON-RPC method", i)
		check.Equal("Bearer "+testKey, reqs[i].Authorization, "request[%d] authorization", i)
	}
}

func TestCallTool_NilArgumentsSendAnEmptyObject(t *testing.T) {
	must := require.New(t)
	fake := mcpclienttest.NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.CallTool(context.Background(), "ping", nil)
	must.NoError(err, "CallTool")
	must.NotNil(fake.Requests()[2].Arguments, "tools/call arguments should be an empty object, not null")
}

func TestCallTool_EchoesSessionAndDeletesOnClose(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithSessionID("sess-abc-123"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := call(c)
	must.NoError(err, "CallTool with a session server")
	must.Equal(3, fake.CallCount(), "want initialize, initialized, tools/call; the session outlives the call")

	must.NoError(c.Close(), "Close")
	reqs := fake.Requests()
	must.Len(reqs, 4, "want initialize, initialized, tools/call, DELETE")
	check.Empty(reqs[0].SessionID, "initialize should carry no session header")
	for i := 1; i < len(reqs); i++ {
		check.Equal("sess-abc-123", reqs[i].SessionID, "request[%d] session header", i)
	}

	end := reqs[3]
	must.Equal(http.MethodDelete, end.HTTPMethod, "last request should be the session DELETE")
	check.Equal("Bearer "+testKey, end.Authorization, "DELETE authorization")
	check.Equal(mcpclient.ProtocolVersion, end.ProtocolVersion, "DELETE protocol version")
}

func TestCallTool_ToolErrorKeepsSession(t *testing.T) {
	// A tool-level failure is a successful exchange: the session stays in
	// use, and is ended only by Close.
	must := require.New(t)
	check := assert.New(t)

	fake := mcpclienttest.NewFakeServer(nil,
		mcpclienttest.WithSessionID("sess-err"),
		mcpclienttest.WithRawResult(`{"content":[{"type":"text","text":"upstream quota exceeded"}],"isError":true}`),
	)
	defer fake.Close()

	c := newClient(t, fake.URL())
	for range 2 {
		got, err := call(c)
		must.NoError(err, "CallTool")
		check.True(got.IsError, "tool error should be flagged as IsError")
		check.Equal("upstream quota exceeded", mcpclient.FirstText(got.Content))
	}
	check.Equal(1, fake.InitializeCount(), "initialize count")
	check.Zero(fake.DeleteCount(), "DELETE count before Close")

	must.NoError(c.Close(), "Close")
	reqs := fake.Requests()
	last := reqs[len(reqs)-1]
	check.Equal(http.MethodDelete, last.HTTPMethod, "last request method")
	check.Equal("sess-err", last.SessionID, "last request session")
}

func TestCallTool_SendsNegotiatedProtocolVersionHeader(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithProtocolVersion("2025-03-26"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := call(c)
	must.NoError(err, "CallTool")

	reqs := fake.Requests()
	check.Empty(reqs[0].ProtocolVersion, "initialize should carry no MCP-Protocol-Version header")
	for i := 1; i < len(reqs); i++ {
		check.Equal("2025-03-26", reqs[i].ProtocolVersion, "request[%d] (%s) MCP-Protocol-Version", i, reqs[i].Method)
	}
}

func TestCallTool_RefusesUnsupportedProtocolVersion(t *testing.T) {
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
			must := require.New(t)

			fake := mcpclienttest.NewFakeServer(textResult("ok"), tt.opts...)
			defer fake.Close()

			_, err := call(newClient(t, fake.URL()))
			must.ErrorIs(err, mcpclient.ErrUnsupportedProtocolVersion)
			var pvErr *mcpclient.ProtocolVersionError
			must.ErrorAs(err, &pvErr)
			must.Equal(tt.wantVersion, pvErr.Version, "ProtocolVersionError.Version")
			must.NotContains(err.Error(), "sess-", "error should not carry the session id")

			var posts []string
			deletes := 0
			for _, r := range fake.Requests() {
				if r.HTTPMethod != http.MethodDelete {
					posts = append(posts, r.Method)
					continue
				}
				deletes++
				must.NotEmpty(r.SessionID, "DELETE should carry the issued session")
				must.Empty(r.ProtocolVersion, "DELETE should carry no protocol version")
			}
			must.Equal([]string{"initialize"}, posts, "only initialize should be POSTed")
			must.Equal(tt.wantDeletes, deletes, "DELETE count")
		})
	}
}

func TestCallTool_BoundsRefusedProtocolVersionText(t *testing.T) {
	// The refused version is server-controlled text: it reaches the error
	// scrubbed, cut to 32 bytes and quoted, so it cannot carry the key, a raw
	// line break or bulk.
	must := require.New(t)
	check := assert.New(t)

	hostile := "\n" + testKey + strings.Repeat("A", 4096)
	fake := mcpclienttest.NewFakeServer(nil, mcpclienttest.WithProtocolVersion(hostile))
	defer fake.Close()

	_, err := call(newClient(t, fake.URL()))
	var pvErr *mcpclient.ProtocolVersionError
	must.ErrorAs(err, &pvErr, "want a ProtocolVersionError")
	check.LessOrEqual(len(pvErr.Version), 32, "Version %q should be at most 32 scrubbed bytes", pvErr.Version)
	check.NotContains(pvErr.Version, testKey[:12], "Version should not carry the key")

	msg := err.Error()
	check.NotContains(msg, "\n", "error should be one line")
	check.NotContains(msg, testKey[:12], "error should not carry the key")
	check.LessOrEqual(len(msg), 200, "error %q should be a short excerpt", msg)
}

func TestCallTool_HandlerErrorIsRPCRejection(t *testing.T) {
	must := require.New(t)
	fake := mcpclienttest.NewFakeServer(func(string, map[string]any) (mcpclient.ToolResult, error) {
		return mcpclient.ToolResult{}, errors.New("unknown tool")
	})
	defer fake.Close()

	_, err := call(newClient(t, fake.URL()))
	must.ErrorContains(err, "tools/call rejected: rpc error -32000: unknown tool", "want the handler error as a JSON-RPC rejection")
}

func TestCallTool_ScrubsKeyFromToolErrorText(t *testing.T) {
	// A tool error echoing the key is returned with the key redacted, so a
	// caller can put the text in an error safely.
	must := require.New(t)

	fake := mcpclienttest.NewFakeServer(textErrorHandler("backend rejected key " + testKey))
	defer fake.Close()

	got, err := call(newClient(t, fake.URL()))
	must.NoError(err, "CallTool")
	must.True(got.IsError, "tool error should be flagged")
	text := mcpclient.FirstText(got.Content)
	must.NotContains(text, testKey, "tool error text should have the key redacted")
	must.Contains(text, "backend rejected key", "tool error text should be kept")
}

func textErrorHandler(text string) mcpclienttest.FakeHandler {
	return func(string, map[string]any) (mcpclient.ToolResult, error) {
		return mcpclient.ToolResult{Content: []mcpclient.ContentBlock{{Type: "text", Text: text}}, IsError: true}, nil
	}
}

func TestCallTool_ReadsReplyOverSSE(t *testing.T) {
	must := require.New(t)
	fake := mcpclienttest.NewFakeServer(textResult("streamed"), mcpclienttest.WithSSE())
	defer fake.Close()

	got, err := call(newClient(t, fake.URL()))
	must.NoError(err, "CallTool over SSE")
	must.Equal("streamed", mcpclient.FirstText(got.Content))
}

func TestCallTool_UndecodableReplyNotRetried(t *testing.T) {
	// A tools/call reply that cannot be decoded must not be retried.
	must := require.New(t)
	fake := mcpclienttest.NewFakeServer(nil, mcpclienttest.WithRawResult("this is not json"))
	defer fake.Close()

	_, err := call(newClient(t, fake.URL()))
	must.Error(err, "CallTool should fail on an undecodable reply")
	must.Equal(1, fake.ToolCallCount(), "tools/call count; the call must not be retried")
}

func TestCallTool_RejectsMalformedHandshakeOrResult(t *testing.T) {
	// An initialize answered with a JSON-RPC error or a non-object result
	// fails the call with nothing sent after it; a non-object tools/call
	// result fails the call too.
	tests := []struct {
		name string
		// initialize is the initialize reply's members after its id; empty
		// for an ordinary reply.
		initialize string
		toolResult string
		wantErr    string
		wantPosts  []string
	}{
		{"initialize rejected", `"error":{"code":-32602,"message":"unsupported capabilities"}`, "", "mcp: initialize rejected: rpc error -32602: unsupported capabilities", []string{"initialize"}},
		{"initialize result not an object", `"result":"oops"`, "", "mcp: decoding initialize result:", []string{"initialize"}},
		{"tool result not an object", "", `"oops"`, "mcp: decoding tool result:", []string{"initialize", "notifications/initialized", "tools/call"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			var mu sync.Mutex
			var posts []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := decodeRPC(t, r)
				mu.Lock()
				posts = append(posts, req.Method)
				mu.Unlock()
				switch {
				case req.Method == "initialize" && tt.initialize != "":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,%s}`, deref(req.ID), tt.initialize)
				case req.Method == "initialize":
					writeInitialize(w, deref(req.ID), "")
				case req.Method == "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				default:
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, deref(req.ID), tt.toolResult)
				}
			}))
			defer server.Close()

			_, err := call(newClient(t, server.URL))
			must.Error(err, "CallTool should fail")
			must.Truef(strings.HasPrefix(err.Error(), tt.wantErr), "error = %v, want prefix %q", err, tt.wantErr)

			mu.Lock()
			defer mu.Unlock()
			must.Equal(tt.wantPosts, posts, "requests sent to the server")
		})
	}
}

func TestCallTool_OversizedReplyFailsAtBodyBound(t *testing.T) {
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
			must := require.New(t)

			fake := mcpclienttest.NewFakeServer(nil, tt.opts...)
			defer fake.Close()

			c := newClient(t, fake.URL(), func(o *mcpclient.Options) { o.MaxBodyBytes = 512 })
			_, err := call(c)
			must.Error(err, "CallTool should fail when the response exceeds MaxBodyBytes")
			must.ErrorContains(err, "512-byte bound", "want a body-bound error")
		})
	}
}

func TestCallTool_EmptyNameSendsNoRequest(t *testing.T) {
	must := require.New(t)
	fake := mcpclienttest.NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.CallTool(context.Background(), "", nil)
	must.Error(err, "CallTool with an empty tool name should fail")
	must.Zero(fake.CallCount(), "validation must precede any request")
}

func TestCallTool_KeylessClientSendsNoAuthorization(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fake := mcpclienttest.NewFakeServer(textResult("ok"))
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *mcpclient.Options) { o.APIKey = "" })
	_, err := call(c)
	must.NoError(err, "CallTool")
	for i, req := range fake.Requests() {
		check.Empty(req.Authorization, "request[%d] authorization on a keyless client", i)
	}
}

func TestCallTool_ErrorTextBoundedAndScrubbed(t *testing.T) {
	// Every server-supplied error text is scrubbed and cut to 4 KiB, so an
	// adapter's error never carries megabytes into a caller's transcript.
	must := require.New(t)

	huge := "backend failed for key " + testKey + "\n" + strings.Repeat("é", 200<<10)
	tests := []struct {
		name          string
		status        int
		body          string
		wantErr       string
		wantTruncated bool
	}{
		{"tools/call rejection", http.StatusOK, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":%q}}`, huge), "tools/call rejected", true},
		{"error body", http.StatusBadGateway, huge, "tools/call failed: HTTP 502: backend failed", true},
		{"non-JSON error body over the read bound", http.StatusBadGateway, strings.Repeat("<html>", (1<<20)/5), "tools/call failed: HTTP 502", false},
		{"empty error body", http.StatusBadGateway, "", "tools/call failed: HTTP 502", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

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
			must.Error(err, "CallTool should fail")
			msg := err.Error()
			must.ErrorContains(err, tt.wantErr)
			must.NotContains(msg, testKey, "error should have the key redacted")
			must.LessOrEqual(len(msg), (4<<10)+64, "error should be at most the 4 KiB bound plus its prefix")
			must.True(utf8.ValidString(msg), "error should be valid UTF-8")
			must.Equal(tt.wantTruncated, strings.HasSuffix(msg, " [truncated]"), "truncation marker")
		})
	}

	fake := mcpclienttest.NewFakeServer(textErrorHandler(huge))
	defer fake.Close()
	got, err := call(newClient(t, fake.URL()))
	must.NoError(err, "CallTool")
	text := mcpclient.FirstText(got.Content)
	must.True(got.IsError, "tool error should be flagged")
	must.LessOrEqual(len(text), 4<<10, "tool error text should be at most 4 KiB")
	must.True(strings.HasSuffix(text, " [truncated]"), "tool error text should end in the truncation marker")
	must.NotContains(text, testKey, "tool error text should have the key redacted")
}
