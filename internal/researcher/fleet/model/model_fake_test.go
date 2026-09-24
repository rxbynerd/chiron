package model_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
)

const testKey = "sk-test-0123456789abcdefABCDEF"

// newClient builds a Client pointed at a fake server, failing the test on a
// construction error.
func newClient(t *testing.T, endpoint string, mutate ...func(*model.Options)) *model.Client {
	t.Helper()
	opts := model.Options{Endpoint: endpoint, Model: "gpt-test", APIKey: testKey}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := model.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestGenerateText(t *testing.T) {
	fake := modeltest.NewFakeServer(modeltest.FakeReply{
		Content:      "The sky is blue because of Rayleigh scattering.",
		FinishReason: "stop",
		Usage:        model.Usage{InputTokens: 12, OutputTokens: 9, TotalTokens: 21},
	})
	defer fake.Close()

	c := newClient(t, fake.URL())
	resp, err := c.Generate(context.Background(), model.Request{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "You are a concise researcher."},
			{Role: model.RoleUser, Content: "Why is the sky blue?"},
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
	if resp.Usage != (model.Usage{InputTokens: 12, OutputTokens: 9, TotalTokens: 21}) {
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
	if len(got.Messages) != 2 || got.Messages[0].Role != model.RoleSystem || got.Messages[1].Content != "Why is the sky blue?" {
		t.Errorf("messages = %+v, want the two-message transcript", got.Messages)
	}
	if got.Authorization != "Bearer "+testKey {
		t.Errorf("Authorization = %q, want the bearer key header", got.Authorization)
	}
}

func TestGenerateStructured(t *testing.T) {
	// The model returns a JSON string as the content for a structured call;
	// the client returns it verbatim for the caller to parse.
	fake := modeltest.NewFakeServer(modeltest.FakeReply{
		Content: `{"subtasks":["a","b","c"]}`,
		Usage:   model.Usage{InputTokens: 30, OutputTokens: 15, TotalTokens: 45},
	})
	defer fake.Close()

	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"subtasks":{"type":"array","items":{"type":"string"}}},"required":["subtasks"]}`)
	c := newClient(t, fake.URL())
	resp, err := c.Generate(context.Background(), model.Request{
		Messages:   []model.Message{{Role: model.RoleUser, Content: "Decompose: why is the sky blue?"}},
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
	fake := modeltest.NewFakeServer(modeltest.FakeReply{Content: `{"answer":"42"}`})
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Generate(context.Background(), model.Request{
		Messages:   []model.Message{{Role: model.RoleUser, Content: "hi"}},
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

	resp, err := c.Generate(context.Background(), model.Request{
		Messages:   []model.Message{{Role: model.RoleUser, Content: "hi"}},
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

func TestStructuredRequiresSchemaName(t *testing.T) {
	fake := modeltest.NewFakeServer()
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Generate(context.Background(), model.Request{
		Messages:   []model.Message{{Role: model.RoleUser, Content: "hi"}},
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
	fake := modeltest.NewFakeServer(modeltest.FakeReply{Status: http.StatusInternalServerError, StatusBody: `{"error":"boom"}`})
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Generate(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
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
	fake := modeltest.NewFakeServer(modeltest.FakeReply{RawBody: fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, big)})
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *model.Options) { o.MaxBodyBytes = 512 })
	_, err := c.Generate(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
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
			fake := modeltest.NewFakeServer(modeltest.FakeReply{RawBody: tt.body})
			defer fake.Close()

			c := newClient(t, fake.URL())
			_, err := c.Generate(context.Background(), model.Request{
				Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Generate = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestErrorNeverLeaksKey(t *testing.T) {
	// A failing request must not surface the API key anywhere in the error,
	// even when the provider echoes request context in its error body.
	fake := modeltest.NewFakeServer(modeltest.FakeReply{
		Status:     http.StatusUnauthorized,
		StatusBody: `{"error":"invalid key ` + testKey + `"}`,
	})
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Generate(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Generate should fail on a 401")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("error leaked the API key: %v", err)
	}
}

func TestEmptyMessagesRejected(t *testing.T) {
	fake := modeltest.NewFakeServer()
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.Generate(context.Background(), model.Request{}); err == nil {
		t.Fatal("Generate with no messages should fail")
	}
	if fake.CallCount() != 0 {
		t.Errorf("call count = %d, want 0 — validation must precede the POST", fake.CallCount())
	}
}
