package search

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
)

// FakeServer is an httptest-backed stand-in for a Streamable-HTTP MCP search
// server, shared by every package that drives a search in tests
// (docs/V2-RESEARCH-AGENT §8 requires a fake search MCP for CI). It answers
// initialize / notifications/initialized / tools/call with scripted results,
// accepts a session DELETE, records the requests it received, and never
// touches the real network. Callers own its lifecycle: build it at the call
// site and defer Close.
//
// The zero value is not usable; construct with NewFakeServer. Point a Client
// at it via Options{Endpoint: fake.URL(), ...}; the Client sends every request
// to that URL, which the fake serves.
type FakeServer struct {
	server *httptest.Server

	// sessionID, when non-empty, is returned on the initialize reply as the
	// Mcp-Session-Id header; the fake then requires it on tools/call and
	// DELETE. Empty models a stateless server.
	sessionID string
	// protocolVersion is the revision the initialize result names.
	protocolVersion string
	// useSSE returns tools/call as a text/event-stream reply instead of
	// application/json, exercising the SSE read path.
	useSSE bool

	mu       sync.Mutex
	results  []Result
	rawTool  string // when set, the verbatim tools/call result JSON (overrides results)
	requests []FakeRequest
}

// FakeRequest is one request the fake received, exposed so tests can assert
// on the method, the credential, the transport headers, and the tools/call
// arguments.
type FakeRequest struct {
	// HTTPMethod is the HTTP method: POST for JSON-RPC, DELETE to end a
	// session.
	HTTPMethod string
	// Method is the JSON-RPC method ("initialize", "notifications/initialized",
	// "tools/call"); "" for a DELETE.
	Method string
	// Authorization is the raw Authorization header value.
	Authorization string
	// SessionID is the Mcp-Session-Id request header value ("" when absent).
	SessionID string
	// ProtocolVersion is the MCP-Protocol-Version request header value (""
	// when absent).
	ProtocolVersion string
	// ToolName is the tools/call params.name ("" for other methods).
	ToolName string
	// Arguments is the tools/call params.arguments (nil for other methods).
	Arguments map[string]interface{}
}

// FakeOption configures a FakeServer at construction.
type FakeOption func(*FakeServer)

// WithSessionID makes the fake assign a session on initialize and require it
// on tools/call and DELETE, modelling a stateful server.
func WithSessionID(id string) FakeOption {
	return func(f *FakeServer) { f.sessionID = id }
}

// WithProtocolVersion makes the fake's initialize result name version instead
// of the client's default revision, modelling a server that negotiates down.
func WithProtocolVersion(version string) FakeOption {
	return func(f *FakeServer) { f.protocolVersion = version }
}

// WithSSE makes the fake return tools/call as a text/event-stream reply,
// exercising the SSE read path. initialize is always plain JSON.
func WithSSE() FakeOption {
	return func(f *FakeServer) { f.useSSE = true }
}

// WithRawToolResult makes the fake return the given verbatim JSON as the
// tools/call result object (bypassing the scripted results), for
// unexpected-shape and oversized-body cases.
func WithRawToolResult(raw string) FakeOption {
	return func(f *FakeServer) { f.rawTool = raw }
}

// NewFakeServer starts a fake MCP search server that answers tools/call with
// the given results. Call Close when done.
func NewFakeServer(results []Result, opts ...FakeOption) *FakeServer {
	f := &FakeServer{results: results, protocolVersion: mcpProtocolVersion}
	for _, o := range opts {
		o(f)
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

// URL is the endpoint to pass as Options.Endpoint.
func (f *FakeServer) URL() string { return f.server.URL }

// Close shuts the server down. Callers should defer this at the call site.
func (f *FakeServer) Close() { f.server.Close() }

// Requests returns a copy of the requests the fake has received, in arrival
// order.
func (f *FakeServer) Requests() []FakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// CallCount reports how many requests the fake has received across the whole
// flow: initialize + initialized + tools/call = 3 for one successful Search,
// plus the closing DELETE when the fake issues a session.
func (f *FakeServer) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// ToolCallCount reports how many tools/call requests the fake has received —
// the count that must be exactly 1 per Search to prove the billable call is
// not retried.
func (f *FakeServer) ToolCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Method == "tools/call" {
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
		case rec.SessionID != f.sessionID:
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}

	var req rpcRequest
	// Decode best-effort: the Client always sends valid JSON-RPC.
	_ = json.NewDecoder(r.Body).Decode(&req)
	rec.Method = req.Method
	// params arrives as generic JSON, so name/arguments are pulled out
	// without the client's param structs.
	if req.Method == "tools/call" {
		if name, args, ok := decodeCallParams(req.Params); ok {
			rec.ToolName = name
			rec.Arguments = args
		}
	}
	f.record(rec)

	switch req.Method {
	case "initialize":
		if f.sessionID != "" {
			w.Header().Set(mcpSessionHeader, f.sessionID)
		}
		result, err := json.Marshal(map[string]any{
			"protocolVersion": f.protocolVersion,
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]string{"name": "fake-search", "version": "0"},
		})
		if err != nil {
			panic(fmt.Sprintf("fake: encoding initialize result: %v", err))
		}
		f.writeJSON(w, rpcResponse{
			JSONRPC: jsonRPCVersion,
			ID:      json.RawMessage(strconv.Itoa(deref(req.ID))),
			Result:  result,
		})
	case "notifications/initialized":
		// A notification: acknowledge with 202 and no body.
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		if f.sessionID != "" && rec.SessionID != f.sessionID {
			// A stateful server rejects a tools/call missing its session.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"missing session"}}`))
			return
		}
		f.writeToolResult(w, req.ID)
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

// writeToolResult writes the scripted tools/call result, as either plain JSON
// or an SSE frame depending on the fake's configuration.
func (f *FakeServer) writeToolResult(w http.ResponseWriter, id *int) {
	toolResult := f.rawTool
	if toolResult == "" {
		toolResult = f.buildResultsJSON()
	}
	resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, deref(id), toolResult)

	if f.useSSE {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// One SSE frame carrying the JSON-RPC response on a data: line.
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(resp))
}

// buildResultsJSON serialises the scripted results into the assumed tool
// result shape: a text content block whose text is the results document.
func (f *FakeServer) buildResultsJSON() string {
	doc := resultsDoc{}
	for _, r := range f.results {
		doc.Results = append(doc.Results, struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
		}{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
	}
	docJSON, err := json.Marshal(doc)
	if err != nil {
		panic(fmt.Sprintf("fake: marshalling results: %v", err))
	}
	block, err := json.Marshal(string(docJSON))
	if err != nil {
		panic(fmt.Sprintf("fake: marshalling text block: %v", err))
	}
	return fmt.Sprintf(`{"content":[{"type":"text","text":%s}],"isError":false}`, block)
}

func (f *FakeServer) writeJSON(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		panic(fmt.Sprintf("fake: encoding reply: %v", err))
	}
}

// decodeCallParams pulls name and arguments out of a tools/call params value
// that arrived as generic JSON.
func decodeCallParams(params interface{}) (string, map[string]interface{}, bool) {
	m, ok := params.(map[string]interface{})
	if !ok {
		return "", nil, false
	}
	name, _ := m["name"].(string)
	args, _ := m["arguments"].(map[string]interface{})
	return name, args, true
}

func deref(id *int) int {
	if id == nil {
		return 0
	}
	return *id
}
