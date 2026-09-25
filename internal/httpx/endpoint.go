// Package httpx holds the HTTP rules every credential-bearing Chiron client
// shares: which endpoints may receive a credential, which redirects may be
// followed, and how a response body is read under a bound. It imports only
// the standard library so config, the CLI and every client can use it.
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

// ParseEndpoint validates a URL a credential will be sent to: https anywhere,
// http only when LoopbackHost admits the host, a non-empty host, and no
// userinfo, query or fragment, not even a bare '?' or '#' (CWE-918, CWE-319).
// Errors never echo the raw value and read as predicates, so callers prefix a
// subject such as "model: endpoint " or a config field name.
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

// LoopbackHost reports whether host is exactly "localhost" or a literal
// 127.0.0.0/8 or ::1 address, IPv4-mapped forms included. host is the bare
// form url.URL.Hostname returns; any other name is refused whatever it
// resolves to.
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

// describe names an endpoint by scheme and host, withholding both when raw
// contains '@': a malformed URL can carry credentials url.Parse does not
// recognise as userinfo, and they can land in the host.
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
