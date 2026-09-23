// Package fetch is the worker's web_fetch tool: a hand-rolled net/http client
// that retrieves the content of an UNTRUSTED external URL discovered by the
// search tool (docs/V2-RESEARCH-AGENT §1, §5). It is the second and last
// read-only network tool a Chiron research worker may use.
//
// It is the first Chiron component to fetch arbitrary URLs, so its security
// posture is deliberate. Unlike the model and search clients — which POST a
// credential to one configured, validated endpoint — web_fetch dials hosts
// chosen by an upstream search result, and carries NO Chiron credential. The
// dominant risk is therefore Server-Side Request Forgery (SSRF, CWE-918): a
// crafted or compromised search result pointing web_fetch at an internal
// address. The guard here refuses loopback, private, link-local, CGNAT,
// multicast, reserved and other special-purpose destinations, including an
// IPv4 address embedded in an IPv6 one (see isInternal), and re-checks the
// destination on every redirect so a redirect to an internal host is refused
// too.
//
// HTTP hardening otherwise follows the Gemini adapter (internal/interactions):
// bounded body reads, a redirect-depth cap, and a per-call timeout that yields
// to a tighter caller deadline. A body over the bound is truncated-and-marked
// rather than an error — partial page text is still useful research input
// (see docs/DECISIONS.md), which is the deliberate difference from the model
// adapter, where an oversize response is an error.
package fetch

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
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

// ErrRefusedDestination is wrapped by every error that refuses a URL because
// its destination is an internal address, whether checked before the request
// or on a redirect.
var ErrRefusedDestination = errors.New("fetch: refused destination")

// Options configures a Client.
type Options struct {
	// HTTPClient supplies the underlying client. nil builds one. A
	// caller-supplied client is shallow-copied so the redirect policy can be
	// set without mutating the caller's client; leave its Timeout zero —
	// per-call deadlines come from RequestTimeout.
	HTTPClient *http.Client
	// RequestTimeout bounds each Fetch call including reading the body.
	// Default 30s. A caller-supplied context deadline still wins if tighter.
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
	requestTimeout  time.Duration
	maxContentBytes int64
	allowLoopback   bool
}

// New builds a Client. There is nothing credential-bearing to validate; the
// options only tune bounds and the loopback allowance.
func New(opts Options) (*Client, error) {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	allowLoopback := opts.AllowLoopback

	// The redirect policy enforces the depth cap and re-checks the SSRF guard
	// on every hop: a 2xx URL that redirects to an internal address must be
	// refused too. It is set on a shallow copy so a caller-supplied client is
	// not mutated.
	hc := *httpClient
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("fetch: stopped after %d redirects", maxRedirects)
		}
		if err := guardHost(req.URL, allowLoopback); err != nil {
			return err
		}
		return nil
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
		requestTimeout:  requestTimeout,
		maxContentBytes: maxContentBytes,
		allowLoopback:   allowLoopback,
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

// guardHost is the SSRF guard: it refuses a destination whose host resolves to
// an internal address. It resolves the hostname to IPs and rejects if ANY
// resolved address is internal — refusing on the union is the safe default,
// since a permissive resolver answer could otherwise smuggle an internal
// target past the check. allowLoopback narrows what counts as internal
// (loopback + unspecified become permitted) but never relaxes private or
// link-local, so a redirect from a loopback test server to an internal
// production address is still caught.
//
// This is a best-effort, resolve-then-check guard. It does NOT pin the
// checked IP for the actual dial: between this lookup and the connection the
// name could re-resolve to a different address (DNS rebinding), and a redirect
// is re-checked here but likewise not pinned. Connection-time IP pinning via a
// custom DialContext is a deliberate follow-up, recorded in docs/DECISIONS.md,
// not implemented here.
func guardHost(u *url.URL, allowLoopback bool) error {
	host := u.Hostname()
	if host == "" {
		return errors.New("fetch: URL has no host")
	}

	// A literal IP is checked directly; a name is resolved and every answer
	// checked.
	if ip, err := netip.ParseAddr(host); err == nil {
		if isInternal(ip, allowLoopback) {
			return fmt.Errorf("%w: %s is an internal address", ErrRefusedDestination, host)
		}
		return nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("fetch: resolving %s: %v", host, err)
	}
	for _, ip := range ips {
		addr, _ := netip.AddrFromSlice(ip)
		if isInternal(addr, allowLoopback) {
			return fmt.Errorf("%w: %s resolves to internal address %s", ErrRefusedDestination, host, ip)
		}
	}
	return nil
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
