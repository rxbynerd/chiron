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
// text/event-stream reply is read under the same bound, cross-host and
// https-to-http redirects are refused, and the search key travels only in the
// Authorization header — never a URL, query, log, error, or trace.
package search

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
	// key-exfiltration and SSRF channel (CWE-918, CWE-319). Userinfo
	// (user:password@) is rejected.
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
	// The policy is set on a shallow copy, sharing the caller's Transport,
	// jar and timeout, so a caller-supplied client cannot bypass it.
	hc := *httpClient
	hc.CheckRedirect = refuseUnsafeRedirects
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
// https:// URL, with http:// admitted for loopback hosts only, and no
// userinfo. This mirrors model.validateEndpoint and allowedEndpointScheme in
// internal/config (validated there too); it is re-applied here because the
// client is a reusable seam that must not depend on config having run. No
// error echoes userinfo.
func validateEndpoint(raw string) error {
	if raw == "" {
		return errors.New("search: endpoint must not be empty")
	}
	u, err := url.Parse(raw)
	if err == nil && u.User != nil {
		return errors.New("search: endpoint must not embed userinfo (user:password@); the API key travels only in the Authorization header")
	}
	if err != nil || u.Host == "" || !allowedEndpointScheme(u) {
		return fmt.Errorf("search: endpoint %s must be an absolute https:// URL (http:// only for loopback test servers)", quoteEndpoint(raw))
	}
	return nil
}

// quoteEndpoint renders an endpoint for an error message. A value containing
// '@' is withheld: a malformed URL can carry credentials that url.Parse does
// not recognise as userinfo.
func quoteEndpoint(raw string) string {
	if strings.Contains(raw, "@") {
		return "(withheld: contains '@')"
	}
	return strconv.Quote(raw)
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

// refuseUnsafeRedirects is the client's redirect policy: same-host redirects
// are followed (capped at three hops), but a redirect to another host
// (CWE-601) or from https to http (CWE-319) is refused. net/http re-sends the
// Authorization header to any target on the same hostname whatever its
// scheme, so a downgrade would put the key on the wire in cleartext. It
// mirrors model.refuseUnsafeRedirects; the duplication is noted in
// docs/DECISIONS.md.
func refuseUnsafeRedirects(req *http.Request, via []*http.Request) error {
	first := via[0].URL
	if req.URL.Host != first.Host {
		return fmt.Errorf("search: redirect to %s refused: cross-origin redirect with sensitive headers", req.URL.Host)
	}
	if first.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("search: redirect to %s://%s refused: downgrade from https with sensitive headers", req.URL.Scheme, req.URL.Host)
	}
	if len(via) >= 3 {
		return errors.New("search: too many redirects")
	}
	return nil
}
