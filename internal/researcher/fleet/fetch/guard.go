package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"

	"github.com/rxbynerd/chiron/internal/httpx"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
)

// ErrRefusedDestination is wrapped by every error that refuses a URL because
// its destination is an internal address, whether checked before the
// request, on a redirect, or when the connection is dialled. It is
// web.ErrRefusedDestination, so callers test for either name.
var ErrRefusedDestination = web.ErrRefusedDestination

// Resolver looks up the addresses of a host. *net.Resolver implements it.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// guard is the SSRF guard shared by the pre-flight check, the redirect check
// and the transport's dialer.
type guard struct {
	resolver      Resolver
	allowLoopback bool
}

// checkURL refuses u before any connection is attempted if its host is, or
// resolves to, an internal address.
func (g guard) checkURL(ctx context.Context, u *url.URL) error {
	host := u.Hostname()
	if host == "" {
		return errors.New("fetch: URL has no host")
	}
	_, err := g.resolve(ctx, host)
	return err
}

// resolve returns the addresses of host, refusing with ErrRefusedDestination
// if any of them is internal. A literal IP is checked without a lookup.
func (g guard) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if httpx.InternalAddr(ip, g.allowLoopback) {
			return nil, fmt.Errorf("%w: %s is an internal address", ErrRefusedDestination, host)
		}
		return []netip.Addr{ip.Unmap()}, nil
	}

	answers, err := g.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("fetch: resolving %s: %w", host, err)
	}
	if len(answers) == 0 {
		return nil, fmt.Errorf("fetch: resolving %s: no addresses", host)
	}
	addrs := make([]netip.Addr, 0, len(answers))
	for _, a := range answers {
		ip, _ := netip.AddrFromSlice(a.IP)
		if httpx.InternalAddr(ip, g.allowLoopback) {
			return nil, fmt.Errorf("%w: %s resolves to internal address %s", ErrRefusedDestination, host, a.IP)
		}
		addrs = append(addrs, ip.Unmap())
	}
	return addrs, nil
}

// dialContext wraps dial so each connection goes to an address resolve has
// just checked, tried in resolver order, and never to a name dial would
// resolve again.
func (g guard) dialContext(dial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("fetch: dial address %q: %w", addr, err)
		}
		ips, err := g.resolve(ctx, host)
		if err != nil {
			return nil, err
		}
		var firstErr error
		for _, ip := range ips {
			conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			if firstErr == nil {
				firstErr = err
			}
			if ctx.Err() != nil {
				break
			}
		}
		return nil, firstErr
	}
}

// guardedTransport clones rt (http.DefaultTransport when nil) with the proxy
// removed and every dial routed through g. A custom TLS dialer would carry
// HTTPS connections past DialContext, so a transport with one is rejected.
func guardedTransport(rt http.RoundTripper, g guard) (*http.Transport, error) {
	if rt == nil {
		rt = http.DefaultTransport
	}
	base, ok := rt.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("fetch: HTTPClient.Transport is %T; the SSRF guard needs an *http.Transport to wrap", rt)
	}
	hasTLSDialer := base.DialTLSContext != nil || base.DialTLS != nil //nolint:staticcheck // DialTLS is deprecated but still honoured.
	if hasTLSDialer {
		return nil, errors.New("fetch: HTTPClient.Transport sets a TLS dialer, which would bypass the SSRF guard")
	}

	t := base.Clone()
	t.Proxy = nil
	dial := t.DialContext
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	t.DialContext = g.dialContext(dial)
	return t, nil
}
