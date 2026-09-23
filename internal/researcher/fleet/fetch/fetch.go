// Package fetch is the worker's web_fetch tool: a hand-rolled net/http client
// that retrieves the content of an UNTRUSTED external URL discovered by the
// search tool (docs/V2-RESEARCH-AGENT §1, §5). It is the second and last
// read-only network tool a Chiron research worker may use.
//
// Unlike the model and search clients — which POST a credential to one
// configured, validated endpoint — web_fetch dials hosts chosen by an upstream
// search result and carries NO Chiron credential, so the dominant risk is
// Server-Side Request Forgery (SSRF, CWE-918). The guard refuses internal
// destinations (see isInternal) when the connection is dialled: the transport
// resolves the host itself, refuses if any address is internal, and connects
// only to the addresses it checked, so neither DNS rebinding nor a redirect
// reaches an unchecked address. The transport never uses a proxy. The same
// check runs before the request and on every redirect to fail fast.
//
// Reads are bounded, redirects are capped, and each call has a timeout that
// yields to a tighter caller deadline. A body over the bound is
// truncated-and-marked rather than an error, because partial page text is
// still useful research input (docs/DECISIONS.md).
package fetch

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Defaults, all overridable via Options.
const (
	defaultRequestTimeout  = 30 * time.Second
	defaultMaxContentBytes = 8 << 20
	maxRedirects           = 5
)

// Options configures a Client.
type Options struct {
	// HTTPClient supplies the underlying client. nil builds one on a clone of
	// http.DefaultTransport. The client is shallow-copied and its Transport,
	// which must be nil or an *http.Transport without a TLS dialer, is cloned
	// with the SSRF guard installed and the proxy removed; the caller's values
	// are not mutated. The transport's DialContext, if set, makes each
	// connection to a checked address. Leave Timeout zero — per-call
	// deadlines come from RequestTimeout.
	HTTPClient *http.Client
	// Resolver looks up hostnames for the SSRF guard. nil uses
	// net.DefaultResolver.
	Resolver Resolver
	// RequestTimeout bounds each Fetch call, including name resolution and
	// reading the body. Default 30s. A caller-supplied context deadline still
	// wins if tighter.
	RequestTimeout time.Duration
	// MaxContentBytes bounds how much of a page body is read. Default 8 MiB.
	// A source exceeding the bound is truncated and Page.Truncated is set —
	// partial page text is still useful (docs/DECISIONS.md), unlike the model
	// adapter where oversize is an error.
	MaxContentBytes int64
	// AllowLoopback, when true, permits fetching loopback and unspecified
	// destinations (127.0.0.0/8, ::1, 0.0.0.0, ::). It exists ONLY so tests
	// can reach loopback httptest servers; production configuration must
	// leave it false. Every other internal range stays refused, so a redirect
	// from a loopback test server to the cloud metadata endpoint
	// (169.254.169.254) is still caught.
	AllowLoopback bool
}

// Client is a hand-rolled net/http web_fetch client. It carries no Chiron
// credential; its security posture is the SSRF guard plus bounded reads and a
// redirect cap.
type Client struct {
	httpClient      *http.Client
	guard           guard
	requestTimeout  time.Duration
	maxContentBytes int64
}

// New builds a Client. It fails only if Options.HTTPClient has a transport
// the SSRF guard cannot wrap.
func New(opts Options) (*Client, error) {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	resolver := opts.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	g := guard{resolver: resolver, allowLoopback: opts.AllowLoopback}

	transport, err := guardedTransport(httpClient.Transport, g)
	if err != nil {
		return nil, err
	}

	hc := *httpClient
	hc.Transport = transport
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("fetch: stopped after %d redirects", maxRedirects)
		}
		return g.checkURL(req.Context(), req.URL)
	}

	requestTimeout := opts.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	maxContentBytes := opts.MaxContentBytes
	if maxContentBytes <= 0 {
		maxContentBytes = defaultMaxContentBytes
	}

	return &Client{
		httpClient:      &hc,
		guard:           g,
		requestTimeout:  requestTimeout,
		maxContentBytes: maxContentBytes,
	}, nil
}

// Page is a fetched document. Content is the body read (up to
// MaxContentBytes); Truncated reports whether the source exceeded the bound
// and Content is therefore a prefix. URL is the requested URL with any
// userinfo credentials stripped, so it is safe to log or cite. ContentType is
// the response's Content-Type header verbatim.
type Page struct {
	URL         string
	ContentType string
	Content     []byte
	Truncated   bool
}

// validateScheme accepts only http and https. javascript:, file:, ftp:,
// data:, gopher: and the rest are rejected — web_fetch retrieves web pages,
// and the other schemes are either non-network or classic SSRF vectors.
func validateScheme(u *url.URL) error {
	switch u.Scheme {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("fetch: unsupported URL scheme %q (only http and https)", u.Scheme)
	}
}

// sanitizeURL returns u with any userinfo (user:password@) stripped, so the
// URL can be recorded on the Page or in a diagnostic without leaking
// credentials the caller may have embedded.
func sanitizeURL(u *url.URL) string {
	if u.User == nil {
		return u.String()
	}
	clean := *u
	clean.User = nil
	return clean.String()
}

// scrubURLString strips userinfo from a raw URL string for diagnostics,
// falling back to a coarse redaction if it will not parse — a malformed URL
// must not leak an embedded credential through an error message.
func scrubURLString(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return sanitizeURL(u)
	}
	if i := strings.LastIndex(raw, "@"); i >= 0 {
		if j := strings.Index(raw, "://"); j >= 0 && j+3 < i {
			return raw[:j+3] + "[REDACTED-USERINFO]@" + raw[i+1:]
		}
	}
	return raw
}
