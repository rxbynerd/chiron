// Package mcpclienttest ships mcpclient.Client's scripted test double.
// It is a separate package from internal/mcpclient so net/http/httptest,
// needed only to script the double, never links into the chiron binary.
package mcpclienttest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/rxbynerd/chiron/internal/mcpclient"
)

// The MCP transport header names the fake reads and sets; mirrored from
// internal/mcpclient's unexported constants since the fake speaks the wire
// protocol directly rather than reaching across the package boundary.
const (
	mcpSessionHeader         = "Mcp-Session-Id"
	mcpProtocolVersionHeader = "MCP-Protocol-Version"
)

// FakeHandler scripts a FakeServer's tools/call replies. A returned error
// becomes a JSON-RPC error reply carrying its text; a ToolResult with IsError
// set models a tool-level failure.
type FakeHandler func(tool string, args map[string]any) (mcpclient.ToolResult, error)

// FakeServer is an httptest-backed Streamable-HTTP MCP server shared by every
// package that drives an MCP tool in tests. It answers initialize,
// notifications/initialized and tools/call, accepts a session DELETE, records
// the requests it received, and never touches the real network. Callers own
// its lifecycle: build it at the call site and defer Close.
type FakeServer struct {
	server  *httptest.Server
	handler FakeHandler

	// sessionID, when non-empty, names the sessions initialize issues; see
	// WithSessionID.
	sessionID           string
	protocolVersion     string
	omitProtocolVersion bool
	useSSE              bool
	rawResult           string

	mu       sync.Mutex
	requests []FakeRequest
	issued   int             // sessions issued so far
	live     map[string]bool // issued sessions not yet ended
}

// FakeRequest is one request the fake received.
type FakeRequest struct {
	// HTTPMethod is POST for JSON-RPC, DELETE to end a session.
	HTTPMethod string
	// Method is the JSON-RPC method ("initialize", "notifications/initialized",
	// "tools/call"); "" for a DELETE.
	Method string
	// Authorization is the raw Authorization header value.
	Authorization string
	// SessionID is the Mcp-Session-Id request header value ("" when absent).
	SessionID string
	// ID is the JSON-RPC request id (0 for a notification or a DELETE).
	ID int
	// ProtocolVersion is the MCP-Protocol-Version request header value (""
	// when absent).
	ProtocolVersion string
	// ToolName is the tools/call params.name ("" for other methods).
	ToolName string
	// Arguments is the tools/call params.arguments (nil for other methods).
	Arguments map[string]any
}

// FakeOption configures a FakeServer at construction.
type FakeOption func(*FakeServer)

// WithSessionID models a stateful server. Each initialize issues a new
// session, named id the first time and id-2, id-3 and so on after. Every
// later request must carry a live session: none is answered with HTTP 400,
// and one that DELETE or ExpireSession ended with HTTP 404.
func WithSessionID(id string) FakeOption {
	return func(f *FakeServer) { f.sessionID = id }
}

// WithProtocolVersion makes the fake's initialize result name version instead
// of ProtocolVersion, modelling a server that negotiates down or one choosing
// a revision the client does not support.
func WithProtocolVersion(version string) FakeOption {
	return func(f *FakeServer) { f.protocolVersion = version }
}

// WithoutProtocolVersion makes the fake's initialize result omit
// protocolVersion, which the MCP schema requires.
func WithoutProtocolVersion() FakeOption {
	return func(f *FakeServer) { f.omitProtocolVersion = true }
}

// WithSSE makes the fake return tools/call as a text/event-stream reply.
// initialize is always plain JSON.
func WithSSE() FakeOption {
	return func(f *FakeServer) { f.useSSE = true }
}

// WithRawResult makes the fake return raw verbatim as the tools/call result
// member, bypassing the handler, for malformed-shape and oversized-body cases.
func WithRawResult(raw string) FakeOption {
	return func(f *FakeServer) { f.rawResult = raw }
}

// NewFakeServer starts a fake MCP server whose tools/call replies come from
// handler. A nil handler answers every call with an empty result. Call Close
// when done.
func NewFakeServer(handler FakeHandler, opts ...FakeOption) *FakeServer {
	f := &FakeServer{handler: handler, protocolVersion: mcpclient.ProtocolVersion, live: map[string]bool{}}
	for _, o := range opts {
		o(f)
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

// URL is the endpoint to pass as Options.Endpoint.
func (f *FakeServer) URL() string { return f.server.URL }

// Close shuts the server down.
func (f *FakeServer) Close() { f.server.Close() }

// Requests returns a copy of the requests received, in arrival order.
func (f *FakeServer) Requests() []FakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// CallCount reports every request received: initialize + initialized +
// tools/call = 3 for a Client's first CallTool and 1 for each later one, plus
// the DELETE on Close when a session was issued.
func (f *FakeServer) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// ToolCallCount reports the tools/call requests received — exactly 1 per
// CallTool proves the call is not retried.
func (f *FakeServer) ToolCallCount() int {
	return f.count(func(r FakeRequest) bool { return r.Method == "tools/call" })
}

// InitializeCount reports the initialize requests received: 1 per Client
// lifetime unless a session ended.
func (f *FakeServer) InitializeCount() int {
	return f.count(func(r FakeRequest) bool { return r.Method == "initialize" })
}

// DeleteCount reports the session DELETE requests received.
func (f *FakeServer) DeleteCount() int {
	return f.count(func(r FakeRequest) bool { return r.HTTPMethod == http.MethodDelete })
}

// ExpireSession ends every session the fake has issued, as a server does on
// restart or idle timeout: a later request bearing one gets HTTP 404, and the
// next initialize issues a fresh session.
func (f *FakeServer) ExpireSession() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.live)
}

func (f *FakeServer) count(match func(FakeRequest) bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if match(r) {
			n++
		}
	}
	return n
}

func (f *FakeServer) handle(w http.ResponseWriter, r *http.Request) {
	rec := FakeRequest{
		HTTPMethod:      r.Method,
		Authorization:   r.Header.Get("Authorization"),
		SessionID:       r.Header.Get(mcpSessionHeader),
		ProtocolVersion: r.Header.Get(mcpProtocolVersionHeader),
	}

	if r.Method == http.MethodDelete {
		f.record(rec)
		switch {
		case f.sessionID == "":
			w.WriteHeader(http.StatusMethodNotAllowed)
		case !f.endSession(rec.SessionID):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}

	var req struct {
		ID     *int   `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	// Decode best-effort: the Client always sends valid JSON-RPC.
	_ = json.NewDecoder(r.Body).Decode(&req)
	rec.Method = req.Method
	rec.ID = deref(req.ID)
	if req.Method == "tools/call" {
		rec.ToolName = req.Params.Name
		rec.Arguments = req.Params.Arguments
	}
	f.record(rec)

	if req.Method != "initialize" && f.sessionID != "" {
		switch {
		case rec.SessionID == "":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"missing session"}}`))
			return
		case !f.sessionLive(rec.SessionID):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"session not found"}}`))
			return
		}
	}

	switch req.Method {
	case "initialize":
		if f.sessionID != "" {
			w.Header().Set(mcpSessionHeader, f.newSession())
		}
		result := map[string]any{
			"capabilities": map[string]any{},
			"serverInfo":   map[string]string{"name": "fake-mcp", "version": "0"},
		}
		if !f.omitProtocolVersion {
			result["protocolVersion"] = f.protocolVersion
		}
		f.writeJSON(w, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, deref(req.ID), mustJSON(result)))
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		f.writeToolResult(w, deref(req.ID), rec.ToolName, rec.Arguments)
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32601,"message":"method not found"}}`))
	}
}

func (f *FakeServer) record(rec FakeRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, rec)
}

func (f *FakeServer) newSession() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued++
	id := f.sessionID
	if f.issued > 1 {
		id = fmt.Sprintf("%s-%d", f.sessionID, f.issued)
	}
	f.live[id] = true
	return id
}

func (f *FakeServer) sessionLive(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[id]
}

// endSession ends a live session, reporting whether it was live.
func (f *FakeServer) endSession(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.live[id] {
		return false
	}
	delete(f.live, id)
	return true
}

// writeToolResult writes the tools/call reply as plain JSON or one SSE frame.
func (f *FakeServer) writeToolResult(w http.ResponseWriter, id int, tool string, args map[string]any) {
	var resp string
	switch {
	case f.rawResult != "":
		resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, id, f.rawResult)
	case f.handler == nil:
		resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[],"isError":false}}`, id)
	default:
		result, err := f.handler(tool, args)
		if err != nil {
			resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":%s}}`, id, mustJSON(err.Error()))
		} else {
			if result.Content == nil {
				result.Content = []mcpclient.ContentBlock{}
			}
			resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, id, mustJSON(result))
		}
	}

	if f.useSSE {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: message\ndata: "+resp+"\n\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		return
	}
	f.writeJSON(w, resp)
}

func (f *FakeServer) writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("mcpclient fake: encoding %T: %v", v, err))
	}
	return string(b)
}

func deref(id *int) int {
	if id == nil {
		return 0
	}
	return *id
}
