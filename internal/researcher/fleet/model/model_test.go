package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestGenerateText(t *testing.T) {
	fake := NewFakeServer(FakeReply{
		Content:      "The sky is blue because of Rayleigh scattering.",
		FinishReason: "stop",
		Usage:        Usage{InputTokens: 12, OutputTokens: 9, TotalTokens: 21},
	})
	defer fake.Close()

	c := newClient(t, fake.URL())
	resp, err := c.Generate(context.Background(), Request{
		Messages: []Message{
			{Role: RoleSystem, Content: "You are a concise researcher."},
			{Role: RoleUser, Content: "Why is the sky blue?"},
		},
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if resp.Content != "The sky is blue because of Rayleigh scattering." {
		t.Errorf("Content = %q, want the scripted reply", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", resp.FinishReason)
	}
	if resp.Usage != (Usage{InputTokens: 12, OutputTokens: 9, TotalTokens: 21}) {
		t.Errorf("Usage = %+v, want prompt→input=12 completion→output=9 total=21", resp.Usage)
	}

	// The request the fake saw carries the transcript, model, completion cap
	// and no response_format (plain text), and the key rides only the
	// Authorization header.
	reqs := fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("call count = %d, want 1", len(reqs))
	}
	got := reqs[0]
	if got.Model != "gpt-test" {
		t.Errorf("model = %q, want gpt-test", got.Model)
	}
	if got.MaxTokens != 256 {
		t.Errorf("max_completion_tokens = %d, want 256", got.MaxTokens)
	}
	if got.ResponseFormatType != "" {
		t.Errorf("response_format present on a text call: %q", got.ResponseFormatType)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != RoleSystem || got.Messages[1].Content != "Why is the sky blue?" {
		t.Errorf("messages = %+v, want the two-message transcript", got.Messages)
	}
	if got.Authorization != "Bearer "+testKey {
		t.Errorf("Authorization = %q, want the bearer key header", got.Authorization)
	}
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

func TestGenerateStructured(t *testing.T) {
	// The model returns a JSON string as the content for a structured call;
	// the client returns it verbatim for the caller to parse.
	fake := NewFakeServer(FakeReply{
		Content: `{"subtasks":["a","b","c"]}`,
		Usage:   Usage{InputTokens: 30, OutputTokens: 15, TotalTokens: 45},
	})
	defer fake.Close()

	schema := json.RawMessage(`{"type":"object","properties":{"subtasks":{"type":"array","items":{"type":"string"}}},"required":["subtasks"]}`)
	c := newClient(t, fake.URL())
	resp, err := c.Generate(context.Background(), Request{
		Messages:   []Message{{Role: RoleUser, Content: "Decompose: why is the sky blue?"}},
		JSONSchema: schema,
		SchemaName: "decomposition",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Content is the raw JSON string; it must parse.
	var parsed struct {
		Subtasks []string `json:"subtasks"`
	}
	if err := json.Unmarshal([]byte(resp.Content), &parsed); err != nil {
		t.Fatalf("structured content did not parse: %v (content %q)", err, resp.Content)
	}
	if len(parsed.Subtasks) != 3 {
		t.Errorf("subtasks = %v, want three", parsed.Subtasks)
	}

	// The fake saw a provider-native response_format, strict, with the
	// schema echoed — not prompt-only coaxing.
	got := fake.Requests()[0]
	if got.ResponseFormatType != "json_schema" {
		t.Errorf("response_format.type = %q, want json_schema", got.ResponseFormatType)
	}
	if got.SchemaName != "decomposition" {
		t.Errorf("schema name = %q, want decomposition", got.SchemaName)
	}
	if !got.Strict {
		t.Errorf("strict = false, want true")
	}
	if string(got.Schema) != string(schema) {
		t.Errorf("schema = %s, want the request schema echoed", got.Schema)
	}
}

func TestStructuredRequiresSchemaName(t *testing.T) {
	fake := NewFakeServer()
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Generate(context.Background(), Request{
		Messages:   []Message{{Role: RoleUser, Content: "hi"}},
		JSONSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err == nil {
		t.Fatal("Generate with a schema but no name should fail")
	}
	// It must fail before touching the wire — no paid call.
	if fake.CallCount() != 0 {
		t.Errorf("call count = %d, want 0 — validation must precede the POST", fake.CallCount())
	}
}

func TestNoRetryOn5xx(t *testing.T) {
	// A 5xx may already have billed a turn; the paid POST is single-attempt.
	fake := NewFakeServer(FakeReply{Status: http.StatusInternalServerError, StatusBody: `{"error":"boom"}`})
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Generate(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Generate should fail on a 5xx")
	}
	if fake.CallCount() != 1 {
		t.Errorf("call count = %d, want exactly 1 — the paid POST must not be retried", fake.CallCount())
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	// A body over the bound is an error, not a truncation-to-success.
	big := strings.Repeat("x", 2048)
	fake := NewFakeServer(FakeReply{RawBody: fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, big)})
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *Options) { o.MaxBodyBytes = 512 })
	_, err := c.Generate(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Generate should fail when the body exceeds MaxBodyBytes")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want a body-bound error", err)
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

func TestErrorNeverLeaksKey(t *testing.T) {
	// A failing request must not surface the API key anywhere in the error,
	// even when the provider echoes request context in its error body.
	fake := NewFakeServer(FakeReply{
		Status:     http.StatusUnauthorized,
		StatusBody: `{"error":"invalid key ` + testKey + `"}`,
	})
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Generate(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Generate should fail on a 401")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("error leaked the API key: %v", err)
	}
}

func TestCallerContextDeadlineWins(t *testing.T) {
	// A tighter caller-supplied deadline must fire even though the client's
	// RequestTimeout is generous — WithTimeout keeps whichever is sooner.
	// The handler either observes the request being cancelled (the client
	// aborted) or, as a backstop, returns after a bounded sleep — so the
	// server can always Close without deadlocking on a stuck handler.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
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
}

func TestEmptyMessagesRejected(t *testing.T) {
	fake := NewFakeServer()
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.Generate(context.Background(), Request{}); err == nil {
		t.Fatal("Generate with no messages should fail")
	}
	if fake.CallCount() != 0 {
		t.Errorf("call count = %d, want 0 — validation must precede the POST", fake.CallCount())
	}
}
