package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBaseURLOverrideRejectedBeforeAnyRequest: a hostile
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
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			t.Setenv("CHIRON_GEMINI_BASE_URL", tt.url)
			t.Setenv("GEMINI_API_KEY", "test-key")
			_, _, err := execute(t, "research", "--query", "q", "-o", "none")
			must.ErrorContains(err, "must be an absolute https:// URL")
		})
	}
}

// TestBaseURLOverride_RefusesCredentialCarriers: userinfo, a query and a
// fragment are refused like any other unsafe override, and the error names
// the value by scheme and host only.
func TestBaseURLOverride_RefusesCredentialCarriers(t *testing.T) {
	const secret = "Winter2026-credential"
	cases := []struct {
		name    string
		url     string
		wantErr string
	}{
		{"userinfo", "https://user:" + secret + "@gemini.example.com", "userinfo"},
		{"query", "https://gemini.example.com/?key=" + secret, "query or fragment"},
		{"empty query on loopback", "http://127.0.0.1:9999/?", "query or fragment"},
		{"fragment", "https://gemini.example.com/#" + secret, "query or fragment"},
		{"cleartext with a secret path", "http://evil.example.com/" + secret, "must be an absolute https:// URL"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			t.Setenv("CHIRON_GEMINI_BASE_URL", tt.url)
			_, err := geminiBaseURL()
			must.ErrorContains(err, tt.wantErr)
			must.True(strings.HasPrefix(err.Error(), "CHIRON_GEMINI_BASE_URL "), "error should name CHIRON_GEMINI_BASE_URL first: %v", err)
			must.NotContains(err.Error(), secret)
			must.NotContains(err.Error(), tt.url)
		})
	}
}

// TestBaseURLOverrideAccepted: https anywhere and http on loopback are
// admitted. The loopback exemption is what lets the smoke tests run
// against httptest servers.
func TestBaseURLOverrideAccepted(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"https host", "https://gemini-proxy.internal.example"},
		{"ipv4 loopback", "http://127.0.0.1:9999"},
		{"localhost", "http://localhost:9999"},
		{"ipv6 loopback", "http://[::1]:9999"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			t.Setenv("CHIRON_GEMINI_BASE_URL", tt.url)
			got, err := geminiBaseURL()
			must.NoError(err)
			must.Equal(tt.url, got)
		})
	}
}

// TestBaseURLOverrideUsed: a valid loopback override is not just
// accepted but actually routes the run's requests. The end-to-end tests
// depend on this too.
func TestBaseURLOverrideUsed(t *testing.T) {
	must := require.New(t)
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"id":"v1_base","status":"in_progress"}`))
			return
		}
		if r.URL.Query().Get("stream") == "true" {
			sseStatus(w, "v1_base", "completed")
			return
		}
		w.Write([]byte(`{"id":"v1_base","status":"completed"}`))
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	_, _, err := execute(t, "research", "--query", "q", "-o", "none")
	must.NoError(err, "research against the override")
	must.Positive(hits, "the override URL received no requests")
}
