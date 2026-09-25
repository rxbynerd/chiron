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
	"net/http"
	"strings"
	"time"

	"github.com/rxbynerd/chiron/internal/httpx"
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

// ProtocolVersion is the MCP revision advertised in initialize. The server
// may answer with any supported revision, and its choice is echoed as
// MCP-Protocol-Version on every later request.
const ProtocolVersion = "2025-06-18"

// supportedProtocolVersions are the revisions whose Streamable-HTTP transport
// this client implements.
var supportedProtocolVersions = []string{ProtocolVersion, "2025-03-26"}

// maxVersionEchoBytes bounds the server-chosen version quoted in a
// ProtocolVersionError.
const maxVersionEchoBytes = 32

// ErrUnsupportedProtocolVersion matches, via errors.Is, the failure of a
// CallTool whose initialize reply named a protocol version outside the
// supported set, or named none. No tools/call follows such a reply.
var ErrUnsupportedProtocolVersion = errors.New("mcp: unsupported protocol version")

// ProtocolVersionError is the concrete error behind
// ErrUnsupportedProtocolVersion.
type ProtocolVersionError struct {
	// Version is the server's choice, scrubbed and cut to 32 bytes; empty
	// when the reply named none.
	Version string
}

func (e *ProtocolVersionError) Error() string {
	supported := strings.Join(supportedProtocolVersions, ", ")
	if e.Version == "" {
		return "mcp: initialize result names no protocol version; supported: " + supported
	}
	return fmt.Sprintf("mcp: server chose unsupported protocol version %q; supported: %s", e.Version, supported)
}

// Is reports whether target is ErrUnsupportedProtocolVersion.
func (e *ProtocolVersionError) Is(target error) bool {
	return target == ErrUnsupportedProtocolVersion
}

// Options configures a Client.
type Options struct {
	// Endpoint is the MCP server's Streamable-HTTP URL. Required. It must be
	// an absolute https:// URL, with http:// admitted for loopback hosts only
	// (127.0.0.1, ::1, localhost): the credential travels to whatever endpoint
	// is set, so a cleartext or internal override would be a key-exfiltration
	// and SSRF channel (CWE-918, CWE-319). Userinfo (user:password@), a query
	// and a fragment are rejected (httpx.ParseEndpoint).
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
	if _, err := httpx.ParseEndpoint(opts.Endpoint); err != nil {
		return nil, fmt.Errorf("mcp: endpoint %w", err)
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	// The policy is set on a shallow copy, sharing the caller's Transport,
	// jar and timeout, so a caller-supplied client cannot bypass it.
	hc := *httpClient
	hc.CheckRedirect = httpx.RefuseUnsafeRedirects

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
// knowingly. An initialize reply naming an unsupported protocol version, or
// none, fails with ErrUnsupportedProtocolVersion before any tools/call.
//
// A tool-level failure is returned as a ToolResult with IsError set, not as an
// error: the caller owns how a tool's failure text is surfaced.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	if name == "" {
		return ToolResult{}, errors.New("mcp: tool name must not be empty")
	}

	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	sess, err := c.handshake(ctx)
	if err != nil {
		return ToolResult{}, err
	}
	if sess.id != "" {
		defer c.endSession(ctx, sess)
	}
	return c.callTool(ctx, sess, name, args)
}
