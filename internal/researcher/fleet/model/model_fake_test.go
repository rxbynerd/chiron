package model_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
)

const testKey = "sk-test-0123456789abcdefABCDEF"

// newClient builds a Client pointed at a fake server, failing the test on a
// construction error.
func newClient(t *testing.T, endpoint string, mutate ...func(*model.Options)) *model.Client {
	t.Helper()
	must := require.New(t)
	opts := model.Options{Endpoint: endpoint, Model: "gpt-test", APIKey: testKey}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := model.New(opts)
	must.NoError(err, "model.New")
	return c
}

func TestGenerate_PlainText_MapsReplyAndWireRequest(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

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
	must.NoError(err)

	check.Equal("The sky is blue because of Rayleigh scattering.", resp.Content)
	check.Equal("stop", resp.FinishReason)
	check.Equal(model.Usage{InputTokens: 12, OutputTokens: 9, TotalTokens: 21}, resp.Usage)

	// The request the fake saw carries the transcript, model, completion cap
	// and no response_format (plain text), and the key rides only the
	// Authorization header.
	reqs := fake.Requests()
	must.Len(reqs, 1)
	got := reqs[0]
	check.Equal("gpt-test", got.Model)
	check.Equal(256, got.MaxTokens)
	check.Empty(got.ResponseFormatType, "response_format must be absent on a text call")
	if check.Len(got.Messages, 2) {
		check.Equal(model.RoleSystem, got.Messages[0].Role)
		check.Equal("Why is the sky blue?", got.Messages[1].Content)
	}
	check.Equal("Bearer "+testKey, got.Authorization)
}

func TestGenerate_Structured_ReturnsRawJSONAndSendsStrictSchema(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

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
	must.NoError(err)

	// Content is the raw JSON string; it must parse.
	var parsed struct {
		Subtasks []string `json:"subtasks"`
	}
	must.NoError(json.Unmarshal([]byte(resp.Content), &parsed), "content %q must parse", resp.Content)
	check.Len(parsed.Subtasks, 3)

	// The fake saw a provider-native response_format, strict, with the
	// schema echoed — not prompt-only coaxing.
	got := fake.Requests()[0]
	check.Equal("json_schema", got.ResponseFormatType)
	check.Equal("decomposition", got.SchemaName)
	check.True(got.Strict, "response_format.json_schema.strict must be set")
	check.Equal(string(schema), string(got.Schema))
}

func TestGenerate_NonStrictSchema_RejectedWithoutConsumingReply(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

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
	must.Error(err)
	for _, want := range []string{"HTTP 400", "invalid_request_error", "Invalid schema for response_format 'reply'", `property \"note\" must be listed in required`} {
		must.ErrorContains(err, want)
	}
	check.Equal(1, fake.CallCount())

	resp, err := c.Generate(context.Background(), model.Request{
		Messages:   []model.Message{{Role: model.RoleUser, Content: "hi"}},
		JSONSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"answer":{"type":"string"}},"required":["answer"]}`),
		SchemaName: "reply",
	})
	must.NoError(err)
	must.JSONEq(`{"answer":"42"}`, resp.Content, "the rejected call must not consume the scripted reply")
}

func TestGenerate_SchemaWithoutName_RejectedBeforePost(t *testing.T) {
	must := require.New(t)

	fake := modeltest.NewFakeServer()
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Generate(context.Background(), model.Request{
		Messages:   []model.Message{{Role: model.RoleUser, Content: "hi"}},
		JSONSchema: json.RawMessage(`{"type":"object"}`),
	})
	must.Error(err)
	// It must fail before touching the wire — no paid call.
	must.Equal(0, fake.CallCount(), "validation must precede the POST")
}

func TestNoRetryOn5xx(t *testing.T) {
	must := require.New(t)

	// A 5xx may already have billed a turn; the paid POST is single-attempt.
	fake := modeltest.NewFakeServer(modeltest.FakeReply{Status: http.StatusInternalServerError, StatusBody: `{"error":"boom"}`})
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Generate(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
	})
	must.Error(err)
	must.Equal(1, fake.CallCount(), "the paid POST must not be retried")
}

func TestGenerate_OversizedBody_IsRejected(t *testing.T) {
	must := require.New(t)

	// A body over the bound is an error, not a truncation-to-success.
	big := strings.Repeat("x", 2048)
	fake := modeltest.NewFakeServer(modeltest.FakeReply{RawBody: fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, big)})
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *model.Options) { o.MaxBodyBytes = 512 })
	_, err := c.Generate(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
	})
	must.ErrorContains(err, "exceeds")
}

// A negative token count in the provider's usage is reported as zero, so it
// cannot offset spend elsewhere, while a non-negative count beside it is kept.
func TestGenerate_NegativeUsage_ClampedToZero(t *testing.T) {
	must := require.New(t)

	fake := modeltest.NewFakeServer(modeltest.FakeReply{
		Content:      "ok",
		FinishReason: "stop",
		Usage:        model.Usage{InputTokens: -5_000_000, OutputTokens: 7, TotalTokens: -4_999_993},
	})
	defer fake.Close()

	c := newClient(t, fake.URL())
	resp, err := c.Generate(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
	})
	must.NoError(err)
	must.Equal(model.Usage{InputTokens: 0, OutputTokens: 7, TotalTokens: 0}, resp.Usage)
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
			must := require.New(t)

			fake := modeltest.NewFakeServer(modeltest.FakeReply{RawBody: tt.body})
			defer fake.Close()

			c := newClient(t, fake.URL())
			_, err := c.Generate(context.Background(), model.Request{
				Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
			})
			must.ErrorContains(err, tt.want)
		})
	}
}

func TestErrorNeverLeaksKey(t *testing.T) {
	must := require.New(t)

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
	must.Error(err)
	must.NotContains(err.Error(), testKey, "error leaked the API key")
}

func TestGenerate_EmptyMessages_RejectedBeforePost(t *testing.T) {
	must := require.New(t)

	fake := modeltest.NewFakeServer()
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Generate(context.Background(), model.Request{})
	must.Error(err)
	must.Equal(0, fake.CallCount(), "validation must precede the POST")
}
