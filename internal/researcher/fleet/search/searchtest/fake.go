// Package searchtest ships search.Client's scripted test double. It is a
// separate package from internal/researcher/fleet/search so
// net/http/httptest, needed only to script the double via mcpclienttest,
// never links into the chiron binary.
package searchtest

import (
	"encoding/json"
	"fmt"

	"github.com/rxbynerd/chiron/internal/mcpclient"
	"github.com/rxbynerd/chiron/internal/mcpclient/mcpclienttest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
)

// FakeServer is an httptest-backed stand-in for a Streamable-HTTP MCP search
// server, shared by every package that drives a search in tests
// (docs/V2-RESEARCH-AGENT §8 requires a fake search MCP for CI). It is an
// mcpclienttest.FakeServer scripted to answer every tools/call with the given
// results as a text block holding the results document. Callers own its
// lifecycle: build it at the call site and defer Close.
//
// The zero value is not usable; construct with NewFakeServer. Point a Client
// at it via search.Options{Endpoint: fake.URL(), ...}.
type FakeServer struct {
	inner   *mcpclienttest.FakeServer
	results []search.Result
	opts    []mcpclienttest.FakeOption
}

// FakeRequest is one request the fake received, exposed so tests can assert
// on the method, the credential, the transport headers, and the tools/call
// arguments.
type FakeRequest = mcpclienttest.FakeRequest

// FakeOption configures a FakeServer at construction.
type FakeOption func(*FakeServer)

// WithSessionID makes the fake assign a session on initialize and require it
// on tools/call and DELETE, modelling a stateful server.
func WithSessionID(id string) FakeOption {
	return func(f *FakeServer) { f.opts = append(f.opts, mcpclienttest.WithSessionID(id)) }
}

// WithProtocolVersion makes the fake's initialize result name version instead
// of the client's default revision, modelling a server that negotiates down.
func WithProtocolVersion(version string) FakeOption {
	return func(f *FakeServer) { f.opts = append(f.opts, mcpclienttest.WithProtocolVersion(version)) }
}

// WithSSE makes the fake return tools/call as a text/event-stream reply,
// exercising the SSE read path. initialize is always plain JSON.
func WithSSE() FakeOption {
	return func(f *FakeServer) { f.opts = append(f.opts, mcpclienttest.WithSSE()) }
}

// WithRawToolResult makes the fake return the given verbatim JSON as the
// tools/call result object (bypassing the scripted results), for
// unexpected-shape and oversized-body cases.
func WithRawToolResult(raw string) FakeOption {
	return func(f *FakeServer) { f.opts = append(f.opts, mcpclienttest.WithRawResult(raw)) }
}

// NewFakeServer starts a fake MCP search server that answers tools/call with
// the given results. Call Close when done.
func NewFakeServer(results []search.Result, opts ...FakeOption) *FakeServer {
	f := &FakeServer{results: results}
	for _, o := range opts {
		o(f)
	}
	f.inner = mcpclienttest.NewFakeServer(f.answer, f.opts...)
	return f
}

// URL is the endpoint to pass as Options.Endpoint.
func (f *FakeServer) URL() string { return f.inner.URL() }

// Close shuts the server down. Callers should defer this at the call site.
func (f *FakeServer) Close() { f.inner.Close() }

// Requests returns a copy of the requests the fake has received, in arrival
// order.
func (f *FakeServer) Requests() []FakeRequest { return f.inner.Requests() }

// CallCount reports how many requests the fake has received across the whole
// flow: initialize + initialized + tools/call = 3 for one successful Search,
// plus the closing DELETE when the fake issues a session.
func (f *FakeServer) CallCount() int { return f.inner.CallCount() }

// ToolCallCount reports how many tools/call requests the fake has received —
// the count that must be exactly 1 per Search to prove the billable call is
// not retried.
func (f *FakeServer) ToolCallCount() int { return f.inner.ToolCallCount() }

// answer serialises the scripted results into the assumed tool result shape:
// a text content block whose text is the results document.
func (f *FakeServer) answer(string, map[string]any) (mcpclient.ToolResult, error) {
	doc := resultsDoc{Results: make([]resultItem, 0, len(f.results))}
	for _, r := range f.results {
		doc.Results = append(doc.Results, resultItem(r))
	}
	text, err := json.Marshal(doc)
	if err != nil {
		return mcpclient.ToolResult{}, fmt.Errorf("fake: marshalling results: %w", err)
	}
	return mcpclient.ToolResult{Content: []mcpclient.ContentBlock{{Type: "text", Text: string(text)}}}, nil
}

// resultsDoc and resultItem mirror the search tool's assumed result shape
// (search.parseResults documents it): a fake speaks the wire protocol
// directly rather than reaching into search's unexported types.
type resultsDoc struct {
	Results []resultItem `json:"results"`
}

type resultItem struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}
