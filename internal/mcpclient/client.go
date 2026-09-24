// Package mcpclient is a minimal, SDK-free net/http client for one tool call
// over the Model Context Protocol (MCP) "Streamable HTTP" transport. It is the
// shared transport under the worker's web-search client
// (internal/researcher/fleet/search) and the Billet memory adapter
// (internal/memory/billet), and knows nothing about either tool's result
// shape.
//
// MCP is JSON-RPC 2.0 carried over HTTP. CallTool implements only the minimum
// correct flow — initialize, an initialized notification, then a tools/call —
// and returns the raw tool result. It is deliberately not a general MCP
// client: there is no resources/prompts/sampling surface, no server-initiated
// request handling, and a session lives for one CallTool: its Mcp-Session-Id
// and negotiated MCP-Protocol-Version are echoed, then the session is ended
// with a best-effort DELETE.
//
// Security follows the model adapter (internal/researcher/fleet/model): every
// response body is bounded, a text/event-stream reply is read under the same
// bound, cross-host and https-to-http redirects are refused, and the key
// travels only in the Authorization header — never a URL, query, log, error,
// or trace. No round-trip is retried: a tools/call may invoke a billable
// upstream.
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Defaults, all overridable via Options. The body bound is generous so a
// misbehaving server cannot exhaust memory, not because a normal response
// approaches it.
const (
	defaultRequestTimeout = 30 * time.Second
	defaultMaxBodyBytes   = 8 << 20
	maxErrorBodyBytes     = 1 << 20
	// maxErrorTextBytes bounds any server-supplied text carried into an
	// error or an IsError result, so a caller never relays megabytes of it.
	maxErrorTextBytes = 4 << 10

	defaultClientName    = "chiron"
	defaultClientVersion = "v2"
)

// ProtocolVersion is the MCP revision advertised in initialize and the
// MCP-Protocol-Version fallback when the result names none. A server choosing
// another revision is tolerated (only the JSON-RPC envelope is relied on), and
// its choice is echoed on every later request.
const ProtocolVersion = "2025-06-18"

// Options configures a Client.
type Options struct {
	// Endpoint is the MCP server's Streamable-HTTP URL. Required. It must be
	// an absolute https:// URL, with http:// admitted for loopback hosts only
	// (127.0.0.1, ::1, localhost): the credential travels to whatever endpoint
	// is set, so a cleartext or internal override would be a key-exfiltration
	// and SSRF channel (CWE-918, CWE-319). Userinfo (user:password@) is
	// rejected.
	Endpoint string
	// APIKey is the already-resolved literal key, sent only in the
	// Authorization header. Empty sends no Authorization header.
	APIKey string
	// HTTPClient supplies the underlying client; nil builds one. It is
	// shallow-copied so the redirect policy applies without mutating the
	// caller's client. Leave its Timeout zero: deadlines come from
	// RequestTimeout.
	HTTPClient *http.Client
	// RequestTimeout bounds one CallTool — initialize, initialized, tools/call
	// and the closing DELETE — including reading the bodies. Default 30s. A
	// tighter caller deadline still wins.
	RequestTimeout time.Duration
	// MaxBodyBytes bounds every response body, application/json or
	// text/event-stream. Default 8 MiB.
	MaxBodyBytes int64
	// ClientName is the initialize clientInfo.name. Default "chiron".
	ClientName string
	// ClientVersion is the initialize clientInfo.version. Default "v2".
	ClientVersion string
}

// Client is a hand-rolled net/http MCP client. The API key is never embedded
// in URLs, logged, or included in error text.
type Client struct {
	endpoint       string
	apiKey         string
	httpClient     *http.Client
	requestTimeout time.Duration
	maxBodyBytes   int64
	clientName     string
	clientVersion  string
}

// ToolResult is a tools/call result: the content blocks, the optional
// structuredContent object, and the isError flag distinguishing a tool-level
// failure from a protocol error. When IsError is set, every text block has
// already been scrubbed of credentials and bounded to 4 KiB, so a caller may
// surface it in an error.
type ToolResult struct {
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError"`
}

// ContentBlock is one block in a tool result's content array. Only text
// blocks carry Text; other types (image, resource) keep only their Type.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// FirstText returns the first non-empty text block, or "".
func FirstText(blocks []ContentBlock) string {
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}

// New builds a Client, validating the endpoint before any request is made.
// The client is a reusable seam and does not trust its caller to have
// validated the endpoint already.
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

	c := &Client{
		endpoint:       strings.TrimSuffix(opts.Endpoint, "/"),
		apiKey:         opts.APIKey,
		httpClient:     &hc,
		requestTimeout: opts.RequestTimeout,
		maxBodyBytes:   opts.MaxBodyBytes,
		clientName:     opts.ClientName,
		clientVersion:  opts.ClientVersion,
	}
	if c.requestTimeout <= 0 {
		c.requestTimeout = defaultRequestTimeout
	}
	if c.maxBodyBytes <= 0 {
		c.maxBodyBytes = defaultMaxBodyBytes
	}
	if c.clientName == "" {
		c.clientName = defaultClientName
	}
	if c.clientVersion == "" {
		c.clientVersion = defaultClientVersion
	}
	return c, nil
}

// CallTool runs initialize, the initialized notification, and one tools/call
// for the named tool, then ends any session the server issued with a
// best-effort DELETE. The exchange is bounded by RequestTimeout and
// MaxBodyBytes. No round-trip is retried; a caller that wants to retry does so
// knowingly.
//
// A tool-level failure is returned as a ToolResult with IsError set, not as an
// error: the caller owns how a tool's failure text is surfaced.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	if name == "" {
		return ToolResult{}, errors.New("mcp: tool name must not be empty")
	}

	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	sess, err := c.initialize(ctx)
	if err != nil {
		return ToolResult{}, err
	}
	if sess.id != "" {
		defer c.endSession(ctx, sess)
	}
	if err := c.notifyInitialized(ctx, sess); err != nil {
		return ToolResult{}, err
	}
	return c.callTool(ctx, sess, name, args)
}

// validateEndpoint applies the CHIRON_GEMINI_BASE_URL rule (absolute https://,
// http:// for loopback only) and rejects userinfo without echoing it. It
// mirrors model.validateEndpoint and internal/config's check because this
// reusable seam must not depend on config having run.
func validateEndpoint(raw string) error {
	if raw == "" {
		return errors.New("mcp: endpoint must not be empty")
	}
	u, err := url.Parse(raw)
	if err == nil && u.User != nil {
		return errors.New("mcp: endpoint must not embed userinfo (user:password@); the API key travels only in the Authorization header")
	}
	if err != nil || u.Host == "" || !allowedEndpointScheme(u) {
		return fmt.Errorf("mcp: endpoint %s must be an absolute https:// URL (http:// only for loopback test servers)", quoteEndpoint(raw))
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
// rule model.allowedEndpointScheme, internal/config and internal/cli enforce,
// mirrored because this seam cannot import the CLI/config layer.
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

// refuseUnsafeRedirects follows same-host redirects (at most three hops) and
// refuses one to another host or from https to http: net/http re-sends
// Authorization to any target on the same hostname whatever its scheme
// (CWE-601, CWE-319). It mirrors model.refuseUnsafeRedirects.
func refuseUnsafeRedirects(req *http.Request, via []*http.Request) error {
	first := via[0].URL
	if req.URL.Host != first.Host {
		return fmt.Errorf("mcp: redirect to %s refused: cross-origin redirect with sensitive headers", req.URL.Host)
	}
	if first.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("mcp: redirect to %s://%s refused: downgrade from https with sensitive headers", req.URL.Scheme, req.URL.Host)
	}
	if len(via) >= 3 {
		return errors.New("mcp: too many redirects")
	}
	return nil
}
