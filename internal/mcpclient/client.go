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
// client: there is no resources/prompts/sampling surface and no
// server-initiated request handling. One session serves the Client's
// lifetime: the first CallTool establishes it, every later request echoes its
// Mcp-Session-Id and negotiated MCP-Protocol-Version, and Close ends it with
// a best-effort DELETE.
//
// Security follows the model adapter (internal/researcher/fleet/model): every
// response body is bounded, a text/event-stream reply is read under the same
// bound, cross-host and https-to-http redirects are refused, and the key
// travels only in the Authorization header — never a URL, query, log, error,
// or trace; nor does the session id. No round-trip is retried, since a
// tools/call may invoke a billable upstream, with one exception: a request the
// server answers with HTTP 404 because it no longer holds the session was not
// processed, so it is sent once more on a fresh session.
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
	// RequestTimeout bounds one CallTool, including any initialize and
	// initialized round-trips it performs and reading the bodies, and
	// separately bounds each session DELETE. Default 30s. A tighter caller
	// deadline still wins, except over a DELETE.
	RequestTimeout time.Duration
	// MaxBodyBytes bounds every response body, application/json or
	// text/event-stream. Default 8 MiB.
	MaxBodyBytes int64
	// ClientName is the initialize clientInfo.name. Default "chiron".
	ClientName string
	// ClientVersion is the initialize clientInfo.version. Default "v2".
	ClientVersion string
}

// ErrClosed is returned by CallTool once Close has been called.
var ErrClosed = errors.New("mcp: client is closed")

// Client is a hand-rolled net/http MCP client, safe for concurrent use. The
// API key is never embedded in URLs, logged, or included in error text.
type Client struct {
	endpoint       string
	apiKey         string
	httpClient     *http.Client
	requestTimeout time.Duration
	maxBodyBytes   int64
	clientName     string
	clientVersion  string

	// lastID numbers JSON-RPC requests; MCP forbids reusing an id within a
	// session.
	lastID atomic.Int64

	// mu guards the session state below and is never held across a request.
	mu      sync.Mutex
	sess    *session   // established session; nil before the first handshake or after expiry
	pending *handshake // handshake in flight, shared by concurrent callers
	closed  bool
}

// handshake is one in-flight session establishment. done is closed once sess
// or err is set.
type handshake struct {
	done chan struct{}
	sess *session
	err  error
	// retry marks a failure after the leading caller's context ended.
	// Waiters do not share it: they try again under their own contexts.
	retry bool
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

// CallTool issues one tools/call for the named tool on the client's session,
// first establishing the session (initialize, then the initialized
// notification) when there is none. Concurrent callers share one handshake.
// The exchange is bounded by RequestTimeout and MaxBodyBytes.
//
// An initialize reply naming an unsupported protocol version, or none, fails
// with ErrUnsupportedProtocolVersion before any tools/call. When the server
// answers the tools/call with HTTP 404, the session is gone and the call was
// not processed: the session is re-established and the call sent once more.
// Nothing else is retried; a caller that wants to retry does so knowingly.
// After Close, CallTool fails with ErrClosed.
//
// A tool-level failure is returned as a ToolResult with IsError set, not as an
// error: the caller owns how a tool's failure text is surfaced.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	if name == "" {
		return ToolResult{}, errors.New("mcp: tool name must not be empty")
	}

	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	sess, err := c.session(ctx)
	if err != nil {
		return ToolResult{}, err
	}
	result, err := c.callTool(ctx, *sess, name, args)
	if !sessionGone(err, *sess) {
		return result, err
	}
	c.forgetSession(sess)
	if sess, err = c.session(ctx); err != nil {
		return ToolResult{}, err
	}
	result, err = c.callTool(ctx, *sess, name, args)
	if sessionGone(err, *sess) {
		c.forgetSession(sess)
	}
	return result, err
}

// Close ends the client's session, when the server issued one, with a
// best-effort DELETE on a fresh context bounded by RequestTimeout, and makes
// every later CallTool fail with ErrClosed. It is idempotent and safe to call
// concurrently with CallTool. It always returns nil: a server may refuse
// client-initiated termination, and its outcome changes nothing here.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	sess := c.sess
	c.sess = nil
	c.mu.Unlock()

	if sess != nil && sess.id != "" {
		c.endSession(*sess)
	}
	return nil
}

// session returns the established session, leading a handshake when there is
// none or joining the one in flight. A waiter honours its own ctx. A failed
// handshake is not kept, so the next call tries again.
func (c *Client) session(ctx context.Context) (*session, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		if c.sess != nil {
			sess := c.sess
			c.mu.Unlock()
			return sess, nil
		}
		if hs := c.pending; hs != nil {
			c.mu.Unlock()
			select {
			case <-hs.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if hs.retry {
				continue
			}
			return hs.sess, hs.err
		}
		hs := &handshake{done: make(chan struct{})}
		c.pending = hs
		c.mu.Unlock()
		return c.lead(ctx, hs)
	}
}

// lead runs the handshake hs stands for and publishes its outcome. A session
// established after Close is ended at once rather than kept.
func (c *Client) lead(ctx context.Context, hs *handshake) (*session, error) {
	sess, err := c.establish(ctx)

	c.mu.Lock()
	c.pending = nil
	orphaned := err == nil && c.closed
	switch {
	case orphaned:
		hs.err = ErrClosed
	case err != nil:
		hs.err = err
		hs.retry = ctx.Err() != nil
	default:
		hs.sess = &sess
		c.sess = hs.sess
	}
	c.mu.Unlock()
	close(hs.done)

	if orphaned && sess.id != "" {
		c.endSession(sess)
	}
	return hs.sess, hs.err
}

// forgetSession drops sess if it is still the established session, so a
// caller holding a stale session never discards a fresh one.
func (c *Client) forgetSession(sess *session) {
	c.mu.Lock()
	if c.sess == sess {
		c.sess = nil
	}
	c.mu.Unlock()
}

// nextID returns a JSON-RPC request id not yet used by this client.
func (c *Client) nextID() int {
	return int(c.lastID.Add(1))
}
