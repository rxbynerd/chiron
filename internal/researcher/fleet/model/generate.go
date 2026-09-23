package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rxbynerd/chiron/internal/secret"
)

// Role identifies who authored a message in the chat transcript. Only the
// three OpenAI-compatible roles are modelled; the worker and lead compose
// their transcripts from these.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn in the chat transcript.
type Message struct {
	Role    Role
	Content string
}

// Request is a single generation call. Messages is the ordered transcript
// (typically a system prompt then the user/assistant exchange). MaxTokens,
// when positive, caps the completion length. Set JSONSchema (with
// SchemaName) to request provider-native structured output: the returned
// Response.Content is then a JSON string conforming to the schema, which
// the caller parses. Leave JSONSchema nil for plain text generation.
type Request struct {
	// Messages is the ordered chat transcript. At least one is required.
	Messages []Message
	// MaxTokens, when > 0, bounds the completion length, reasoning tokens
	// included (the wire max_completion_tokens field). Zero leaves it unset.
	MaxTokens int
	// JSONSchema, when set, requests provider-native structured output via
	// response_format {type: json_schema, strict: true}. It is the JSON
	// Schema object for the expected reply; Response.Content is the JSON
	// string the model returns. SchemaName must be set alongside it.
	JSONSchema json.RawMessage
	// SchemaName names the schema in the response_format payload (the
	// provider requires a name). Required when JSONSchema is set.
	SchemaName string
}

// Response is a single generation result. Content is the assistant reply —
// plain text, or a JSON string when the Request carried a JSONSchema.
// FinishReason is the provider's stop reason (e.g. "stop", "length").
type Response struct {
	// Content is choices[0].message.content: the assistant's reply text,
	// or a JSON string for a structured request.
	Content string
	// FinishReason is choices[0].finish_reason, surfaced so callers can
	// distinguish a completed answer ("stop") from a truncated one
	// ("length").
	FinishReason string
	// Usage carries the token counters the provider reported.
	Usage Usage
}

// Usage is the provider's token accounting. Callers convert this to
// internal/types.Usage and attach cost estimates; this package stays free
// of domain coupling. InputTokens maps prompt_tokens, OutputTokens maps
// completion_tokens.
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

// --- wire types: minimal OpenAI-compatible Chat Completions, unexported ---

// chatRequest sends max_completion_tokens rather than the deprecated
// max_tokens, which GPT-5 and o-series models reject.
type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	ResponseFormat      *responseFormat `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type       string         `json:"type"`
	JSONSchema jsonSchemaSpec `json:"json_schema"`
}

type jsonSchemaSpec struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Generate performs one Chat Completions call and returns the assistant
// reply plus usage. It makes EXACTLY ONE POST attempt: a model turn may
// already be billed after an ambiguous 5xx, so — unlike the idempotent GETs
// in internal/interactions — this paid create is never auto-retried.
// Failures surface to the caller, who retries knowingly.
//
// A JSONSchema on the request switches on provider-native structured
// output; the returned Response.Content is then the JSON string to parse.
func (c *Client) Generate(ctx context.Context, req Request) (Response, error) {
	if len(req.Messages) == 0 {
		return Response{}, fmt.Errorf("model: at least one message is required")
	}
	if req.JSONSchema != nil && req.SchemaName == "" {
		return Response{}, fmt.Errorf("model: a structured request requires a schema name")
	}

	body, err := json.Marshal(c.buildRequest(req))
	if err != nil {
		// Scrub defensively: a message could quote credential-shaped input.
		return Response{}, fmt.Errorf("model: encoding request: %s", c.scrub(err.Error()))
	}

	// Bound the call by RequestTimeout without overriding a tighter
	// caller-supplied deadline: context.WithTimeout keeps whichever fires
	// first.
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+chatCompletionsPath, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("model: building request: %s", c.scrub(err.Error()))
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	// Exactly one attempt — see the method contract. No retry loop.
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// The transport error can carry the request URL; the key is never
		// in the URL, but scrub regardless so no diagnostic can leak it.
		return Response{}, fmt.Errorf("model: POST %s: %s", chatCompletionsPath, c.scrub(err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Response{}, c.errorFromResponse(resp)
	}

	data, err := readBounded(resp.Body, c.maxBodyBytes)
	if err != nil {
		return Response{}, fmt.Errorf("model: reading response: %s", c.scrub(err.Error()))
	}

	var wire chatResponse
	if err := json.Unmarshal(data, &wire); err != nil {
		return Response{}, fmt.Errorf("model: decoding response: %s", c.scrub(err.Error()))
	}
	if len(wire.Choices) == 0 {
		return Response{}, fmt.Errorf("model: response carried no choices")
	}

	return Response{
		Content:      wire.Choices[0].Message.Content,
		FinishReason: wire.Choices[0].FinishReason,
		Usage: Usage{
			InputTokens:  wire.Usage.PromptTokens,
			OutputTokens: wire.Usage.CompletionTokens,
			TotalTokens:  wire.Usage.TotalTokens,
		},
	}, nil
}

// buildRequest maps the public Request onto the wire body. A JSONSchema
// switches on response_format with strict: true — provider-native
// structured output, not prompt-only coaxing.
func (c *Client) buildRequest(req Request) chatRequest {
	msgs := make([]chatMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = chatMessage{Role: string(m.Role), Content: m.Content}
	}
	out := chatRequest{
		Model:               c.model,
		Messages:            msgs,
		MaxCompletionTokens: req.MaxTokens,
	}
	if req.JSONSchema != nil {
		out.ResponseFormat = &responseFormat{
			Type: "json_schema",
			JSONSchema: jsonSchemaSpec{
				Name:   req.SchemaName,
				Schema: req.JSONSchema,
				Strict: true,
			},
		}
	}
	return out
}

// errorFromResponse builds an error from a non-2xx response, bounding the
// error-body read and scrubbing the body — a provider error payload could
// echo the submitted key back (a 401 body commonly does). The key is never
// in the body Chiron sends (it is header-only), but the provider's echo is
// outside Chiron's control, so the body is scrubbed unconditionally.
func (c *Client) errorFromResponse(resp *http.Response) error {
	data, err := readBounded(resp.Body, maxErrorBodyBytes)
	if err != nil {
		data = nil
	}
	detail := strings.TrimSpace(string(data))
	if detail == "" {
		return fmt.Errorf("model: request failed: HTTP %d", resp.StatusCode)
	}
	return fmt.Errorf("model: request failed: HTTP %d: %s", resp.StatusCode, c.scrub(detail))
}

// scrub redacts credentials from a diagnostic string. It first replaces the
// client's own key by exact match — a guaranteed redaction that does not
// depend on the key clearing secret.Scrub's entropy heuristics (an
// OpenAI-style "sk-..." key with structured segments may fall below the
// backstop's entropy bar) — then runs secret.Scrub to catch any other
// credential-shaped material. The empty check guards against an
// accidentally-empty apiKey redacting every empty substring.
func (c *Client) scrub(s string) string {
	if c.apiKey != "" {
		s = strings.ReplaceAll(s, c.apiKey, "[REDACTED:model-api-key]")
	}
	return secret.Scrub(s)
}

// readBounded reads at most max bytes, failing — rather than silently
// truncating — if the body is larger, so a misbehaving server cannot
// exhaust memory or smuggle a clipped document through as complete. This
// mirrors internal/interactions.readBounded.
func readBounded(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("body exceeds %d-byte bound", max)
	}
	return data, nil
}
