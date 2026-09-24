package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"subtasks":{"type":"array","items":{"type":"string"}}},"required":["subtasks"]}`)
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

func TestFakeRejectsNonStrictSchema(t *testing.T) {
	// A strict request whose schema the provider would refuse fails with the
	// provider's 400 shape, and the scripted reply stays queued for the next
	// compliant call.
	fake := NewFakeServer(FakeReply{Content: `{"answer":"42"}`})
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Generate(context.Background(), Request{
		Messages:   []Message{{Role: RoleUser, Content: "hi"}},
		JSONSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"answer":{"type":"string"},"note":{"type":"string"}},"required":["answer"]}`),
		SchemaName: "reply",
	})
	if err == nil {
		t.Fatal("Generate with a non-strict schema should fail against the fake")
	}
	for _, want := range []string{"HTTP 400", "invalid_request_error", "Invalid schema for response_format 'reply'", `property \"note\" must be listed in required`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if fake.CallCount() != 1 {
		t.Errorf("call count = %d, want 1", fake.CallCount())
	}

	resp, err := c.Generate(context.Background(), Request{
		Messages:   []Message{{Role: RoleUser, Content: "hi"}},
		JSONSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"answer":{"type":"string"}},"required":["answer"]}`),
		SchemaName: "reply",
	})
	if err != nil {
		t.Fatalf("Generate with a strict schema: %v", err)
	}
	if resp.Content != `{"answer":"42"}` {
		t.Errorf("Content = %q, want the scripted reply the rejected call did not consume", resp.Content)
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

// TestGenerateMalformedBodyIsError: a 200 whose body is not a Chat Completions
// object is a decode error naming the problem, never an empty success.
func TestGenerateMalformedBodyIsError(t *testing.T) {
	for _, tt := range []struct {
		name, body, want string
	}{
		{"not json", "<html>upstream proxy error</html>", "decoding response"},
		{"no choices", `{"id":"x","choices":[]}`, "no choices"},
		{"wrong shape", `{"choices":"nope"}`, "decoding response"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := NewFakeServer(FakeReply{RawBody: tt.body})
			defer fake.Close()

			c := newClient(t, fake.URL())
			_, err := c.Generate(context.Background(), Request{
				Messages: []Message{{Role: RoleUser, Content: "hi"}},
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Generate = %v, want an error containing %q", err, tt.want)
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

func TestRedirectPolicy(t *testing.T) {
	request := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", raw, err)
		}
		return &http.Request{URL: u}
	}
	tests := []struct {
		name    string
		via     []string
		target  string
		wantErr string
	}{
		{"same-host https path change followed", []string{"https://api.example.com/v1/chat/completions"}, "https://api.example.com/v2/chat/completions", ""},
		{"same-host loopback http followed", []string{"http://127.0.0.1:8080/v1"}, "http://127.0.0.1:8080/v2", ""},
		{"same-host upgrade to https followed", []string{"http://127.0.0.1:8080/v1"}, "https://127.0.0.1:8080/v1", ""},
		{"cross-host refused", []string{"https://api.example.com/v1"}, "https://evil.example/v1", "cross-origin"},
		{"https to http on the same host refused", []string{"https://api.example.com/v1"}, "http://api.example.com/v1", "downgrade"},
		{"downgrade on a later hop refused", []string{"https://api.example.com/v1", "https://api.example.com/v2"}, "http://api.example.com/v3", "downgrade"},
		{"third hop refused", []string{"https://api.example.com/a", "https://api.example.com/b", "https://api.example.com/c"}, "https://api.example.com/d", "too many redirects"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var via []*http.Request
			for _, v := range tt.via {
				via = append(via, request(v))
			}
			err := refuseUnsafeRedirects(request(tt.target), via)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("refuseUnsafeRedirects = %v, want the redirect followed", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("refuseUnsafeRedirects = %v, want an error containing %q", err, tt.wantErr)
			}
		})
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
