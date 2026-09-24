package fetch

import (
	"context"
	"errors"
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
)

// publicIP is a documentation address the guard treats as public. Tests only
// reach it through a testDialer route.
const publicIP = "203.0.113.10"

// newClient builds a Client, failing the test on a construction error.
func newClient(t *testing.T, mutate ...func(*Options)) *Client {
	t.Helper()
	// Tests reach loopback httptest servers, so AllowLoopback defaults true
	// here; the SSRF-refusal tests flip it off explicitly. The default
	// resolver answers nothing, so no test reaches real DNS.
	opts := Options{AllowLoopback: true, Resolver: &fakeResolver{}}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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

func TestFetchHappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>the sky is blue</body></html>"))
	}))
	defer server.Close()

	c := newClient(t)
	page, err := c.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(page.Content) != "<html><body>the sky is blue</body></html>" {
		t.Errorf("Content = %q, want the page body", page.Content)
	}
	if page.Truncated {
		t.Errorf("Truncated = true, want false for an in-bound page")
	}
	if page.ContentType != "text/html; charset=utf-8" {
		t.Errorf("ContentType = %q, want the header echoed", page.ContentType)
	}
	if page.URL != server.URL {
		t.Errorf("URL = %q, want %q", page.URL, server.URL)
	}
}

func TestFetchSendsIdentifyingHeaders(t *testing.T) {
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
			if _, err := c.Fetch(context.Background(), server.URL+"/start"); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			want := headers{tt.want, acceptHeader}
			for hop := range 2 {
				if got := <-seen; got != want {
					t.Errorf("request %d headers = %+v, want %+v", hop, got, want)
				}
			}
		})
	}
}

func TestFetchLoopbackRefusedWithoutAllow(t *testing.T) {
	// Without AllowLoopback the SSRF guard refuses a loopback destination
	// before any connection is made.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server was reached — the SSRF guard should have refused loopback")
	}))
	defer server.Close()

	c := newClient(t, func(o *Options) { o.AllowLoopback = false })
	_, err := c.Fetch(context.Background(), server.URL)
	if !errors.Is(err, ErrRefusedDestination) {
		t.Fatalf("error = %v, want ErrRefusedDestination for loopback without AllowLoopback", err)
	}
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
			dialer := &testDialer{}
			c := newClient(t, func(o *Options) {
				o.AllowLoopback = false
				o.HTTPClient = dialer.client()
			})
			_, err := c.Fetch(context.Background(), tt.url)
			if !errors.Is(err, ErrRefusedDestination) {
				t.Errorf("Fetch(%q) error = %v, want ErrRefusedDestination", tt.url, err)
			}
			if got := dialer.dialed(); len(got) != 0 {
				t.Errorf("dialled %v, want no connection attempt", got)
			}
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
			dialer := &testDialer{}
			c := newClient(t, func(o *Options) {
				o.AllowLoopback = true
				o.HTTPClient = dialer.client()
			})
			_, err := c.Fetch(context.Background(), tt.url)
			if !errors.Is(err, ErrRefusedDestination) {
				t.Errorf("Fetch(%q) under AllowLoopback error = %v, want ErrRefusedDestination", tt.url, err)
			}
			if got := dialer.dialed(); len(got) != 0 {
				t.Errorf("dialled %v, want no connection attempt", got)
			}
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
			_, err := c.Fetch(context.Background(), tt.url)
			if err == nil {
				t.Fatalf("Fetch(%q) succeeded, want a scheme rejection", tt.url)
			}
			if !strings.Contains(err.Error(), "scheme") {
				t.Errorf("error = %v, want a scheme error", err)
			}
		})
	}
}

func TestFetchOversizedTruncated(t *testing.T) {
	// A body over the bound is not an error: Content holds the prefix and
	// Truncated is set (the deliberate difference from the model adapter).
	body := strings.Repeat("x", 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := newClient(t, func(o *Options) { o.MaxContentBytes = 512 })
	page, err := c.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch should truncate, not error: %v", err)
	}
	if !page.Truncated {
		t.Errorf("Truncated = false, want true for an oversize body")
	}
	if len(page.Content) != 512 {
		t.Errorf("Content length = %d, want the 512-byte bound", len(page.Content))
	}
}

func TestFetchDefaultContentBound(t *testing.T) {
	// With MaxContentBytes unset, reads stop at 1 MiB.
	body := strings.Repeat("x", 1<<20+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := newClient(t)
	page, err := c.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !page.Truncated || len(page.Content) != 1<<20 {
		t.Errorf("Truncated = %v, len(Content) = %d; want true and %d", page.Truncated, len(page.Content), 1<<20)
	}
}

func TestFetchExactBoundNotTruncated(t *testing.T) {
	// A body exactly at the bound is complete, not truncated.
	body := strings.Repeat("x", 512)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := newClient(t, func(o *Options) { o.MaxContentBytes = 512 })
	page, err := c.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if page.Truncated {
		t.Errorf("Truncated = true, want false for a body exactly at the bound")
	}
	if len(page.Content) != 512 {
		t.Errorf("Content length = %d, want 512", len(page.Content))
	}
}

func TestFetchRedirectFollowedSameHost(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(page.Content) != "final page" {
		t.Errorf("Content = %q, want the redirect target's body", page.Content)
	}
	if !strings.HasSuffix(page.URL, "/final") {
		t.Errorf("URL = %q, want the final redirected URL", page.URL)
	}
}

func TestFetchRedirectToPrivateRefused(t *testing.T) {
	// A redirect to an internal address must be refused AFTER the redirect,
	// not just on the initial URL — the guard re-checks every hop.
	//
	// AllowLoopback is true so the loopback start server is reachable, but the
	// redirect target is the link-local metadata address (169.254.169.254),
	// which AllowLoopback deliberately does NOT exempt. So the initial hop is
	// permitted and the redirect is refused — proving the re-check fires on
	// the redirect, not merely the initial URL.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer server.Close()

	dialer := &testDialer{}
	c := newClient(t, func(o *Options) { o.HTTPClient = dialer.client() }) // AllowLoopback true
	_, err := c.Fetch(context.Background(), server.URL)
	if !errors.Is(err, ErrRefusedDestination) {
		t.Fatalf("error = %v, want ErrRefusedDestination on the redirect", err)
	}
	if got, want := dialer.dialed(), []string{server.Listener.Addr().String()}; !slices.Equal(got, want) {
		t.Errorf("dialled %v, want only the start server %v", got, want)
	}
}

func TestFetchRefusedRedirectNeverLeaksUserinfo(t *testing.T) {
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
	if !errors.Is(err, ErrRefusedDestination) {
		t.Fatalf("error = %v, want ErrRefusedDestination on the redirect", err)
	}
	if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "user:") {
		t.Fatalf("refusal leaked userinfo: %v", err)
	}
}

func TestFetchRedirectDepthCapped(t *testing.T) {
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
	if err == nil {
		t.Fatal("Fetch should stop an endless redirect chain")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("stopped after %d redirects", maxRedirects)) {
		t.Errorf("error = %v, want the redirect-cap error", err)
	}
	if got := hops.Load(); got != maxRedirects {
		t.Errorf("server saw %d requests, want %d", got, maxRedirects)
	}
}

func TestFetchNon2xxIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer server.Close()

	c := newClient(t)
	_, err := c.Fetch(context.Background(), server.URL)
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error = %v, want an *HTTPStatusError", err)
	}
	if statusErr.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want 404", statusErr.Status)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want the status in the message", err)
	}
	if errors.Is(err, ErrRefusedDestination) {
		t.Errorf("error = %v, a 404 is not a refused destination", err)
	}
}

func TestFetchTimeoutHonoured(t *testing.T) {
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
	if err == nil {
		t.Fatal("Fetch should fail when the caller deadline fires")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("Fetch took %s — the caller's 100ms deadline should have won", elapsed)
	}
}

func TestFetchRequestTimeoutHonoured(t *testing.T) {
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
	if err == nil {
		t.Fatal("Fetch should fail when RequestTimeout fires")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("Fetch took %s — the 150ms RequestTimeout should have bounded it", elapsed)
	}
}

func TestFetchUserinfoStrippedFromURL(t *testing.T) {
	// A URL with embedded userinfo must have it stripped from the returned
	// Page.URL so it is safe to log or cite.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	// Inject userinfo into the loopback URL.
	withCreds := strings.Replace(server.URL, "http://", "http://user:s3cr3t@", 1)
	c := newClient(t)
	page, err := c.Fetch(context.Background(), withCreds)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if strings.Contains(page.URL, "s3cr3t") || strings.Contains(page.URL, "user:") {
		t.Errorf("Page.URL leaked userinfo: %q", page.URL)
	}
}

func TestFetchErrorNeverLeaksUserinfo(t *testing.T) {
	// A failing fetch of a URL with userinfo must not echo the credential in
	// the error. Point at a closed port so the dial fails.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := closed.URL
	closed.Close() // now nothing is listening

	withCreds := strings.Replace(addr, "http://", "http://user:s3cr3t@", 1)
	c := newClient(t)
	_, err := c.Fetch(context.Background(), withCreds)
	if err == nil {
		t.Fatal("Fetch should fail dialling a closed port")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("error leaked userinfo credential: %v", err)
	}
	if errors.Is(err, ErrRefusedDestination) {
		t.Errorf("error = %v, a dial failure is not a refused destination", err)
	}
}

func TestFetchBadURLRejected(t *testing.T) {
	c := newClient(t)
	if _, err := c.Fetch(context.Background(), "://not a url"); err == nil {
		t.Fatal("Fetch should reject an unparseable URL")
	}
}
