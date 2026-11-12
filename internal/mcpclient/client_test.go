package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testKey = "sk-mcp-0123456789abcdefABCDEF"

// newClient builds a Client pointed at an endpoint, failing the test on a
// construction error.
func newClient(t *testing.T, endpoint string, mutate ...func(*Options)) *Client {
	t.Helper()
	must := require.New(t)
	opts := Options{Endpoint: endpoint, APIKey: testKey}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := New(opts)
	must.NoError(err, "New")
	return c
}

// call runs one CallTool with a fixed tool and argument set.
func call(c *Client) (ToolResult, error) {
	return c.CallTool(context.Background(), "lookup", map[string]any{"query": "q"})
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
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q,"capabilities":{},"serverInfo":{"name":"test","version":"0"}}}`, deref(req.ID), ProtocolVersion)
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
	check := assert.New(t)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, f := range frames {
		if _, err := fmt.Fprint(w, f); err != nil {
			check.Fail("writing SSE frame", err.Error())
		}
	}
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func TestCallTool_Headers_AcceptsBothFramingsAndSendsClientInfo(t *testing.T) {
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
			must := require.New(t)
			check := assert.New(t)
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
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q}}`, deref(req.ID), ProtocolVersion)
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				default:
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[]}}`, deref(req.ID))
				}
			}))
			defer server.Close()

			c := newClient(t, server.URL, tt.mutate)
			_, err := call(c)
			must.NoError(err, "CallTool")
			mu.Lock()
			defer mu.Unlock()
			for i := range accepts {
				check.Equal("application/json, text/event-stream", accepts[i], "request %d Accept", i)
				check.Equal("application/json", contentTypes[i], "request %d Content-Type", i)
			}
			must.Equal(tt.wantName, info.Name, "clientInfo name")
			must.Equal(tt.wantVersion, info.Version, "clientInfo version")
		})
	}
}

func TestClose_IgnoresFailedSessionDelete(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err, "CallTool")
	check.Equal("done", FirstText(got.Content), "result = %+v", got)
	must.NoError(c.Close(), "Close should ignore a failed session DELETE")
	check.EqualValues(1, deletes.Load(), "DELETE count")
}

func TestCallTool_RPCError_IsScrubbedAndNotRetried(t *testing.T) {
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
			must := require.New(t)
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
			must.Error(err, "CallTool should fail on a JSON-RPC error reply")
			must.True(strings.HasPrefix(err.Error(), "mcp: tools/call rejected"), "error = %v", err)
			must.ErrorContains(err, tt.wantErr)
			must.NotContains(err.Error(), testKey, "error leaked the API key")
			must.EqualValues(1, toolCalls.Load(), "tools/call count")
		})
	}
}

func TestCallTool_MismatchedResponseID_IsRejected(t *testing.T) {
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
			must := require.New(t)
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
			must.Error(err, "CallTool should reject a reply with a mismatched id")
			must.ErrorContains(err, "does not answer request id 2")
		})
	}
}

func TestCallTool_SSE_RejoinsMultiLineData(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err, "CallTool over multi-line SSE data")
	must.Equal("multi", FirstText(got.Content), "result = %+v", got)
}

func TestCallTool_SSE_SkipsServerMessagesBeforeReply(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	elapsed := time.Since(start)
	must.NoError(err, "CallTool")
	check.LessOrEqual(elapsed, time.Second, "CallTool took %s; it should return on the reply frame, not at end of stream", elapsed)
	must.Equal("after", FirstText(got.Content), "result = %+v", got)
}

func TestCallTool_SSE_StreamBoundedInAggregate(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.Error(err, "CallTool should fail when the SSE stream exceeds MaxBodyBytes")
	check.ErrorContains(err, "mcp: SSE stream exceeds 512-byte bound")
}

func TestCallTool_MalformedReply_Fails(t *testing.T) {
	// A tools/call reply body that is not JSON, or an event stream that is
	// malformed, has a line past the bound, or ends without a response, fails
	// the call.
	tests := []struct {
		name         string
		contentType  string
		body         string
		maxBodyBytes int64
		wantErr      string
	}{
		{"JSON body truncated", "application/json", `{"jsonrpc":"2.0","id":2,`, 0, "mcp: decoding response:"},
		{"SSE data not JSON", "text/event-stream", "data: {not json}\n\n", 0, "mcp: decoding SSE response:"},
		// The line must outgrow the scanner's 64 KiB initial buffer to reach
		// the per-line bound rather than the aggregate one.
		{"SSE line over the bound", "text/event-stream", "data: " + strings.Repeat("x", 200<<10), 100 << 10, "mcp: SSE event exceeds 102400-byte bound"},
		{"SSE stream without a response", "text/event-stream", ": keep-alive\n\n" + `data: {"jsonrpc":"2.0","method":"notifications/progress"}` + "\n\n", 0, "mcp: SSE stream carried no JSON-RPC response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, answered := answerHandshake(t, w, r); answered {
					return
				}
				w.Header().Set("Content-Type", tt.contentType)
				// The client may stop reading early, so a write error is expected.
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			c := newClient(t, server.URL, func(o *Options) { o.MaxBodyBytes = tt.maxBodyBytes })
			_, err := call(c)
			must.Error(err, "CallTool should fail on a malformed reply")
			must.True(strings.HasPrefix(err.Error(), tt.wantErr), "error = %.200v, want prefix %q", err, tt.wantErr)
		})
	}
}

func TestCallTool_Redirect_CrossHostRefused(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Fail("redirect target was called", "the key would have leaked to %s", r.Host)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer elsewhere.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	c := newClient(t, redirector.URL)
	_, err := call(c)
	must.Error(err, "CallTool should fail rather than follow a cross-host redirect")
	must.ErrorContains(err, "cross-origin")
}

func TestCallTool_Redirect_CallerPolicyOverridden(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// New must impose its own redirect policy even when the caller's
	// http.Client already carries a permissive one.
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer target.Close()

	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	permissive := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	c := newClient(t, origin.URL, func(o *Options) { o.HTTPClient = permissive })
	_, err := call(c)
	must.Error(err, "CallTool should refuse the redirect even though the caller's client permits it")
	must.ErrorContains(err, "cross-origin")
	check.EqualValues(1, originHits.Load(), "origin hits")
	check.EqualValues(0, targetHits.Load(), "target hits")
}

func TestCallTool_Redirect_HTTPSDowngradeRefused(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.Error(err, "CallTool should fail rather than follow an https-to-http redirect")
	must.ErrorContains(err, "downgrade")
	check.EqualValues(1, hits.Load(), "handler hits")
	check.EqualValues(1, conns.Load(), "connections; the cleartext redirect target must never be dialled")
}

func TestClose_Redirect_CrossHostRefused(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// Close's session DELETE carries the key and the session id, so it
	// refuses a cross-host redirect exactly as a POST does.
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Fail("the session DELETE followed a cross-host redirect", "redirect target was called with %s", r.Method)
	}))
	defer elsewhere.Close()

	var deletes atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
			http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set(mcpSessionHeader, "sess-redirect")
		req, answered := answerHandshake(t, w, r)
		if answered {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[]}}`, deref(req.ID))
	}))
	defer origin.Close()

	c := newClient(t, origin.URL)
	_, err := call(c)
	must.NoError(err, "CallTool")
	check.NoError(c.Close(), "Close")
	check.EqualValues(1, deletes.Load(), "origin DELETE count")
}

func TestNew_CallerHTTPClientNotMutated(t *testing.T) {
	must := require.New(t)
	hc := &http.Client{}
	_ = newClient(t, "https://mcp.example.com/", func(o *Options) { o.HTTPClient = hc })
	must.Nil(hc.CheckRedirect, "New set CheckRedirect on the caller's client; it must work on a copy")
}

func TestNew_EndpointCredentialsRejectedWithoutEcho(t *testing.T) {
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
			must := require.New(t)
			_, err := New(Options{Endpoint: tt.endpoint, APIKey: testKey})
			must.Error(err, "New(%q) should fail", tt.endpoint)
			must.ErrorContains(err, tt.wantErr)
			must.NotContains(err.Error(), password, "error echoed the credentials")
			must.NotContains(err.Error(), username, "error echoed the credentials")
		})
	}
}

func TestNew_EndpointValidation(t *testing.T) {
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
		{"query rejected", "https://mcp.example.com/mcp?key=x", true},
		{"empty query rejected", "https://mcp.example.com/mcp?", true},
		{"fragment rejected", "https://mcp.example.com/mcp#frag", true},
		{"empty fragment rejected", "https://mcp.example.com/mcp#", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			_, err := New(Options{Endpoint: tt.endpoint, APIKey: testKey})
			if !tt.wantErr {
				must.NoError(err, "New(%q)", tt.endpoint)
				return
			}
			must.Error(err, "New(%q) should fail", tt.endpoint)
			must.True(strings.HasPrefix(err.Error(), "mcp: "), "error = %v, want the mcp: prefix", err)
		})
	}
}

func TestCallTool_HTTPErrorNeverLeaksKey(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// A failing request must not surface the API key anywhere in the error,
	// even when the server echoes request context in its error body.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key ` + testKey + `"}`))
	}))
	defer server.Close()

	c := newClient(t, server.URL)
	_, err := call(c)
	must.Error(err, "CallTool should fail on a 401")
	must.NotContains(err.Error(), testKey, "error leaked the API key")
	check.ErrorContains(err, "mcp: initialize failed: HTTP 401")
}

func TestCallTool_KeylessErrorTextIntact(t *testing.T) {
	// A keyless client has no key to redact, so server text reaches an error
	// or a tool error unaltered, not interleaved with redaction markers.
	tests := []struct {
		name     string
		status   int
		reply    string
		wantErr  string
		wantText string
	}{
		{"error body", http.StatusBadGateway, "backend unavailable", "mcp: tools/call failed: HTTP 502: backend unavailable", ""},
		{"JSON-RPC error", http.StatusOK, `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"backend unavailable"}}`, "mcp: tools/call rejected: rpc error -32000: backend unavailable", ""},
		{"tool error", http.StatusOK, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"backend unavailable"}],"isError":true}}`, "", "backend unavailable"},
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
				_, _ = io.WriteString(w, tt.reply)
			}))
			defer server.Close()

			got, err := call(newClient(t, server.URL, func(o *Options) { o.APIKey = "" }))
			if tt.wantErr != "" {
				must.EqualError(err, tt.wantErr)
				return
			}
			must.NoError(err, "CallTool")
			must.True(got.IsError, "tool result should be flagged as an error")
			must.Equal(tt.wantText, FirstText(got.Content), "tool error text")
		})
	}
}

func TestCallTool_Deadline_CallerDeadlineBeatsRequestTimeout(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.Error(err, "CallTool should fail when the caller deadline fires")
	check.LessOrEqual(time.Since(start), 1500*time.Millisecond, "the caller's 100ms deadline should win over the 10s RequestTimeout")
	select {
	case <-aborted:
	case <-time.After(time.Second):
		check.Fail("the server never observed the request being aborted")
	}
}

func TestFirstText_ReturnsFirstNonEmptyText(t *testing.T) {
	tests := []struct {
		name   string
		blocks []ContentBlock
		want   string
	}{
		{"no blocks", nil, ""},
		{"skips non-text and empty", []ContentBlock{{Type: "image"}, {Type: "text"}, {Type: "text", Text: "b"}}, "b"},
		{"first wins", []ContentBlock{{Type: "text", Text: "a"}, {Type: "text", Text: "b"}}, "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.want, FirstText(tt.blocks))
		})
	}
}
