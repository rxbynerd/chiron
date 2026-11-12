package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/httpx"
)

func redirectRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	must := require.New(t)
	u, err := url.Parse(raw)
	must.NoError(err, "url.Parse(%q)", raw)
	return &http.Request{URL: u}
}

func redirectChain(t *testing.T, raws []string) []*http.Request {
	t.Helper()
	var via []*http.Request
	for _, raw := range raws {
		via = append(via, redirectRequest(t, raw))
	}
	return via
}

func TestRedirectPolicy_RefuseUnsafe(t *testing.T) {
	tests := []struct {
		name    string
		via     []string
		target  string
		wantErr string // "" means followed
	}{
		{"same-host https path change followed", []string{"https://api.example.com/v1"}, "https://api.example.com/v2", ""},
		{"same-host loopback http followed", []string{"http://127.0.0.1:8080/v1"}, "http://127.0.0.1:8080/v2", ""},
		{"same-host upgrade to https followed", []string{"http://127.0.0.1:8080/v1"}, "https://127.0.0.1:8080/v1", ""},
		{"implicit https port to explicit default port followed", []string{"https://api.example.com/v1"}, "https://api.example.com:443/v1", ""},
		{"explicit default port to implicit https port followed", []string{"https://api.example.com:443/v1"}, "https://api.example.com/v2", ""},
		{"second redirect followed", []string{"https://api.example.com/a", "https://api.example.com/b"}, "https://api.example.com/c", ""},
		{"cross-host refused", []string{"https://api.example.com/v1"}, "https://evil.example/v1", "cross-origin"},
		{"subdomain refused", []string{"https://example.com/v1"}, "https://api.example.com/v1", "cross-origin"},
		{"same hostname on another port refused", []string{"https://api.example.com/v1"}, "https://api.example.com:8443/v1", "cross-origin"},
		{"cross-host on a later hop refused", []string{"https://api.example.com/a", "https://api.example.com/b"}, "https://evil.example/c", "cross-origin"},
		{"https to http on the same host refused", []string{"https://api.example.com/v1"}, "http://api.example.com/v1", "downgrade"},
		{"downgrade on a later hop refused", []string{"https://api.example.com/a", "https://api.example.com/b"}, "http://api.example.com/c", "downgrade"},
		{"third redirect refused", []string{"https://api.example.com/a", "https://api.example.com/b", "https://api.example.com/c"}, "https://api.example.com/d", "too many redirects"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			err := httpx.RefuseUnsafeRedirects(redirectRequest(t, tt.target), redirectChain(t, tt.via))
			if tt.wantErr == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tt.wantErr)
		})
	}
}

func TestRedirectPolicy_RefuseAll(t *testing.T) {
	tests := []struct {
		name   string
		via    []string
		target string
	}{
		{"same-host https", []string{"https://api.example.com/v1"}, "https://api.example.com/v2"},
		{"same-host loopback http", []string{"http://127.0.0.1:8080/v1"}, "http://127.0.0.1:8080/v2"},
		{"cross-host", []string{"https://api.example.com/v1"}, "https://evil.example/v1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			err := httpx.RefuseAllRedirects(redirectRequest(t, tt.target), redirectChain(t, tt.via))
			must.ErrorContains(err, "redirect")
		})
	}
}

// TestRedirectPolicy_OnHTTPClient drives each policy through a real
// http.Client: the unsafe-only policy follows a same-host redirect and
// refuses a cross-host one without contacting the target, and the refuse-all
// policy stops at the first redirect.
func TestRedirectPolicy_OnHTTPClient(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
	}))
	defer target.Close()

	var nextHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same-host":
			http.Redirect(w, r, "/next", http.StatusFound)
		case "/cross-host":
			http.Redirect(w, r, target.URL+"/next", http.StatusFound)
		case "/next":
			nextHits.Add(1)
		}
	}))
	defer origin.Close()

	get := func(policy func(*http.Request, []*http.Request) error, path string) error {
		hc := *origin.Client()
		hc.CheckRedirect = policy
		resp, err := hc.Get(origin.URL + path)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}

	must.NoError(get(httpx.RefuseUnsafeRedirects, "/same-host"), "RefuseUnsafeRedirects on a same-host redirect")
	check.Equal(int32(1), nextHits.Load(), "same-host target hits")

	must.ErrorContains(get(httpx.RefuseUnsafeRedirects, "/cross-host"), "cross-origin", "RefuseUnsafeRedirects on a cross-host redirect")
	check.Equal(int32(0), targetHits.Load(), "cross-host target hits")

	must.ErrorContains(get(httpx.RefuseAllRedirects, "/same-host"), "redirect", "RefuseAllRedirects on a same-host redirect")
	check.Equal(int32(1), nextHits.Load(), "same-host target hits after RefuseAllRedirects")
}
