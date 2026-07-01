package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newClient builds a Client, failing the test on a construction error.
func newClient(t *testing.T, mutate ...func(*Options)) *Client {
	t.Helper()
	// Tests reach loopback httptest servers, so AllowLoopback defaults true
	// here; the SSRF-refusal tests flip it off explicitly.
	opts := Options{AllowLoopback: true}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
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

func TestFetchLoopbackRefusedWithoutAllow(t *testing.T) {
	// Without AllowLoopback the SSRF guard refuses a loopback destination
	// before any connection is made.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server was reached — the SSRF guard should have refused loopback")
	}))
	defer server.Close()

	c := newClient(t, func(o *Options) { o.AllowLoopback = false })
	_, err := c.Fetch(context.Background(), server.URL)
	if err == nil {
		t.Fatal("Fetch should refuse a loopback URL when AllowLoopback is false")
	}
	if !strings.Contains(err.Error(), "internal") && !strings.Contains(err.Error(), "refus") {
		t.Errorf("error = %v, want an SSRF refusal", err)
	}
}

func TestFetchPrivateAddressRefused(t *testing.T) {
	// Literal private/link-local/unspecified addresses are refused by the
	// guard directly (no DNS needed), so these do not touch the network.
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
	}
	c := newClient(t, func(o *Options) { o.AllowLoopback = false })
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.Fetch(context.Background(), tt.url)
			if err == nil {
				t.Errorf("Fetch(%q) succeeded, want an SSRF refusal", tt.url)
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
	c := newClient(t, func(o *Options) { o.AllowLoopback = true })
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.Fetch(context.Background(), tt.url)
			if err == nil {
				t.Errorf("Fetch(%q) succeeded under AllowLoopback, want it still refused", tt.url)
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
				t.Errorf("Fetch(%q) succeeded, want a scheme rejection", tt.url)
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

	c := newClient(t) // AllowLoopback true
	_, err := c.Fetch(context.Background(), server.URL)
	if err == nil {
		t.Fatal("Fetch should refuse a redirect to an internal address")
	}
	if !strings.Contains(err.Error(), "internal") && !strings.Contains(err.Error(), "refus") {
		t.Errorf("error = %v, want an SSRF refusal on the redirect", err)
	}
}

func TestFetchRedirectDepthCapped(t *testing.T) {
	// An endless redirect loop is stopped by the depth cap rather than
	// followed forever.
	var server *httptest.Server
	hops := 0
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, server.URL+fmt.Sprintf("/%d", hops), http.StatusFound)
	}))
	defer server.Close()

	c := newClient(t)
	_, err := c.Fetch(context.Background(), server.URL)
	if err == nil {
		t.Fatal("Fetch should stop an endless redirect chain")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error = %v, want a redirect-cap error", err)
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
	if err == nil {
		t.Fatal("Fetch should error on a 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want the status surfaced", err)
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
}

func TestFetchBadURLRejected(t *testing.T) {
	c := newClient(t)
	if _, err := c.Fetch(context.Background(), "://not a url"); err == nil {
		t.Fatal("Fetch should reject an unparseable URL")
	}
}
