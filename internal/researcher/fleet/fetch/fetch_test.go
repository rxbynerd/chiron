package fetch

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// publicIP is a documentation address the guard treats as public. Tests only
// reach it through a testDialer route.
const publicIP = "203.0.113.10"

// newClient builds a Client, failing the test on a construction error.
func newClient(t *testing.T, mutate ...func(*Options)) *Client {
	t.Helper()
	must := require.New(t)
	// Tests reach loopback httptest servers, so AllowLoopback defaults true
	// here; the SSRF-refusal tests flip it off explicitly. The default
	// resolver answers nothing, so no test reaches real DNS.
	opts := Options{AllowLoopback: true, Resolver: &fakeResolver{}}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := New(opts)
	must.NoError(err, "New should accept the test options")
	return c
}

// fakeResolver answers lookups from script without touching DNS. Each lookup
// of a host consumes its next answer, the last one repeating; a host with no
// script is not found.
type fakeResolver struct {
	mu      sync.Mutex
	script  map[string][][]string
	lookups int
}

func (r *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups++
	answers := r.script[host]
	if len(answers) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	answer := answers[0]
	if len(answers) > 1 {
		r.script[host] = answers[1:]
	}
	addrs := make([]net.IPAddr, 0, len(answer))
	for _, s := range answer {
		addrs = append(addrs, net.IPAddr{IP: net.ParseIP(s)})
	}
	return addrs, nil
}

func (r *fakeResolver) lookupCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lookups
}

// resolverFunc adapts a function to the Resolver interface.
type resolverFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

func (f resolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

// testDialer is a Transport.DialContext that records every address it is
// asked to dial. An address in route connects to its loopback target, any
// other loopback address connects directly, and anything else is refused, so
// no test reaches the real network.
type testDialer struct {
	route map[string]string
	mu    sync.Mutex
	addrs []string
}

func (d *testDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()

	target, ok := d.route[addr]
	if !ok {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if ip, err := netip.ParseAddr(host); err != nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("test dialer: refusing non-loopback address %s", addr)
		}
		target = addr
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, target)
}

func (d *testDialer) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.addrs)
}

// client returns an *http.Client whose transport dials through d.
func (d *testDialer) client() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: d.DialContext}}
}

func TestFetch_ReturnsBodyAndMetadata(t *testing.T) {
	must := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>the sky is blue</body></html>"))
	}))
	defer server.Close()

	c := newClient(t)
	page, err := c.Fetch(context.Background(), server.URL)
	must.NoError(err)
	must.Equal("<html><body>the sky is blue</body></html>", string(page.Content))
	must.False(page.Truncated)
	must.Equal("text/html; charset=utf-8", page.ContentType)
	must.Equal(server.URL, page.URL)
	must.Zero(page.Spend.USD, "a direct fetch bills nothing")
	must.False(page.Spend.Unknown)
}

func TestFetch_SendsIdentifyingHeaders(t *testing.T) {
	// Every request, redirect hops included, identifies Chiron (or the
	// configured agent) and prefers textual content.
	tests := []struct {
		name      string
		userAgent string
		want      string
	}{
		{"default agent", "", defaultUserAgent},
		{"configured agent", "chiron-test/1", "chiron-test/1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			type headers struct{ userAgent, accept string }
			seen := make(chan headers, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case seen <- headers{r.UserAgent(), r.Header.Get("Accept")}:
				default:
				}
				if r.URL.Path == "/start" {
					http.Redirect(w, r, "/final", http.StatusFound)
					return
				}
				_, _ = w.Write([]byte("ok"))
			}))
			defer server.Close()

			c := newClient(t, func(o *Options) { o.UserAgent = tt.userAgent })
			_, err := c.Fetch(context.Background(), server.URL+"/start")
			must.NoError(err)
			want := headers{tt.want, acceptHeader}
			for hop := range 2 {
				got := <-seen
				must.Equal(want, got, "request %d headers", hop)
			}
		})
	}
}

func TestFetch_RefusesLoopbackWithoutAllowLoopback(t *testing.T) {
	must := require.New(t)
	// Without AllowLoopback the SSRF guard refuses a loopback destination
	// before any connection is made.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server was reached — the SSRF guard should have refused loopback")
	}))
	defer server.Close()

	c := newClient(t, func(o *Options) { o.AllowLoopback = false })
	_, err := c.Fetch(context.Background(), server.URL)
	must.ErrorIs(err, ErrRefusedDestination)
}

func TestFetchPrivateAddressRefused(t *testing.T) {
	// Literal internal addresses are refused without DNS and without a dial.
	tests := []struct {
		name string
		url  string
	}{
		{"rfc1918 10/8", "http://10.0.0.1/x"},
		{"rfc1918 192.168/16", "http://192.168.1.1/x"},
		{"rfc1918 172.16/12", "http://172.16.0.1/x"},
		{"link-local 169.254/16", "http://169.254.169.254/latest/meta-data"},
		{"loopback literal", "http://127.0.0.1/x"},
		{"ipv6 loopback", "http://[::1]/x"},
		{"ipv6 link-local", "http://[fe80::1]/x"},
		{"unspecified", "http://0.0.0.0/x"},
		{"cgnat metadata", "http://100.100.100.200/latest/meta-data"},
		{"mapped loopback", "http://[::ffff:127.0.0.1]/x"},
		{"mapped metadata", "http://[::ffff:169.254.169.254]/x"},
		{"unique local", "http://[fd00::1]/x"},
		{"nat64 metadata", "http://[64:ff9b::a9fe:a9fe]/x"},
		{"6to4 metadata", "http://[2002:a9fe:a9fe::1]/x"},
		{"zoned link-local", "http://[fe80::1%25eth0]/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			dialer := &testDialer{}
			c := newClient(t, func(o *Options) {
				o.AllowLoopback = false
				o.HTTPClient = dialer.client()
			})
			_, err := c.Fetch(context.Background(), tt.url)
			must.ErrorIs(err, ErrRefusedDestination)
			must.Empty(dialer.dialed(), "no connection attempt should be dialled")
		})
	}
}

func TestFetchAllowLoopbackDoesNotRelaxPrivate(t *testing.T) {
	// AllowLoopback exempts loopback/unspecified only; private and link-local
	// remain refused even with it set, so a loopback test harness cannot be
	// used to reach an internal production or metadata address.
	tests := []struct {
		name string
		url  string
	}{
		{"rfc1918 still refused", "http://10.0.0.1/x"},
		{"link-local metadata still refused", "http://169.254.169.254/latest/meta-data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			dialer := &testDialer{}
			c := newClient(t, func(o *Options) {
				o.AllowLoopback = true
				o.HTTPClient = dialer.client()
			})
			_, err := c.Fetch(context.Background(), tt.url)
			must.ErrorIs(err, ErrRefusedDestination)
			must.Empty(dialer.dialed(), "no connection attempt should be dialled")
		})
	}
}

func TestFetchSchemeRejected(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"file", "file:///etc/passwd"},
		{"ftp", "ftp://example.com/x"},
		{"javascript", "javascript:alert(1)"},
		{"data", "data:text/plain;base64,aGk="},
		{"gopher", "gopher://example.com/x"},
	}
	c := newClient(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			_, err := c.Fetch(context.Background(), tt.url)
			must.Error(err, "Fetch(%q) should be rejected", tt.url)
			must.ErrorContains(err, "scheme")
		})
	}
}

func TestFetch_TruncatesOversizedBody(t *testing.T) {
	must := require.New(t)
	// A body over the bound is not an error: Content holds the prefix and
	// Truncated is set (the deliberate difference from the model adapter).
	body := strings.Repeat("x", 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := newClient(t, func(o *Options) { o.MaxContentBytes = 512 })
	page, err := c.Fetch(context.Background(), server.URL)
	must.NoError(err, "Fetch should truncate, not error")
	must.True(page.Truncated)
	must.Len(page.Content, 512)
}

func TestFetch_DefaultContentBoundIsOneMiB(t *testing.T) {
	must := require.New(t)
	// With MaxContentBytes unset, reads stop at 1 MiB.
	body := strings.Repeat("x", 1<<20+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := newClient(t)
	page, err := c.Fetch(context.Background(), server.URL)
	must.NoError(err)
	must.True(page.Truncated)
	must.Len(page.Content, 1<<20)
}

func TestFetch_BodyAtBoundNotTruncated(t *testing.T) {
	must := require.New(t)
	// A body exactly at the bound is complete, not truncated.
	body := strings.Repeat("x", 512)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := newClient(t, func(o *Options) { o.MaxContentBytes = 512 })
	page, err := c.Fetch(context.Background(), server.URL)
	must.NoError(err)
	must.False(page.Truncated)
	must.Len(page.Content, 512)
}

func TestFetch_FollowsSameHostRedirect(t *testing.T) {
	must := require.New(t)
	// A same-host redirect to an allowed destination is followed.
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, server.URL+"/final", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("final page"))
	}))
	defer server.Close()

	c := newClient(t)
	page, err := c.Fetch(context.Background(), server.URL+"/start")
	must.NoError(err)
	must.Equal("final page", string(page.Content))
	must.True(strings.HasSuffix(page.URL, "/final"), "URL = %q, want the final redirected URL", page.URL)
}

func TestFetchRedirectToPrivateRefused(t *testing.T) {
	must := require.New(t)
	// A redirect to an internal address is refused after the redirect, so the
	// guard re-checks every hop. The target is link-local, which AllowLoopback
	// does not exempt, so only the loopback start server is dialled.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer server.Close()

	dialer := &testDialer{}
	// AllowLoopback stays on (the default) so the start server is reachable.
	c := newClient(t, func(o *Options) { o.HTTPClient = dialer.client() })
	_, err := c.Fetch(context.Background(), server.URL)
	must.ErrorIs(err, ErrRefusedDestination)
	must.Equal([]string{server.Listener.Addr().String()}, dialer.dialed(), "only the start server should be dialled")
}

func TestFetch_RefusalOmitsRedirectUserinfo(t *testing.T) {
	must := require.New(t)
	// A refusal keeps its error chain for classification, so its message
	// must still carry no userinfo from the requested URL.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer server.Close()

	withCreds := strings.Replace(server.URL, "http://", "http://user:s3cr3t@", 1)
	dialer := &testDialer{}
	c := newClient(t, func(o *Options) { o.HTTPClient = dialer.client() })
	_, err := c.Fetch(context.Background(), withCreds)
	must.ErrorIs(err, ErrRefusedDestination)
	must.NotContains(err.Error(), "s3cr3t", "refusal leaked userinfo")
	must.NotContains(err.Error(), "user:", "refusal leaked userinfo")
}

func TestFetchRedirectDepthCapped(t *testing.T) {
	must := require.New(t)
	// An endless redirect loop is stopped by the depth cap rather than
	// followed forever: the first request plus four followed redirects reach
	// the server, and the fifth redirect is refused.
	var server *httptest.Server
	var hops atomic.Int32
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hops.Add(1)
		http.Redirect(w, r, server.URL+fmt.Sprintf("/%d", n), http.StatusFound)
	}))
	defer server.Close()

	c := newClient(t)
	_, err := c.Fetch(context.Background(), server.URL)
	must.Error(err, "Fetch should stop an endless redirect chain")
	must.ErrorContains(err, fmt.Sprintf("stopped after %d redirects", maxRedirects))
	must.EqualValues(maxRedirects, hops.Load(), "requests reaching the server")
}

func TestFetch_Non2xxIsHTTPStatusError(t *testing.T) {
	must := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer server.Close()

	c := newClient(t)
	_, err := c.Fetch(context.Background(), server.URL)
	var statusErr *HTTPStatusError
	must.ErrorAs(err, &statusErr)
	must.Equal(http.StatusNotFound, statusErr.Status)
	must.ErrorContains(err, "404")
	must.NotErrorIs(err, ErrRefusedDestination, "a 404 is not a refused destination")
}

func TestFetch_CallerDeadlineBoundsCall(t *testing.T) {
	must := require.New(t)
	// A tighter caller deadline fires even though RequestTimeout is generous.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()

	c := newClient(t, func(o *Options) { o.RequestTimeout = 10 * time.Second })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Fetch(ctx, server.URL)
	must.Error(err, "Fetch should fail when the caller deadline fires")
	elapsed := time.Since(start)
	must.LessOrEqual(elapsed, 1500*time.Millisecond, "the caller's 100ms deadline should have won")
}

func TestFetch_RequestTimeoutBoundsCall(t *testing.T) {
	must := require.New(t)
	// With no caller deadline, the client's own RequestTimeout bounds the call.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()

	c := newClient(t, func(o *Options) { o.RequestTimeout = 150 * time.Millisecond })
	start := time.Now()
	_, err := c.Fetch(context.Background(), server.URL)
	must.Error(err, "Fetch should fail when RequestTimeout fires")
	elapsed := time.Since(start)
	must.LessOrEqual(elapsed, 1500*time.Millisecond, "the 150ms RequestTimeout should have bounded it")
}

func TestFetch_StripsUserinfoFromPageURL(t *testing.T) {
	must := require.New(t)
	// A URL with embedded userinfo must have it stripped from the returned
	// Page.URL so it is safe to log or cite.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	withCreds := strings.Replace(server.URL, "http://", "http://user:s3cr3t@", 1)
	c := newClient(t)
	page, err := c.Fetch(context.Background(), withCreds)
	must.NoError(err)
	must.NotContains(page.URL, "s3cr3t")
	must.NotContains(page.URL, "user:")
}

func TestFetch_ErrorOmitsUserinfo(t *testing.T) {
	must := require.New(t)
	// A failing fetch of a URL with userinfo must not echo the credential in
	// the error. Point at a closed port so the dial fails.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := closed.URL
	closed.Close() // now nothing is listening

	withCreds := strings.Replace(closedURL, "http://", "http://user:s3cr3t@", 1)
	c := newClient(t)
	_, err := c.Fetch(context.Background(), withCreds)
	must.Error(err, "Fetch should fail dialling a closed port")
	must.NotContains(err.Error(), "s3cr3t", "error leaked userinfo credential")
	must.NotErrorIs(err, ErrRefusedDestination, "a dial failure is not a refused destination")
}

func TestFetch_RejectsUnparseableURL(t *testing.T) {
	must := require.New(t)
	c := newClient(t)
	_, err := c.Fetch(context.Background(), "://not a url")
	must.Error(err, "Fetch should reject an unparseable URL")
}
