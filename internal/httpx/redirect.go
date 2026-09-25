package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// maxRedirectChain caps the requests in a followed redirect chain, the
// original included, so at most two redirects are followed.
const maxRedirectChain = 3

// RefuseUnsafeRedirects is a CheckRedirect policy for credential-bearing
// clients. It follows a redirect only to the original host:port, never from
// https to a non-https target, and within maxRedirectChain requests, because
// net/http forwards credential headers to redirect targets (CWE-601, CWE-319).
func RefuseUnsafeRedirects(req *http.Request, via []*http.Request) error {
	first := via[0].URL
	if !sameOrigin(req.URL, first) {
		return fmt.Errorf("redirect to %s refused: cross-origin redirect with sensitive headers", req.URL.Host)
	}
	if first.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("redirect to %s://%s refused: downgrade from https with sensitive headers", req.URL.Scheme, req.URL.Host)
	}
	if len(via) >= maxRedirectChain {
		return errors.New("too many redirects")
	}
	return nil
}

// sameOrigin reports whether a and b name the same host and effective port,
// comparing hostnames case-insensitively. A URL without an explicit port
// defaults to original's scheme, not its own, so a same-host scheme
// downgrade still reaches the downgrade check below rather than being
// misread here as a port change.
func sameOrigin(a, b *url.URL) bool {
	if !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return effectivePort(a, a.Scheme) == effectivePort(b, a.Scheme)
}

// effectivePort returns u's port, or defaultScheme's default port when u
// carries none.
func effectivePort(u *url.URL, defaultScheme string) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch defaultScheme {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

// RefuseAllRedirects is a CheckRedirect policy that follows no redirect, for
// a client whose credentials include a custom header, which net/http forwards
// to any target.
func RefuseAllRedirects(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("redirect to %s refused: credentials are never sent to a redirect target", req.URL.Host)
}
