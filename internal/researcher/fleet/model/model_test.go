package model

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "sk-test-0123456789abcdefABCDEF"

// newClient builds a Client pointed at a fake server, failing the test on a
// construction error.
func newClient(t *testing.T, endpoint string, mutate ...func(*Options)) *Client {
	t.Helper()
	opts := Options{Endpoint: endpoint, Model: "gpt-test", APIKey: testKey}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestGenerateSendsMaxCompletionTokens(t *testing.T) {
	// GPT-5 and o-series models reject max_tokens; the cap must ride
	// max_completion_tokens, and the legacy field must be absent.
	var body map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	c := newClient(t, server.URL)
	if _, err := c.Generate(context.Background(), Request{
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
		MaxTokens: 512,
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := string(body["max_completion_tokens"]); got != "512" {
		t.Errorf("max_completion_tokens = %q, want 512", got)
	}
	if _, present := body["max_tokens"]; present {
		t.Errorf("request carried the deprecated max_tokens field: %s", body["max_tokens"])
	}
}

func TestGenerateOmitsCompletionCapWhenUnset(t *testing.T) {
	var body map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	c := newClient(t, server.URL)
	if _, err := c.Generate(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, field := range []string{"max_completion_tokens", "max_tokens"} {
		if _, present := body[field]; present {
			t.Errorf("request carried %s with MaxTokens unset: %s", field, body[field])
		}
	}
}

func TestValidateStrictSchema(t *testing.T) {
	tests := []struct {
		name    string
		schema  string
		wantErr string
	}{
		{
			name: "compliant nested schema",
			schema: `{
				"type": "object",
				"additionalProperties": false,
				"properties": {
					"action": {"type": "string", "enum": ["search", "final"]},
					"query": {"type": ["string", "null"]},
					"citations": {
						"type": ["array", "null"],
						"items": {
							"type": "object",
							"additionalProperties": false,
							"properties": {"url": {"type": "string"}, "title": {"type": ["string", "null"]}},
							"required": ["url", "title"]
						}
					},
					"meta": {"anyOf": [{"$ref": "#/$defs/meta"}, {"type": "null"}]}
				},
				"required": ["action", "query", "citations", "meta"],
				"$defs": {
					"meta": {"type": "object", "additionalProperties": false, "properties": {"k": {"type": "string"}}, "required": ["k"]}
				}
			}`,
		},
		{
			name:    "missing required at the root",
			schema:  `{"type":"object","additionalProperties":false,"properties":{"action":{"type":"string"},"query":{"type":"string"}},"required":["action"]}`,
			wantErr: `strict schema object at the root: property "query" must be listed in required`,
		},
		{
			name:    "required absent entirely",
			schema:  `{"type":"object","additionalProperties":false,"properties":{"action":{"type":"string"}}}`,
			wantErr: `property "action" must be listed in required`,
		},
		{
			name:    "missing additionalProperties on a nested object",
			schema:  `{"type":"object","additionalProperties":false,"properties":{"citations":{"type":"array","items":{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}}},"required":["citations"]}`,
			wantErr: "strict schema object at /properties/citations/items: additionalProperties must be false",
		},
		{
			name:    "additionalProperties true",
			schema:  `{"type":"object","additionalProperties":true,"properties":{},"required":[]}`,
			wantErr: "at the root: additionalProperties must be false",
		},
		{
			name:    "open object inside anyOf",
			schema:  `{"type":"object","additionalProperties":false,"properties":{"x":{"anyOf":[{"type":"object","properties":{}},{"type":"null"}]}},"required":["x"]}`,
			wantErr: "at /properties/x/anyOf/0: additionalProperties must be false",
		},
		{
			name:    "non-compliant definition",
			schema:  `{"type":"object","additionalProperties":false,"properties":{},"required":[],"$defs":{"a/b":{"type":"object","additionalProperties":false,"properties":{"k":{"type":"string"}}}}}`,
			wantErr: `at /$defs/a~1b: property "k" must be listed in required`,
		},
		{
			name:    "not JSON",
			schema:  `{"type":`,
			wantErr: "not valid JSON",
		},
		{
			name:    "not an object",
			schema:  `["object"]`,
			wantErr: "must be a JSON object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStrictSchema(json.RawMessage(tt.schema))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateStrictSchema = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateStrictSchema = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.HasPrefix(err.Error(), "model: ") || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ValidateStrictSchema = %q, want a model: error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestCrossHostRedirectRefused(t *testing.T) {
	// A credential-bearing client must not follow a redirect to another
	// host — that would hand the Authorization header to the target.
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("redirect target was called — the key would have leaked to %s", r.Host)
		w.Write([]byte(`{"choices":[{"message":{"content":"leaked"}}]}`))
	}))
	defer elsewhere.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+chatCompletionsPath, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	c := newClient(t, redirector.URL)
	_, err := c.Generate(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Generate should fail rather than follow a cross-host redirect")
	}
	if !strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "cross-origin") {
		t.Errorf("error = %v, want a cross-host redirect refusal", err)
	}
}

func TestHTTPSDowngradeRedirectRefused(t *testing.T) {
	// net/http re-sends Authorization to the same hostname on any scheme, so
	// an https endpoint redirecting to http on the same host must be refused
	// before a second connection is opened.
	var hits, conns atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://"+r.Host+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()

	c := newClient(t, server.URL, func(o *Options) { o.HTTPClient = server.Client() })
	_, err := c.Generate(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Generate should fail rather than follow an https-to-http redirect")
	}
	if !strings.Contains(err.Error(), "downgrade") {
		t.Errorf("error = %v, want a downgrade refusal", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("handler hits = %d, want 1", got)
	}
	if got := conns.Load(); got != 1 {
		t.Errorf("connections = %d, want 1 — the cleartext redirect target must never be dialled", got)
	}
}

func TestEndpointValidation(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{"https ok", "https://api.openai.com/v1", false},
		{"http loopback ip ok", "http://127.0.0.1:8080/v1", false},
		{"http loopback ipv6 ok", "http://[::1]:8080/v1", false},
		{"http localhost ok", "http://localhost:8080/v1", false},
		{"http non-loopback rejected", "http://api.openai.com/v1", true},
		{"non-absolute rejected", "/v1/chat", true},
		{"empty rejected", "", true},
		{"garbage rejected", "://not a url", true},
		{"ftp scheme rejected", "ftp://example.com", true},
		{"query rejected", "https://api.openai.com/v1?api_key=x", true},
		{"empty query rejected", "https://api.openai.com/v1?", true},
		{"fragment rejected", "https://api.openai.com/v1#frag", true},
		{"empty fragment rejected", "https://api.openai.com/v1#", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(Options{Endpoint: tt.endpoint, Model: "gpt-test", APIKey: testKey})
			if tt.wantErr && err == nil {
				t.Errorf("New(%q) succeeded, want an error", tt.endpoint)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("New(%q) = %v, want success", tt.endpoint, err)
			}
			if err != nil && !strings.HasPrefix(err.Error(), "model: endpoint ") {
				t.Errorf("error = %v, want the model: endpoint prefix", err)
			}
		})
	}
}

func TestEndpointCredentialsRejectedWithoutEcho(t *testing.T) {
	// An endpoint carrying credentials is rejected, and the error never
	// repeats them, even when a typo stops url.Parse recognising userinfo.
	const (
		username = "alice"
		password = "Winter2026!"
	)
	tests := []struct {
		name     string
		endpoint string
		wantErr  string
	}{
		{"user and password", "https://" + username + ":" + password + "@api.example.com/v1", "userinfo"},
		{"key as username", "https://" + password + "@api.example.com/v1", "userinfo"},
		{"deceptive host", "https://api.openai.com:" + password + "@evil.example/v1", "userinfo"},
		{"loopback with userinfo", "http://" + username + ":" + password + "@127.0.0.1:8080/v1", "userinfo"},
		{"mistyped scheme", "htps://" + username + ":" + password + "@gateway.corp/v1", "userinfo"},
		{"single slash", "https:/" + username + ":" + password + "@gateway.corp/v1", "withheld"},
		{"unparseable", "://" + username + ":" + password + "@gateway.corp/v1", "withheld"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(Options{Endpoint: tt.endpoint, Model: "gpt-test", APIKey: testKey})
			if err == nil {
				t.Fatalf("New(%q) succeeded, want an error", tt.endpoint)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), username) {
				t.Errorf("error echoed the credentials: %v", err)
			}
		})
	}
}

func TestNewValidatesRequiredFields(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{"missing key", Options{Endpoint: "https://api.openai.com/v1", Model: "gpt-test"}},
		{"missing model", Options{Endpoint: "https://api.openai.com/v1", APIKey: testKey}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.opts); err == nil {
				t.Errorf("New(%+v) succeeded, want an error", tt.opts)
			}
		})
	}
}

func TestCallerContextDeadlineWins(t *testing.T) {
	// The caller's tighter deadline must beat the generous RequestTimeout. The
	// handler drains the body because net/http only cancels r.Context() on
	// client disconnect once it is read; the 2s backstop keeps Close unblocked.
	aborted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			close(aborted)
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()

	c := newClient(t, server.URL, func(o *Options) { o.RequestTimeout = 10 * time.Second })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Generate(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("Generate should fail when the caller deadline fires")
	}
	// The 100ms caller deadline must win decisively over the 10s
	// RequestTimeout (and comfortably under the handler's 2s backstop).
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("Generate took %s — the caller's 100ms deadline should have won over the 10s RequestTimeout", elapsed)
	}
	select {
	case <-aborted:
	case <-time.After(time.Second):
		t.Error("the server never observed the request being aborted")
	}
}
