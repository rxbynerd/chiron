package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetch_Hostname_DialsCheckedAddress(t *testing.T) {
	// The connection goes to the address the guard checked, while the
	// request still names the host.
	must := require.New(t)
	check := assert.New(t)

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
	must.NoError(err)
	check.Equal("public page", string(page.Content), "page body")
	check.Equal([]string{publicIP + ":80"}, dialer.dialed(), "dialled addresses")
	check.Equal("public.example", <-hosts, "Host header")
}

func TestFetch_HTTPS_KeepsHostnameForTLS(t *testing.T) {
	// Dialling the checked address leaves SNI and certificate verification on
	// the hostname: the test certificate covers *.example.com, not publicIP.
	must := require.New(t)
	check := assert.New(t)

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
	must.Truef(ok, "httptest client transport is %T, want *http.Transport", server.Client().Transport)
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
	must.NoError(err)
	check.Equal("secure page", string(page.Content), "page body")
	check.Equal(seen{serverName: "www.example.com", host: "www.example.com"}, <-requests, "SNI and Host")
	check.Equal([]string{publicIP + ":443"}, dialer.dialed(), "dialled addresses")
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
			must := require.New(t)

			resolver := &fakeResolver{script: map[string][][]string{"internal.example": {tt.answers}}}
			dialer := &testDialer{}
			c := newClient(t, func(o *Options) {
				o.AllowLoopback = false
				o.Resolver = resolver
				o.HTTPClient = dialer.client()
			})
			_, err := c.Fetch(context.Background(), "http://internal.example/admin")
			must.ErrorIs(err, ErrRefusedDestination)
			must.ErrorContains(err, "internal.example resolves to internal address")
			must.Empty(dialer.dialed())
		})
	}
}

func TestFetch_DNSRebinding_RefusedAtDial(t *testing.T) {
	// The pre-flight lookup sees a public address and the dial-time lookup a
	// private one. The dial is refused because the guard checks the answer it
	// is about to connect to, not an earlier one.
	must := require.New(t)
	check := assert.New(t)

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
	must.ErrorIs(err, ErrRefusedDestination)
	must.ErrorContains(err, "10.0.0.1", "rebound address")
	check.Equal(2, resolver.lookupCount(), "lookups for the pre-flight check and the dial")
	check.Empty(dialer.dialed(), "connection attempts")
}

func TestFetchRedirectToHostnameResolvingInternalRefused(t *testing.T) {
	must := require.New(t)

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
	must.ErrorIs(err, ErrRefusedDestination)
	must.Equal([]string{server.Listener.Addr().String()}, dialer.dialed())
}

func TestFetchResolveErrorSurfaced(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	c := newClient(t) // the default fake resolver knows no hosts
	_, err := c.Fetch(context.Background(), "http://missing.example/")
	must.ErrorContains(err, "resolving missing.example")

	var dnsErr *net.DNSError
	must.ErrorAs(err, &dnsErr, "the *net.DNSError should stay in the chain")
	check.NotErrorIs(err, ErrRefusedDestination, "a resolution failure is not a refused destination")
}

func TestFetchEmptyHostRefused(t *testing.T) {
	must := require.New(t)

	c := newClient(t)
	_, err := c.Fetch(context.Background(), "http:///x")
	must.ErrorContains(err, "no host")
}

func TestFetch_SlowResolver_BoundedByRequestTimeout(t *testing.T) {
	must := require.New(t)

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
	must.ErrorIs(err, context.DeadlineExceeded)
	must.LessOrEqual(time.Since(start), 1500*time.Millisecond, "the 150ms RequestTimeout should bound resolution")
}

func TestFetch_IgnoresProxyEnvironment(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

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
	must.NoError(err, "origin address")
	resolver := &fakeResolver{script: map[string][][]string{"public.example": {{"127.0.0.1"}}}}
	c := newClient(t, func(o *Options) { o.Resolver = resolver })

	page, err := c.Fetch(context.Background(), "http://public.example:"+port+"/")
	must.NoError(err)
	check.Equal("direct", string(page.Content), "page body")
	check.Zero(proxied.Load(), "proxy requests")
	// http.ProxyFromEnvironment caches the environment on first use, so the
	// transport's Proxy is checked directly as well.
	tr, ok := c.httpClient.Transport.(*http.Transport)
	must.Truef(ok, "transport is %T, want *http.Transport", c.httpClient.Transport)
	check.Nil(tr.Proxy, "transport Proxy func")
}

func TestFetch_CallerTransportProxyDropped(t *testing.T) {
	// A caller-supplied transport keeps its dialer, loses its proxy, and is
	// not mutated.
	must := require.New(t)
	check := assert.New(t)

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
	must.NoError(err, "proxy URL")
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
	must.NoError(err)
	check.Equal("direct", string(page.Content), "page body")
	check.Zero(proxied.Load(), "proxy requests")
	check.Equal([]string{publicIP + ":80"}, dialer.dialed(), "dialled addresses")
	check.Same(callerTransport, caller.Transport, "caller transport replaced")
	check.NotNil(callerTransport.Proxy, "caller transport proxy cleared")
	check.Nil(caller.CheckRedirect, "caller CheckRedirect set")
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNew_RejectsUnwrappableTransport(t *testing.T) {
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
			must := require.New(t)

			_, err := New(Options{HTTPClient: &http.Client{Transport: tt.rt}})
			must.Error(err)
		})
	}
}
