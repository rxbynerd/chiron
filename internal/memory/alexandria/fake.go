package alexandria

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
)

// FakeServer is an httptest-backed stand-in for Alexandria's GET /v1/search,
// shared by every package that drives recall in tests. It answers with a
// scripted SearchResponse (or raw body), records each request, and never
// touches the real network. Callers own its lifecycle: build it at the call
// site and defer Close.
//
// Construct with NewFakeServer and point a Client at it via
// Options{Endpoint: fake.URL(), ...}.
type FakeServer struct {
	server *httptest.Server

	doc        SearchResponse
	rawBody    string
	status     int
	statusBody string
	headers    http.Header
	redirectTo string
	oversize   int

	mu       sync.Mutex
	requests []FakeRequest
}

// FakeRequest is one request the fake received.
type FakeRequest struct {
	Method string
	Path   string
	// Query holds the decoded query parameters (q, mode, max_tokens, space).
	Query              url.Values
	Authorization      string
	Accept             string
	AccessClientID     string
	AccessClientSecret string
}

// FakeOption configures a FakeServer at construction.
type FakeOption func(*FakeServer)

// WithStatus makes the fake answer every request with code and body instead
// of the scripted document.
func WithStatus(code int, body string) FakeOption {
	return func(f *FakeServer) { f.status, f.statusBody = code, body }
}

// WithHeader adds a response header to every reply, e.g. Retry-After
// alongside WithStatus(429, ...).
func WithHeader(key, value string) FakeOption {
	return func(f *FakeServer) { f.headers.Add(key, value) }
}

// WithRedirect makes the fake answer every request with a 302 to location.
func WithRedirect(location string) FakeOption {
	return func(f *FakeServer) { f.redirectTo = location }
}

// WithRawBody makes the fake answer with the given verbatim body (200,
// application/json) instead of the scripted document, for malformed and
// unexpected-shape cases.
func WithRawBody(raw string) FakeOption {
	return func(f *FakeServer) { f.rawBody = raw }
}

// WithOversizedBody makes the fake answer with a well-formed JSON object of
// at least n bytes, for exercising the client's body bound.
func WithOversizedBody(n int) FakeOption {
	return func(f *FakeServer) { f.oversize = n }
}

// NewFakeServer starts a fake Alexandria search endpoint answering with doc.
// Call Close when done.
func NewFakeServer(doc SearchResponse, opts ...FakeOption) *FakeServer {
	f := &FakeServer{doc: doc, headers: http.Header{}}
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

func (f *FakeServer) handle(w http.ResponseWriter, r *http.Request) {
	f.record(FakeRequest{
		Method:             r.Method,
		Path:               r.URL.Path,
		Query:              r.URL.Query(),
		Authorization:      r.Header.Get("Authorization"),
		Accept:             r.Header.Get("Accept"),
		AccessClientID:     r.Header.Get("cf-access-client-id"),
		AccessClientSecret: r.Header.Get("cf-access-client-secret"),
	})
	for k, vs := range f.headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}

	switch {
	case f.redirectTo != "":
		http.Redirect(w, r, f.redirectTo, http.StatusFound)
	case f.status != 0:
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.statusBody))
	case r.Method != http.MethodGet || r.URL.Path != searchPath:
		http.NotFound(w, r)
	case f.oversize > 0:
		writeJSON(w, fmt.Sprintf(`{"results":[],"pad":%q}`, strings.Repeat("x", f.oversize)))
	case f.rawBody != "":
		writeJSON(w, f.rawBody)
	default:
		doc := f.doc
		if doc.Results == nil {
			doc.Results = []SearchResult{}
		}
		body, err := json.Marshal(doc)
		if err != nil {
			panic(fmt.Sprintf("fake: encoding search response: %v", err))
		}
		writeJSON(w, string(body))
	}
}

func (f *FakeServer) record(rec FakeRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, rec)
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}
