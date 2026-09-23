package fetch

import (
	"net/netip"
	"testing"
)

func TestIsInternal(t *testing.T) {
	// internal is the verdict with AllowLoopback off; allowed is the verdict
	// with it on, which may differ only for loopback and unspecified.
	tests := []struct {
		name     string
		addr     string
		internal bool
		allowed  bool
	}{
		{"ipv4 unspecified", "0.0.0.0", true, false},
		{"this network 0/8", "0.0.0.1", true, true},
		{"loopback", "127.0.0.1", true, false},
		{"loopback 127/8", "127.0.0.2", true, false},
		{"rfc1918 10/8", "10.0.0.1", true, true},
		{"rfc1918 172.16/12", "172.16.0.1", true, true},
		{"rfc1918 192.168/16", "192.168.1.1", true, true},
		{"link-local metadata", "169.254.169.254", true, true},
		{"cgnat low", "100.64.0.1", true, true},
		{"cgnat alibaba metadata", "100.100.100.200", true, true},
		{"cgnat high", "100.127.255.255", true, true},
		{"above cgnat", "100.128.0.1", false, false},
		{"ietf protocol assignments", "192.0.0.8", true, true},
		{"benchmarking low", "198.18.0.1", true, true},
		{"benchmarking high", "198.19.255.255", true, true},
		{"above benchmarking", "198.20.0.1", false, false},
		{"multicast", "224.0.0.1", true, true},
		{"multicast ssdp", "239.255.255.250", true, true},
		{"reserved 240/4", "240.0.0.1", true, true},
		{"broadcast", "255.255.255.255", true, true},
		{"public ipv4", "8.8.8.8", false, false},

		{"ipv6 unspecified", "::", true, false},
		{"ipv6 loopback", "::1", true, false},
		{"mapped loopback", "::ffff:127.0.0.1", true, false},
		{"mapped rfc1918", "::ffff:10.0.0.1", true, true},
		{"mapped metadata", "::ffff:169.254.169.254", true, true},
		{"mapped public", "::ffff:8.8.8.8", false, false},
		{"unique local fc00", "fc00::1", true, true},
		{"unique local fd00", "fd00::1", true, true},
		{"aws ipv6 metadata", "fd00:ec2::254", true, true},
		{"ipv6 link-local", "fe80::1", true, true},
		{"ipv6 link-local with zone", "fe80::1%eth0", true, true},
		{"deprecated site-local", "fec0::1", true, true},
		{"ipv6 multicast link-local", "ff02::1", true, true},
		{"ipv6 multicast global", "ff0e::1", true, true},
		{"nat64 metadata", "64:ff9b::a9fe:a9fe", true, true},
		{"nat64 rfc1918", "64:ff9b::a00:5", true, true},
		{"nat64 loopback", "64:ff9b::7f00:1", true, true},
		{"nat64 public", "64:ff9b::808:808", false, false},
		{"local-use nat64", "64:ff9b:1::808:808", true, true},
		{"6to4 metadata", "2002:a9fe:a9fe::1", true, true},
		{"6to4 rfc1918", "2002:a00:1::1", true, true},
		{"6to4 public", "2002:808:808::1", false, false},
		{"ipv4-compatible metadata", "::a9fe:a9fe", true, true},
		{"ipv4-compatible loopback", "::7f00:1", true, true},
		{"ipv4-compatible this network", "::2", true, true},
		{"ipv4-compatible public", "::808:808", false, false},
		{"public ipv6", "2606:4700:4700::1111", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := netip.MustParseAddr(tt.addr)
			if got := isInternal(ip, false); got != tt.internal {
				t.Errorf("isInternal(%s, false) = %v, want %v", tt.addr, got, tt.internal)
			}
			if got := isInternal(ip, true); got != tt.allowed {
				t.Errorf("isInternal(%s, true) = %v, want %v", tt.addr, got, tt.allowed)
			}
		})
	}
}

func TestIsInternalRefusesInvalidAddress(t *testing.T) {
	if !isInternal(netip.Addr{}, true) {
		t.Fatal("isInternal(zero Addr) = false, want an unparseable address refused")
	}
}
