// Package alexandria is a memory.Recaller over Alexandria's REST search API
// (GET /v1/search), the integration path Alexandria's own Claude Code plugin
// uses (docs/KNOWLEDGE.md §3.3). It is recall-only: Alexandria's write path
// needs a citation model Chiron does not have.
//
// Security follows the fleet model and search clients: the endpoint must be
// https (http for loopback only), the API key travels only in the
// Authorization header, redirects are never followed because they would
// carry the bearer elsewhere, bodies are bounded, and every error is
// scrubbed of the key before it leaves the package.
package alexandria

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/rxbynerd/chiron/internal/httpx"
	"github.com/rxbynerd/chiron/internal/memory"
)

const (
	defaultRequestTimeout = 30 * time.Second
	defaultMaxBodyBytes   = 1 << 20
	defaultLimit          = 5
	maxLimit              = 20
	defaultMaxTokens      = 4000
	minMaxTokens          = 100
	maxMaxTokens          = 20000
	maxSpaceLen           = 64
	searchPath            = "/v1/search"
)

// spaceSlug is Alexandria's space slug grammar; the length bound is
// maxSpaceLen.
var spaceSlug = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Options configures a Client.
type Options struct {
	// Endpoint is the Alexandria deployment origin, e.g.
	// "https://alexandria-api.example.workers.dev"; the client GETs
	// "{Endpoint}/v1/search". It must be an absolute https:// URL, with
	// http:// admitted for loopback hosts only, and must carry no userinfo,
	// query or fragment. Fragment locators are built on its origin.
	Endpoint string
	// APIKey is the already-resolved bearer token (an alx_ static token or
	// an OAuth access token). Required; sent only in the Authorization
	// header.
	APIKey string
	// HTTPClient supplies the underlying client; nil builds one. It is
	// shallow-copied so the no-redirect policy never mutates the caller's
	// client.
	HTTPClient *http.Client
	// RequestTimeout bounds each Recall including the body read. Default
	// 30s; a tighter caller deadline still wins.
	RequestTimeout time.Duration
	// DefaultLimit is the hit cap when Query.Limit is not positive.
	// Default 5, at most 20.
	DefaultLimit int
	// MaxTokens is Alexandria's max_tokens budget for one search. Default
	// 4000, between 100 and 20000.
	MaxTokens int
	// AccessClientID and AccessClientSecret are an optional Cloudflare
	// Access service token, sent as the cf-access-client-id and
	// cf-access-client-secret headers. Set both or neither.
	AccessClientID     string
	AccessClientSecret string
	// MaxBodyBytes bounds the response body; a larger body is an error, not
	// a truncation. Default 1 MiB.
	MaxBodyBytes int64
}

// Client recalls long-term memory from one Alexandria deployment.
type Client struct {
	endpoint           string // no trailing slash
	origin             string // scheme://host of endpoint
	apiKey             string
	accessClientID     string
	accessClientSecret string
	httpClient         *http.Client
	requestTimeout     time.Duration
	defaultLimit       int
	maxTokens          int
	maxBodyBytes       int64
}

var _ memory.Recaller = (*Client)(nil)

// New validates opts and builds a Client. Nothing is sent until Recall.
func New(opts Options) (*Client, error) {
	if opts.APIKey == "" {
		return nil, errors.New("alexandria: API key must not be empty")
	}
	u, err := httpx.ParseEndpoint(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("alexandria: endpoint %w", err)
	}
	if (opts.AccessClientID == "") != (opts.AccessClientSecret == "") {
		return nil, errors.New("alexandria: Access client id and secret must be set together")
	}

	limit := opts.DefaultLimit
	switch {
	case limit == 0:
		limit = defaultLimit
	case limit < 0 || limit > maxLimit:
		return nil, fmt.Errorf("alexandria: default limit %d out of range 1..%d", limit, maxLimit)
	}
	maxTokens := opts.MaxTokens
	switch {
	case maxTokens == 0:
		maxTokens = defaultMaxTokens
	case maxTokens < minMaxTokens || maxTokens > maxMaxTokens:
		return nil, fmt.Errorf("alexandria: max tokens %d out of range %d..%d", maxTokens, minMaxTokens, maxMaxTokens)
	}
	requestTimeout := opts.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	maxBodyBytes := opts.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultMaxBodyBytes
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	// net/http re-sends Authorization to a same-host target and the Access
	// secret, a custom header, to any target, so no redirect is followed.
	hc := *httpClient
	hc.CheckRedirect = httpx.RefuseAllRedirects

	return &Client{
		endpoint:           strings.TrimSuffix(opts.Endpoint, "/"),
		origin:             u.Scheme + "://" + u.Host,
		apiKey:             opts.APIKey,
		accessClientID:     opts.AccessClientID,
		accessClientSecret: opts.AccessClientSecret,
		httpClient:         &hc,
		requestTimeout:     requestTimeout,
		defaultLimit:       limit,
		maxTokens:          maxTokens,
		maxBodyBytes:       maxBodyBytes,
	}, nil
}
