package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchHostnameDialsCheckedAddress(t *testing.T) {
	// The connection goes to the address the guard checked, while the
	// request still names the host.
	hosts := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hosts <- r.Host:
		default:
		}
		_, _ = w.Write([]byte("public page"))
	}))
	defer server.Close()

	resolver := &fakeResolver{script: map[string][][]string{"public.example": {{publicIP}}}}
	dialer := &testDialer{route: map[string]string{publicIP + ":80": server.Listener.Addr().String()}}
	c := newClient(t, func(o *Options) {
		o.AllowLoopback = false
		o.Resolver = resolver
		o.HTTPClient = dialer.client()
	})

	page, err := c.Fetch(context.Background(), "http://public.example/page")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(page.Content) != "public page" {
		t.Errorf("Content = %q, want the page body", page.Content)
	}
	if got, want := dialer.dialed(), []string{publicIP + ":80"}; !slices.Equal(got, want) {
		t.Errorf("dialled %v, want only the checked address %v", got, want)
	}
	if got := <-hosts; got != "public.example" {
		t.Errorf("Host = %q, want the hostname from the URL", got)
	}
}

func TestFetchHTTPSKeepsHostnameForTLS(t *testing.T) {
	// Dialling the checked address leaves SNI and certificate verification on
	// the hostname: the test certificate covers *.example.com, not publicIP.
	type seen struct{ serverName, host string }
	requests := make(chan seen, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- seen{serverName: r.TLS.ServerName, host: r.Host}:
		default:
		}
		_, _ = w.Write([]byte("secure page"))
	}))
	defer server.Close()

	serverTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httptest client transport is %T, want *http.Transport", server.Client().Transport)
	}
	resolver := &fakeResolver{script: map[string][][]string{"www.example.com": {{publicIP}}}}
	dialer := &testDialer{route: map[string]string{publicIP + ":443": server.Listener.Addr().String()}}
	c := newClient(t, func(o *Options) {
		o.AllowLoopback = false
		o.Resolver = resolver
		o.HTTPClient = &http.Client{Transport: &http.Transport{
			DialContext:     dialer.DialContext,
			TLSClientConfig: serverTransport.TLSClientConfig.Clone(),
		}}
	})

	page, err := c.Fetch(context.Background(), "https://www.example.com/")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(page.Content) != "secure page" {
		t.Errorf("Content = %q, want the page body", page.Content)
	}
	if got := <-requests; got != (seen{serverName: "www.example.com", host: "www.example.com"}) {
		t.Errorf("server saw %+v, want SNI and Host of www.example.com", got)
	}
	if got, want := dialer.dialed(), []string{publicIP + ":443"}; !slices.Equal(got, want) {
		t.Errorf("dialled %v, want only the checked address %v", got, want)
	}
}

func TestFetchHostnameResolvingInternalRefused(t *testing.T) {
	// A hostname is refused if any answer is internal, before a connection
	// is attempted.
	tests := []struct {
		name    string
		answers []string
	}{
		{"rfc1918", []string{"10.0.0.5"}},
		{"cgnat", []string{"100.64.0.1"}},
		{"metadata", []string{"169.254.169.254"}},
		{"mapped rfc1918", []string{"::ffff:10.0.0.5"}},
		{"nat64 metadata", []string{"64:ff9b::a9fe:a9fe"}},
		{"public and private mixed", []string{publicIP, "10.0.0.5"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := &fakeResolver{script: map[string][][]string{"internal.example": {tt.answers}}}
			dialer := &testDialer{}
			c := newClient(t, func(o *Options) {
				o.AllowLoopback = false
				o.Resolver = resolver
				o.HTTPClient = dialer.client()
			})
			_, err := c.Fetch(context.Background(), "http://internal.example/admin")
			if !errors.Is(err, ErrRefusedDestination) {
				t.Fatalf("error = %v, want ErrRefusedDestination", err)
			}
			if !strings.Contains(err.Error(), "internal.example resolves to internal address") {
				t.Errorf("error = %v, want it to name the host and the internal address", err)
			}
			if got := dialer.dialed(); len(got) != 0 {
				t.Errorf("dialled %v, want no connection attempt", got)
			}
		})
	}
}

func TestFetchDNSRebindingRefusedAtDial(t *testing.T) {
	// The pre-flight lookup sees a public address and the dial-time lookup a
	// private one. The dial is refused because the guard checks the answer it
	// is about to connect to, not an earlier one.
	resolver := &fakeResolver{script: map[string][][]string{
		"rebind.example": {{publicIP}, {"10.0.0.1"}},
	}}
	dialer := &testDialer{}
	c := newClient(t, func(o *Options) {
		o.AllowLoopback = false
		o.Resolver = resolver
		o.HTTPClient = dialer.client()
	})

	_, err := c.Fetch(context.Background(), "http://rebind.example/")
	if !errors.Is(err, ErrRefusedDestination) {
		t.Fatalf("error = %v, want ErrRefusedDestination from the dial-time check", err)
	}
	if !strings.Contains(err.Error(), "10.0.0.1") {
		t.Errorf("error = %v, want the rebound address named", err)
	}
	if got := resolver.lookupCount(); got != 2 {
		t.Errorf("resolver saw %d lookups, want 2 (pre-flight and dial)", got)
	}
	if got := dialer.dialed(); len(got) != 0 {
		t.Errorf("dialled %v, want no connection attempt", got)
	}
}

func TestFetchRedirectToHostnameResolvingInternalRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://metadata.example/latest/meta-data", http.StatusFound)
	}))
	defer server.Close()

	resolver := &fakeResolver{script: map[string][][]string{"metadata.example": {{"169.254.169.254"}}}}
	dialer := &testDialer{}
	c := newClient(t, func(o *Options) {
		o.Resolver = resolver
		o.HTTPClient = dialer.client()
	})

	_, err := c.Fetch(context.Background(), server.URL)
	if !errors.Is(err, ErrRefusedDestination) {
		t.Fatalf("error = %v, want ErrRefusedDestination on the redirect", err)
	}
	if got, want := dialer.dialed(), []string{server.Listener.Addr().String()}; !slices.Equal(got, want) {
		t.Errorf("dialled %v, want only the start server %v", got, want)
	}
}

func TestFetchResolveErrorSurfaced(t *testing.T) {
	c := newClient(t) // the default fake resolver knows no hosts
	_, err := c.Fetch(context.Background(), "http://missing.example/")
	if err == nil || !strings.Contains(err.Error(), "resolving missing.example") {
		t.Fatalf("error = %v, want a resolution failure", err)
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		t.Errorf("error = %v, want the *net.DNSError kept in the chain", err)
	}
	if errors.Is(err, ErrRefusedDestination) {
		t.Errorf("error = %v, a resolution failure is not a refused destination", err)
	}
}

func TestFetchEmptyHostRefused(t *testing.T) {
	c := newClient(t)
	_, err := c.Fetch(context.Background(), "http:///x")
	if err == nil || !strings.Contains(err.Error(), "no host") {
		t.Fatalf("error = %v, want a no-host error", err)
	}
}

func TestFetchSlowResolverBoundedByRequestTimeout(t *testing.T) {
	blocking := resolverFunc(func(ctx context.Context, _ string) ([]net.IPAddr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	c := newClient(t, func(o *Options) {
		o.Resolver = blocking
		o.RequestTimeout = 150 * time.Millisecond
	})

	start := time.Now()
	_, err := c.Fetch(context.Background(), "http://slow.example/")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the RequestTimeout deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("Fetch took %s — the 150ms RequestTimeout should have bounded resolution", elapsed)
	}
}

func TestFetchIgnoresProxyEnvironment(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		http.Error(w, "proxied", http.StatusBadGateway)
	}))
	defer proxy.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("direct"))
	}))
	defer origin.Close()

	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	// A non-loopback hostname, because the environment proxy never applies
	// to loopback hosts.
	_, port, err := net.SplitHostPort(origin.Listener.Addr().String())
	if err != nil {
		t.Fatalf("origin address: %v", err)
	}
	resolver := &fakeResolver{script: map[string][][]string{"public.example": {{"127.0.0.1"}}}}
	c := newClient(t, func(o *Options) { o.Resolver = resolver })

	page, err := c.Fetch(context.Background(), "http://public.example:"+port+"/")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(page.Content) != "direct" {
		t.Errorf("Content = %q, want the origin's body", page.Content)
	}
	if got := proxied.Load(); got != 0 {
		t.Errorf("proxy received %d requests, want 0", got)
	}
	// http.ProxyFromEnvironment caches the environment on first use, so the
	// transport's Proxy is checked directly as well.
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Errorf("transport %T has a Proxy func, want none", c.httpClient.Transport)
	}
}

func TestFetchCallerTransportProxyDropped(t *testing.T) {
	// A caller-supplied transport keeps its dialer, loses its proxy, and is
	// not mutated.
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		http.Error(w, "proxied", http.StatusBadGateway)
	}))
	defer proxy.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("direct"))
	}))
	defer origin.Close()

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("proxy URL: %v", err)
	}
	dialer := &testDialer{route: map[string]string{publicIP + ":80": origin.Listener.Addr().String()}}
	callerTransport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DialContext: dialer.DialContext}
	caller := &http.Client{Transport: callerTransport}
	resolver := &fakeResolver{script: map[string][][]string{"public.example": {{publicIP}}}}
	c := newClient(t, func(o *Options) {
		o.AllowLoopback = false
		o.Resolver = resolver
		o.HTTPClient = caller
	})

	page, err := c.Fetch(context.Background(), "http://public.example/")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(page.Content) != "direct" {
		t.Errorf("Content = %q, want the origin's body", page.Content)
	}
	if got := proxied.Load(); got != 0 {
		t.Errorf("proxy received %d requests, want 0", got)
	}
	if got, want := dialer.dialed(), []string{publicIP + ":80"}; !slices.Equal(got, want) {
		t.Errorf("dialled %v, want only the checked address %v", got, want)
	}
	if caller.Transport != callerTransport || callerTransport.Proxy == nil || caller.CheckRedirect != nil {
		t.Error("New mutated the caller's client or transport")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNewRejectsUnwrappableTransport(t *testing.T) {
	tests := []struct {
		name string
		rt   http.RoundTripper
	}{
		{"not an *http.Transport", roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("unused")
		})},
		{"custom TLS dialer", &http.Transport{DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("unused")
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(Options{HTTPClient: &http.Client{Transport: tt.rt}}); err == nil {
				t.Fatal("New succeeded, want an error for a transport the guard cannot wrap")
			}
		})
	}
}

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
