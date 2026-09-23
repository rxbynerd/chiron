package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
)

// ErrRefusedDestination is wrapped by every error that refuses a URL because
// its destination is an internal address, whether checked before the
// request, on a redirect, or when the connection is dialled.
var ErrRefusedDestination = errors.New("fetch: refused destination")

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
		if isInternal(ip, g.allowLoopback) {
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
		if isInternal(ip, g.allowLoopback) {
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

// internalPrefixes are the ranges web_fetch never reaches, in addition to
// loopback and the unspecified address.
var internalPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),     // RFC 1918
	netip.MustParsePrefix("100.64.0.0/10"),  // CGNAT, incl. 100.100.100.200 cloud metadata
	netip.MustParsePrefix("169.254.0.0/16"), // link-local, incl. 169.254.169.254 cloud metadata
	netip.MustParsePrefix("172.16.0.0/12"),  // RFC 1918
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("192.168.0.0/16"), // RFC 1918
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("224.0.0.0/4"),    // multicast
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, incl. 255.255.255.255 broadcast
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("fc00::/7"),       // unique local
	netip.MustParsePrefix("fe80::/10"),      // link-local
	netip.MustParsePrefix("fec0::/10"),      // deprecated site-local
	netip.MustParsePrefix("ff00::/8"),       // multicast
}

// IPv6 prefixes whose addresses carry an IPv4 destination.
var (
	nat64Prefix      = netip.MustParsePrefix("64:ff9b::/96")
	sixToFourPrefix  = netip.MustParsePrefix("2002::/16")
	ipv4CompatPrefix = netip.MustParsePrefix("::/96")
)

// isInternal reports whether web_fetch must refuse ip. allowLoopback exempts
// loopback and the unspecified address only; an IPv4 address embedded in a
// NAT64, 6to4 or IPv4-compatible address is judged without that exemption.
func isInternal(ip netip.Addr, allowLoopback bool) bool {
	ip = ip.Unmap().WithZone("")
	if !ip.IsValid() {
		return true
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return !allowLoopback
	}
	if v4, ok := embeddedIPv4(ip); ok {
		return isInternal(v4, false)
	}
	for _, p := range internalPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// embeddedIPv4 returns the IPv4 address carried by a NAT64 (RFC 6052), 6to4
// (RFC 3056) or IPv4-compatible IPv6 address.
func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	b := ip.As16()
	switch {
	case nat64Prefix.Contains(ip), ipv4CompatPrefix.Contains(ip):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFourPrefix.Contains(ip):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}
