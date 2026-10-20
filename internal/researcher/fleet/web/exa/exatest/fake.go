// Package exatest ships exa.Client's scripted test double. It is a separate
// package from internal/researcher/fleet/web/exa so net/http/httptest,
// needed only to script the double, never links into the chiron binary.
package exatest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa"
)

// Paths mirror exa's unexported request paths; the fake speaks the wire
// protocol rather than reaching into the client.
const (
	searchPath   = "/search"
	contentsPath = "/contents"
)

// Default costs, matching Exa's prices for an auto search of up to ten
// results and for one page's text.
const (
	defaultSearchCost = 0.007
	defaultPageCost   = 0.001
)

// maxRecordedBody bounds the request body the fake records.
const maxRecordedBody = 1 << 20

// FakeServer is an httptest-backed stand-in for Exa's POST /search and
// POST /contents, shared by every package that drives the Exa provider in
// tests. It answers with scripted results and pages, records each request,
// and never touches the real network. Like Exa, it answers a request
// without an x-api-key header with a 402 payment challenge. Callers own its
// lifecycle: build it at the call site and defer Close.
//
// Point a Client at it via exa.Options{Endpoint: fake.URL(), ...}.
type FakeServer struct {
	server *httptest.Server

	results    []web.Result
	pages      map[string]string
	failed     map[string]string
	cost       *float64
	status     int
	statusBody string
	headers    http.Header
	redirectTo string
	rawBody    string
	oversize   int

	mu       sync.Mutex
	requests []FakeRequest
}

// FakeRequest is one request the fake received.
type FakeRequest struct {
	Method        string
	Path          string
	APIKey        string
	Authorization string
	ContentType   string
	UserAgent     string
	// Body is the request body as sent, for asserting the search options,
	// the objective and the contents request.
	Body json.RawMessage
}

// FakeOption configures a FakeServer at construction.
type FakeOption func(*FakeServer)

// WithResults sets the search results, served with each Snippet as the
// result's one highlight.
func WithResults(rs []web.Result) FakeOption {
	return func(f *FakeServer) { f.results = rs }
}

// WithPages sets the pages /contents serves, keyed by URL exactly as
// requested and holding the page text. A requested URL with no page fails
// with CRAWL_NOT_FOUND.
func WithPages(pages map[string]string) FakeOption {
	return func(f *FakeServer) {
		for u, text := range pages {
			f.pages[u] = text
		}
	}
}

// WithFailedURL makes /contents report url as an error with tag. A
// CRAWL_HTTP_<n> tag reports target status n; CRAWL_NOT_FOUND and
// CONTENT_NOT_CACHED report 404.
func WithFailedURL(url, tag string) FakeOption {
	return func(f *FakeServer) { f.failed[url] = tag }
}

// WithCostDollars sets costDollars.total on every successful reply. Without
// it a search costs 0.007 and a contents reply 0.001 per page returned.
func WithCostDollars(total float64) FakeOption {
	return func(f *FakeServer) { f.cost = &total }
}

// WithStatus makes the fake answer every keyed request with code and body.
func WithStatus(code int, body string) FakeOption {
	return func(f *FakeServer) { f.status, f.statusBody = code, body }
}

// WithHeader adds a response header to every reply.
func WithHeader(key, value string) FakeOption {
	return func(f *FakeServer) { f.headers.Add(key, value) }
}

// WithRedirect makes the fake answer every keyed request with a 307 to
// location.
func WithRedirect(location string) FakeOption {
	return func(f *FakeServer) { f.redirectTo = location }
}

// WithRawBody makes the fake answer every keyed request with the given
// verbatim body (200, application/json), for fixtures and malformed or
// unexpected shapes.
func WithRawBody(raw string) FakeOption {
	return func(f *FakeServer) { f.rawBody = raw }
}

// WithOversizedBody makes the fake answer with a well-formed JSON object of
// at least n bytes, for exercising the client's body bound.
func WithOversizedBody(n int) FakeOption {
	return func(f *FakeServer) { f.oversize = n }
}

// NewFakeServer starts a fake Exa API. Call Close when done.
func NewFakeServer(opts ...FakeOption) *FakeServer {
	f := &FakeServer{headers: http.Header{}, pages: map[string]string{}, failed: map[string]string{}}
	for _, o := range opts {
		o(f)
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

// URL is the endpoint to pass as exa.Options.Endpoint.
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
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxRecordedBody))
	f.record(FakeRequest{
		Method:        r.Method,
		Path:          r.URL.Path,
		APIKey:        r.Header.Get("x-api-key"),
		Authorization: r.Header.Get("Authorization"),
		ContentType:   r.Header.Get("Content-Type"),
		UserAgent:     r.Header.Get("User-Agent"),
		Body:          json.RawMessage(body),
	})
	for k, vs := range f.headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}

	switch {
	case r.Header.Get("x-api-key") == "":
		writeJSON(w, http.StatusPaymentRequired, exa.ErrorResponse{RequestID: "fake-request", Error: "Payment required to access this resource", Tag: "X402_PAYMENT_REQUIRED"})
	case f.redirectTo != "":
		http.Redirect(w, r, f.redirectTo, http.StatusTemporaryRedirect)
	case f.status != 0:
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.statusBody)
	case r.Method != http.MethodPost || (r.URL.Path != searchPath && r.URL.Path != contentsPath):
		http.NotFound(w, r)
	case f.oversize > 0:
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"results":[],"pad":%q}`, strings.Repeat("x", f.oversize))
	case f.rawBody != "":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, f.rawBody)
	case r.URL.Path == searchPath:
		writeJSON(w, http.StatusOK, f.search(body))
	default:
		writeJSON(w, http.StatusOK, f.contents(body))
	}
}

func (f *FakeServer) search(body []byte) exa.SearchResponse {
	var req struct {
		NumResults int `json:"numResults"`
	}
	_ = json.Unmarshal(body, &req)
	doc := exa.SearchResponse{RequestID: "fake-request", Results: []exa.SearchResult{}}
	for _, r := range f.results {
		if req.NumResults > 0 && len(doc.Results) == req.NumResults {
			break
		}
		res := exa.SearchResult{ID: r.URL, Title: r.Title, URL: r.URL}
		if r.Snippet != "" {
			res.Highlights = []string{r.Snippet}
		}
		doc.Results = append(doc.Results, res)
	}
	doc.CostDollars = &exa.CostDollars{Total: defaultSearchCost}
	if f.cost != nil {
		doc.CostDollars.Total = *f.cost
	}
	return doc
}

func (f *FakeServer) contents(body []byte) exa.ContentsResponse {
	var req struct {
		URLs []string `json:"urls"`
		Text struct {
			MaxCharacters int `json:"maxCharacters"`
		} `json:"text"`
	}
	_ = json.Unmarshal(body, &req)
	doc := exa.ContentsResponse{RequestID: "fake-request", Results: []exa.SearchResult{}, Statuses: []exa.ContentStatus{}}
	pages := 0
	for _, u := range req.URLs {
		tag, failed := f.failed[u]
		text, found := f.pages[u]
		switch {
		case failed:
		case found:
			doc.Results = append(doc.Results, exa.SearchResult{ID: u, URL: u, Text: cutRunes(text, req.Text.MaxCharacters)})
			doc.Statuses = append(doc.Statuses, exa.ContentStatus{ID: u, Status: "success", Source: "cached"})
			pages++
			continue
		default:
			tag = "CRAWL_NOT_FOUND"
		}
		doc.Statuses = append(doc.Statuses, exa.ContentStatus{ID: u, Status: "error", Error: &exa.ContentError{Tag: tag, HTTPStatusCode: targetStatus(tag)}})
	}
	doc.CostDollars = &exa.CostDollars{Total: defaultPageCost * float64(pages)}
	if f.cost != nil {
		doc.CostDollars.Total = *f.cost
	}
	return doc
}

// targetStatus is the target page status Exa reports beside tag.
func targetStatus(tag string) *int {
	status := 0
	switch {
	case tag == "CRAWL_NOT_FOUND", tag == "CONTENT_NOT_CACHED":
		status = http.StatusNotFound
	case strings.HasPrefix(tag, "CRAWL_HTTP_"):
		status, _ = strconv.Atoi(strings.TrimPrefix(tag, "CRAWL_HTTP_"))
	}
	if status == 0 {
		return nil
	}
	return &status
}

// cutRunes cuts s to at most n runes; n <= 0 leaves it whole.
func cutRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

func (f *FakeServer) record(rec FakeRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, rec)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("exatest: encoding response: %v", err))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}
