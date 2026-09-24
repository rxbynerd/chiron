package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rxbynerd/chiron/internal/httpx"
)

func redirectRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return &http.Request{URL: u}
}

func TestRefuseUnsafeRedirects(t *testing.T) {
	tests := []struct {
		name    string
		via     []string
		target  string
		wantErr string // "" means followed
	}{
		{"same-host https path change followed", []string{"https://api.example.com/v1"}, "https://api.example.com/v2", ""},
		{"same-host loopback http followed", []string{"http://127.0.0.1:8080/v1"}, "http://127.0.0.1:8080/v2", ""},
		{"same-host upgrade to https followed", []string{"http://127.0.0.1:8080/v1"}, "https://127.0.0.1:8080/v1", ""},
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
			var via []*http.Request
			for _, v := range tt.via {
				via = append(via, redirectRequest(t, v))
			}
			err := httpx.RefuseUnsafeRedirects(redirectRequest(t, tt.target), via)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("RefuseUnsafeRedirects = %v, want the redirect followed", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("RefuseUnsafeRedirects = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRefuseAllRedirects(t *testing.T) {
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
			var via []*http.Request
			for _, v := range tt.via {
				via = append(via, redirectRequest(t, v))
			}
			err := httpx.RefuseAllRedirects(redirectRequest(t, tt.target), via)
			if err == nil || !strings.Contains(err.Error(), "redirect") {
				t.Errorf("RefuseAllRedirects = %v, want a redirect refusal", err)
			}
		})
	}
}

// TestRedirectPoliciesOnAClient drives each policy through a real
// http.Client: the unsafe-only policy follows a same-host redirect and
// refuses a cross-host one without contacting the target, and the refuse-all
// policy stops at the first redirect.
func TestRedirectPoliciesOnAClient(t *testing.T) {
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

	if err := get(httpx.RefuseUnsafeRedirects, "/same-host"); err != nil {
		t.Fatalf("RefuseUnsafeRedirects on a same-host redirect: %v", err)
	}
	if got := nextHits.Load(); got != 1 {
		t.Errorf("same-host target hits = %d, want 1", got)
	}

	if err := get(httpx.RefuseUnsafeRedirects, "/cross-host"); err == nil || !strings.Contains(err.Error(), "cross-origin") {
		t.Errorf("RefuseUnsafeRedirects on a cross-host redirect = %v, want a cross-origin refusal", err)
	}
	if got := targetHits.Load(); got != 0 {
		t.Errorf("cross-host target hits = %d, want 0", got)
	}

	if err := get(httpx.RefuseAllRedirects, "/same-host"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("RefuseAllRedirects on a same-host redirect = %v, want a refusal", err)
	}
	if got := nextHits.Load(); got != 1 {
		t.Errorf("same-host target hits after RefuseAllRedirects = %d, want still 1", got)
	}
}
