// Package httpx holds the HTTP rules shared by every Chiron client that sends
// a credential: which endpoints may receive it (ParseEndpoint, LoopbackHost),
// which redirects may be followed (RefuseUnsafeRedirects, RefuseAllRedirects),
// and how a response body is read under a bound (ReadAllBounded). It imports
// only the standard library, so config, the CLI and every client can share
// one copy.
package httpx

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrInvalidEndpoint matches, via errors.Is, every error ParseEndpoint
// returns.
var ErrInvalidEndpoint = errors.New("invalid endpoint")

type endpointError string

func (e endpointError) Error() string { return string(e) }

func (endpointError) Is(target error) bool { return target == ErrInvalidEndpoint }

// ParseEndpoint parses and validates a URL that a credential will be sent to.
// It admits an absolute https:// URL anywhere and an http:// URL only when
// LoopbackHost admits its host, so a cleartext or internal-network endpoint
// can never receive a key (CWE-918, CWE-319). The host must be non-empty, and
// userinfo, a query and a fragment are refused, including a bare trailing '?'
// or '#': a deployment origin carries none of them, and each is a place a
// credential could hide.
//
// Error text names the URL by scheme and host only, never the raw value, and
// withholds even those when the value contains '@', since a malformed URL can
// carry credentials url.Parse does not recognise as userinfo. Each message
// reads as a predicate, so callers prefix a subject: "model: endpoint ",
// "CHIRON_GEMINI_BASE_URL ", or a config field name.
func ParseEndpoint(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, endpointError("must not be empty")
	}
	u, err := url.Parse(raw)
	if err == nil && u.User != nil {
		return nil, endpointError("must not embed userinfo (user:password@); credentials travel only in request headers")
	}
	if err != nil || u.Hostname() == "" || !allowedScheme(u) {
		return nil, endpointError("must be an absolute https:// URL (http:// only for loopback test servers); got " + describe(raw, u))
	}
	// Checked on the raw value so an empty query or fragment is refused too.
	if strings.ContainsAny(raw, "?#") {
		return nil, endpointError("must not carry a query or fragment; got " + describe(raw, u))
	}
	return u, nil
}

// LoopbackHost reports whether host names this machine: exactly "localhost"
// (lower-case, no trailing dot), or a literal address in 127.0.0.0/8 or ::1,
// including the IPv4-mapped form of a 127.0.0.0/8 address. host is a bare
// hostname as url.URL.Hostname returns it; a bracketed or port-qualified value
// is not recognised. Any other name is refused even if it resolves to
// loopback, because what a name resolves to is outside this check's control.
func LoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func allowedScheme(u *url.URL) bool {
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return LoopbackHost(u.Hostname())
	default:
		return false
	}
}

// describe names an endpoint in an error without echoing the raw value.
func describe(raw string, u *url.URL) string {
	switch {
	case strings.Contains(raw, "@"):
		return "a value withheld because it contains '@'"
	case u == nil:
		return "an unparseable URL"
	default:
		return fmt.Sprintf("scheme %q host %q", u.Scheme, u.Host)
	}
}
