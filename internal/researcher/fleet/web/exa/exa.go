// Package exa is the Exa provider for the web seam: one Client serves both
// web.Searcher (POST /search) and web.Fetcher (POST /contents). The key
// travels only in x-api-key, redirects are refused, bodies are bounded,
// Fetch refuses internal or credential-bearing URLs before sending them,
// and no POST is retried except one re-send of an overloaded 503
// (docs/DECISIONS.md, 2026-10-20).
package exa

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rxbynerd/chiron/internal/httpx"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
)

// DefaultEndpoint is Exa's public API origin. It is documentation only: the
// client has no default endpoint, so the operator always names the
// destination its key is sent to.
const DefaultEndpoint = "https://api.exa.ai"

const (
	defaultRequestTimeout = 30 * time.Second
	defaultNumResults     = 10
	maxNumResults         = 100
	defaultMaxCharacters  = 10_000
	maxMaxCharacters      = 1_000_000
	maxMaxAgeHours        = 720
	// A page of N characters can take about six bytes per character once
	// JSON-escaped, plus the envelope; the body bound never drops below
	// minMaxBodyBytes.
	bodyBytesPerCharacter = 6
	bodyBytesOverhead     = 64 << 10
	minMaxBodyBytes       = 1 << 20
	defaultUserAgent      = "chiron/2 (+https://github.com/rxbynerd/chiron; research-only)"
	searchPath            = "/search"
	contentsPath          = "/contents"
)

// Options configures a Client.
type Options struct {
	// Endpoint is the Exa API origin, normally DefaultEndpoint. Required. It
	// must be an absolute https:// URL, with http:// admitted for loopback
	// hosts only, and carry no userinfo, query or fragment.
	Endpoint string
	// APIKey is the already-resolved Exa key, sent only in the x-api-key
	// header. Required: Exa answers a keyless request with a payment
	// challenge rather than an authentication error.
	APIKey string
	// HTTPClient supplies the underlying client; nil builds one. It is
	// shallow-copied so the no-redirect policy never mutates the caller's
	// client. Leave its Timeout zero; RequestTimeout bounds each call.
	HTTPClient *http.Client
	// RequestTimeout bounds each Search or Fetch, including the one re-send
	// of an overloaded request and reading the body. Default 30s; a tighter
	// caller deadline still wins.
	RequestTimeout time.Duration
	// MaxBodyBytes bounds a response body; a larger body is an error, not a
	// truncation. Default max(1 MiB, 6*MaxCharacters + 64 KiB); a smaller
	// explicit value than 6*MaxCharacters + 64 KiB is refused, because a
	// legitimate page could not fit.
	MaxBodyBytes int64
	// UserAgent is sent on every request. Empty uses a Chiron agent string.
	UserAgent string
	// NumResults is the number of search results requested and returned.
	// 0 selects 10; otherwise 1 to 100.
	NumResults int
	// MaxCharacters bounds a fetched page's text, in characters (runes),
	// not bytes. 0 selects 10,000; otherwise 1 to 1,000,000.
	MaxCharacters int
	// MaxAgeHours lets Fetch use a cached copy younger than this many hours
	// and crawl live otherwise. 0 omits the field, which Exa treats as
	// "cached when available, else live"; 1 to 720 is sent; Exa's own 0
	// (always live) and -1 (cache only) cannot be expressed.
	MaxAgeHours int
	// SendObjective sends web.Query.Objective with each search. Off, the
	// objective never leaves the process.
	SendObjective bool
}

// Client is an Exa search and fetch client. It is stateless and safe for
// concurrent use.
type Client struct {
	endpoint       string // no trailing slash
	apiKey         string
	httpClient     *http.Client
	requestTimeout time.Duration
	maxBodyBytes   int64
	userAgent      string
	numResults     int
	maxCharacters  int
	maxAgeHours    int
	sendObjective  bool
}

var (
	_ web.Searcher = (*Client)(nil)
	_ web.Fetcher  = (*Client)(nil)
)

// New validates opts and builds a Client. Nothing is sent until a call.
func New(opts Options) (*Client, error) {
	if opts.APIKey == "" {
		return nil, errors.New("exa: API key must not be empty")
	}
	if _, err := httpx.ParseEndpoint(opts.Endpoint); err != nil {
		return nil, fmt.Errorf("exa: endpoint %w", err)
	}

	numResults := opts.NumResults
	switch {
	case numResults == 0:
		numResults = defaultNumResults
	case numResults < 1 || numResults > maxNumResults:
		return nil, fmt.Errorf("exa: result count %d out of range 1..%d", numResults, maxNumResults)
	}
	maxCharacters := opts.MaxCharacters
	switch {
	case maxCharacters == 0:
		maxCharacters = defaultMaxCharacters
	case maxCharacters < 1 || maxCharacters > maxMaxCharacters:
		return nil, fmt.Errorf("exa: page character bound %d out of range 1..%d", maxCharacters, maxMaxCharacters)
	}
	if opts.MaxAgeHours < 0 || opts.MaxAgeHours > maxMaxAgeHours {
		return nil, fmt.Errorf("exa: cache age %d hours out of range 0..%d", opts.MaxAgeHours, maxMaxAgeHours)
	}
	floor := int64(maxCharacters)*bodyBytesPerCharacter + bodyBytesOverhead
	maxBodyBytes := opts.MaxBodyBytes
	switch {
	case maxBodyBytes == 0:
		maxBodyBytes = max(floor, minMaxBodyBytes)
	case maxBodyBytes < floor:
		return nil, fmt.Errorf("exa: body bound %d bytes is below the %d bytes a %d-character page needs", maxBodyBytes, floor, maxCharacters)
	}
	requestTimeout := opts.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	hc := *httpClient
	hc.CheckRedirect = httpx.RefuseAllRedirects

	return &Client{
		endpoint:       strings.TrimSuffix(opts.Endpoint, "/"),
		apiKey:         opts.APIKey,
		httpClient:     &hc,
		requestTimeout: requestTimeout,
		maxBodyBytes:   maxBodyBytes,
		userAgent:      userAgent,
		numResults:     numResults,
		maxCharacters:  maxCharacters,
		maxAgeHours:    opts.MaxAgeHours,
		sendObjective:  opts.SendObjective,
	}, nil
}
