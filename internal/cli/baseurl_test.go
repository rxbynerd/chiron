package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBaseURLOverrideRejectedBeforeAnyRequest pins C1-SEC-1: a hostile
// CHIRON_GEMINI_BASE_URL fails validation before any client is built,
// so the API key cannot be delivered to an attacker-controlled or
// internal endpoint.
func TestBaseURLOverrideRejectedBeforeAnyRequest(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"cleartext non-loopback", "http://evil.example.com"},
		{"ssrf to metadata service", "http://169.254.169.254"},
		{"non-http scheme", "ftp://example.com"},
		{"missing scheme", "evil.example.com"},
		{"bare string no host", "not-a-url"},
		{"https without host", "https://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CHIRON_GEMINI_BASE_URL", tc.url)
			t.Setenv("GEMINI_API_KEY", "test-key")
			_, _, err := execute(t, "research", "--query", "q", "-o", "none")
			if err == nil || !strings.Contains(err.Error(), "must be an absolute https:// URL") {
				t.Errorf("err = %v, want the base-URL validation error", err)
			}
		})
	}
}

// TestBaseURLOverrideAccepted: https anywhere and http on loopback are
// admitted — the loopback exemption is what lets the smoke tests run
// against httptest servers.
func TestBaseURLOverrideAccepted(t *testing.T) {
	for _, raw := range []string{
		"https://gemini-proxy.internal.example",
		"http://127.0.0.1:9999",
		"http://localhost:9999",
		"http://[::1]:9999",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("CHIRON_GEMINI_BASE_URL", raw)
			got, err := geminiBaseURL()
			if err != nil {
				t.Fatalf("geminiBaseURL(%q): %v", raw, err)
			}
			if got != raw {
				t.Errorf("geminiBaseURL(%q) = %q, want it passed through unchanged", raw, got)
			}
		})
	}
}

// TestBaseURLOverrideUsed: a valid loopback override is not just
// accepted but actually routes the run's requests (the full e2e suite
// rides on this too).
func TestBaseURLOverrideUsed(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"id":"v1_base","status":"in_progress"}`))
			return
		}
		w.Write([]byte(`{"id":"v1_base","status":"completed"}`))
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	if _, _, err := execute(t, "research", "--query", "q", "-o", "none"); err != nil {
		t.Fatalf("research against the override: %v", err)
	}
	if hits == 0 {
		t.Error("the override URL received no requests")
	}
}
