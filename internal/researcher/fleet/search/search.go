// Package search is the web-search tool client for a Chiron research worker:
// one of the two read-only network tools a worker may use
// (docs/V2-RESEARCH-AGENT §1, §5). The worker calls Search to discover
// candidate URLs, then hands them to the web_fetch client.
//
// The tool is reached over the Model Context Protocol "Streamable HTTP"
// transport through internal/mcpclient, which owns the handshake, bounded
// reads, redirect refusal and key scrubbing. This package is the result-shape
// layer: it names the tool and its query argument and parses the reply into a
// flat []Result.
package search

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rxbynerd/chiron/internal/mcpclient"
)

const (
	defaultToolName    = "search"
	defaultQueryArgKey = "query"
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

// Client is an MCP web-search client. The API key travels only in the
// Authorization header: it is never embedded in URLs, logged, or included in
// error text.
type Client struct {
	mcp         *mcpclient.Client
	toolName    string
	queryArgKey string
}

// New builds a Client, validating the endpoint before any request is made.
func New(opts Options) (*Client, error) {
	mc, err := mcpclient.New(mcpclient.Options{
		Endpoint:       opts.Endpoint,
		APIKey:         opts.APIKey,
		HTTPClient:     opts.HTTPClient,
		RequestTimeout: opts.RequestTimeout,
		MaxBodyBytes:   opts.MaxBodyBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	c := &Client{mcp: mc, toolName: opts.ToolName, queryArgKey: opts.QueryArgKey}
	if c.toolName == "" {
		c.toolName = defaultToolName
	}
	if c.queryArgKey == "" {
		c.queryArgKey = defaultQueryArgKey
	}
	return c, nil
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

// Search calls the configured search tool once and returns the parsed
// results. The exchange is bounded by RequestTimeout (a tighter caller
// deadline wins) and MaxBodyBytes.
//
// Nothing is retried: tools/call may invoke a billable upstream search, so it
// is single-attempt like the model adapter's paid POST. A caller that wants to
// retry a failed search does so knowingly.
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	if query == "" {
		return nil, errors.New("search: query must not be empty")
	}
	result, err := c.mcp.CallTool(ctx, c.toolName, map[string]any{c.queryArgKey: query})
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if result.IsError {
		return nil, fmt.Errorf("search: tool reported an error: %s", mcpclient.FirstText(result.Content))
	}
	return parseResults(result), nil
}
