package fetch

import "net/netip"

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
