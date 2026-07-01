// Package search is a minimal, SDK-free net/http client for a web-search
// tool exposed over the Model Context Protocol (MCP) "Streamable HTTP"
// transport. It is one of the two read-only network tools a Chiron research
// worker may use (docs/V2-RESEARCH-AGENT §1, §5): the worker calls Search to
// discover candidate URLs, then hands them to the web_fetch client.
//
// MCP is JSON-RPC 2.0 carried over HTTP. This client implements only the
// minimum correct flow for a single search backend — initialize, an
// initialized notification, then a tools/call — and parses the tool result
// into a flat []Result. It is deliberately not a general MCP client: there
// is no resources/prompts/sampling surface, no server-initiated request
// handling, and no session resumption beyond echoing an Mcp-Session-Id.
//
// Money-safety and security follow the Gemini adapter (internal/interactions,
// internal/researcher/gemini) and the sibling model adapter
// (internal/researcher/fleet/model): every response body is bounded, a
// text/event-stream reply is read under the same bound, cross-host redirects
// carrying the credential are refused, and the search key travels only in the
// Authorization header — never a URL, query, log, error, or trace.
package search

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Defaults, all overridable via Options. The body bound is generous — a
// verbose result set with long snippets stays well under it — so a
// misbehaving server cannot exhaust memory, not that a normal response ever
// approaches it.
const (
	defaultRequestTimeout = 30 * time.Second
	defaultMaxBodyBytes   = 8 << 20
	maxErrorBodyBytes     = 1 << 20

	defaultToolName    = "search"
	defaultQueryArgKey = "query"

	// mcpProtocolVersion is the protocol revision advertised in initialize.
	// It is a date-stamped string per the MCP spec; a server that speaks a
	// different revision still answers, and this client only relies on the
	// JSON-RPC envelope, so a mismatch is tolerated rather than fatal.
	mcpProtocolVersion = "2025-06-18"
)

// Options configures a Client.
type Options struct {
	// Endpoint is the MCP server's Streamable-HTTP URL, e.g.
	// "https://search.example.com/mcp". Required. It must be an absolute
	// https:// URL, with http:// admitted for loopback hosts only
	// (127.0.0.1, ::1, localhost) — the credential travels to whatever
	// endpoint is set, so a cleartext or internal override would be a
	// key-exfiltration and SSRF channel (CWE-918, CWE-319).
	Endpoint string
	// APIKey is the ALREADY-RESOLVED literal key. This client never resolves
	// secret:// references itself — resolution happens at the CLI composition
	// root. It is sent only in the Authorization header. It may be empty for
	// a keyless server; when empty no Authorization header is sent.
	APIKey string
	// HTTPClient supplies the underlying client. nil builds one. A
	// caller-supplied client is shallow-copied so the cross-host redirect
	// policy can be set without mutating the caller's client; leave its
	// Timeout zero — per-call deadlines come from RequestTimeout.
	HTTPClient *http.Client
	// RequestTimeout bounds each Search call — spanning the initialize,
	// initialized and tools/call round-trips — including reading the bodies.
	// Default 30s. A caller-supplied context deadline still wins if tighter.
	RequestTimeout time.Duration
	// MaxBodyBytes bounds how much of any single response body is read before
	// failing, whether the reply is application/json or text/event-stream.
	// Default 8 MiB.
	MaxBodyBytes int64
	// ToolName is the MCP tool to invoke in tools/call. Default "search".
	ToolName string
	// QueryArgKey is the argument key the search query is passed under in the
	// tools/call arguments object. Default "query".
	QueryArgKey string
}

// Client is a hand-rolled net/http MCP search client. The API key travels
// only in the Authorization header: it is never embedded in URLs, logged, or
// included in error text.
type Client struct {
	endpoint       string
	apiKey         string
	httpClient     *http.Client
	requestTimeout time.Duration
	maxBodyBytes   int64
	toolName       string
	queryArgKey    string
}

// New builds a Client, validating the endpoint before any request is made.
// The endpoint is validated here defensively even though config validates it
// too — the client is a reusable seam and must not trust its caller to have
// checked.
func New(opts Options) (*Client, error) {
	if err := validateEndpoint(opts.Endpoint); err != nil {
		return nil, err
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	// Go's net/http strips only its own sensitive headers (Authorization,
	// Cookie, ...) on cross-domain redirects, and following a same-scheme
	// cross-host redirect would still hand the Authorization header to the
	// new host. The same-host policy is set on a shallow copy (sharing the
	// caller's Transport, jar and timeout), so supplying a bare client via
	// Options.HTTPClient cannot lose the guarantee.
	hc := *httpClient
	hc.CheckRedirect = refuseCrossHostRedirects
	httpClient = &hc

	requestTimeout := opts.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	maxBodyBytes := opts.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultMaxBodyBytes
	}
	toolName := opts.ToolName
	if toolName == "" {
		toolName = defaultToolName
	}
	queryArgKey := opts.QueryArgKey
	if queryArgKey == "" {
		queryArgKey = defaultQueryArgKey
	}

	return &Client{
		endpoint:       strings.TrimSuffix(opts.Endpoint, "/"),
		apiKey:         opts.APIKey,
		httpClient:     httpClient,
		requestTimeout: requestTimeout,
		maxBodyBytes:   maxBodyBytes,
		toolName:       toolName,
		queryArgKey:    queryArgKey,
	}, nil
}

// Result is one search hit. The three fields are the ones the worker loop
// needs to reason about a candidate source and later fetch it; the assumed
// tool-result shape carrying them is documented on parseResults and in
// docs/DECISIONS.md.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// validateEndpoint applies the CHIRON_GEMINI_BASE_URL rule: an absolute
// https:// URL, with http:// admitted for loopback hosts only. This mirrors
// model.validateEndpoint and allowedEndpointScheme in internal/config
// (validated there too); it is re-applied here because the client is a
// reusable seam that must not depend on config having run. The error echoes
// only the endpoint, never a credential.
func validateEndpoint(raw string) error {
	if raw == "" {
		return errors.New("search: endpoint must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !allowedEndpointScheme(u) {
		return fmt.Errorf("search: endpoint %q must be an absolute https:// URL (http:// only for loopback test servers)", raw)
	}
	return nil
}

// allowedEndpointScheme admits https anywhere and http on loopback only — the
// same rule model.allowedEndpointScheme, internal/config, and internal/cli
// enforce, mirrored here because this seam cannot import those without
// depending on the CLI/config layer. The duplication is noted in
// docs/DECISIONS.md as acceptable pending a shared helper.
func allowedEndpointScheme(u *url.URL) bool {
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}

// refuseCrossHostRedirects is the client's redirect policy: same-host
// redirects are followed (capped at three hops), cross-host redirects are
// refused outright — the Authorization header travels on every request, and
// following one would hand the key to the redirect target (CWE-601). This
// reimplements internal/interactions.refuseCrossHostRedirects (unexported)
// and model.refuseCrossHostRedirects; the duplication is noted in
// docs/DECISIONS.md.
func refuseCrossHostRedirects(req *http.Request, via []*http.Request) error {
	if req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("search: redirect to %s refused: cross-origin redirect with sensitive headers", req.URL.Host)
	}
	if len(via) >= 3 {
		return errors.New("search: too many redirects")
	}
	return nil
}
