package model

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
)

// FakeServer is an httptest-backed stand-in for the Chat Completions
// endpoint, shared by this package's tests and the later worker/lead
// chunks (docs/V2-RESEARCH-AGENT §8 requires a fake model transport for
// CI). It scripts one reply per call, records the decoded requests it
// received, and never touches the real network. Callers own its lifecycle:
// build it at the call site and defer Close.
//
// The zero value is not usable; construct with NewFakeServer. Point a
// Client at it via Options{Endpoint: fake.URL(), ...}; the Client appends
// "/chat/completions", which the fake serves.
type FakeServer struct {
	server *httptest.Server

	mu       sync.Mutex
	replies  []FakeReply
	nextCall int
	requests []FakeRequest
}

// FakeReply scripts one response. Exactly one of the fields drives the
// reply: if Status is a non-2xx code it is returned with StatusBody as the
// body; otherwise a 200 is returned echoing Content, FinishReason and
// Usage as a Chat Completions body. RawBody, when set, is written verbatim
// (for malformed-body or oversized-body cases) and overrides the rest.
type FakeReply struct {
	// Content is the assistant reply text placed at
	// choices[0].message.content on a success reply.
	Content string
	// FinishReason is choices[0].finish_reason; defaults to "stop" when
	// empty on a success reply.
	FinishReason string
	// Usage is echoed into the response's usage block.
	Usage Usage
	// Status, when >= 400, makes the fake return that HTTP status with
	// StatusBody instead of a success reply.
	Status int
	// StatusBody is the body returned alongside a non-2xx Status.
	StatusBody string
	// RawBody, when non-empty, is written verbatim as a 200 response,
	// overriding Content/FinishReason/Usage — for oversized or malformed
	// body test cases.
	RawBody string
}

// FakeRequest is one decoded request the fake received, exposed so tests
// can assert on the wire body and the Authorization header the Client
// sent.
type FakeRequest struct {
	// Authorization is the raw Authorization header value.
	Authorization string
	// Model is the model field from the request body.
	Model string
	// Messages is the transcript from the request body.
	Messages []Message
	// MaxTokens is the max_tokens field (0 when unset).
	MaxTokens int
	// ResponseFormatType is response_format.type ("" when the request was
	// plain text, "json_schema" for a structured request).
	ResponseFormatType string
	// SchemaName is response_format.json_schema.name for a structured
	// request.
	SchemaName string
	// Schema is response_format.json_schema.schema for a structured
	// request.
	Schema json.RawMessage
	// Strict is response_format.json_schema.strict for a structured
	// request.
	Strict bool
}

// NewFakeServer starts a fake Chat Completions server that returns the
// given replies in order, one per call. If more calls arrive than there
// are replies, the fake responds 500 (so an unexpected retry is visible as
// a distinct failure, not a silent success). Call Close when done.
func NewFakeServer(replies ...FakeReply) *FakeServer {
	f := &FakeServer{replies: replies}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

// URL is the base URL to pass as Options.Endpoint. The Client POSTs to
// URL + "/chat/completions".
func (f *FakeServer) URL() string { return f.server.URL }

// Close shuts the server down. Callers should defer this at the call site.
func (f *FakeServer) Close() { f.server.Close() }

// Requests returns a copy of the decoded requests the fake has received,
// in arrival order.
func (f *FakeServer) Requests() []FakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// CallCount reports how many requests the fake has received. Tests assert
// this is exactly 1 after a failing call to prove the paid POST is not
// retried.
func (f *FakeServer) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *FakeServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	var body chatRequest
	// Decode best-effort: a malformed request body is not this fake's
	// concern (the Client always sends valid JSON), and recording an empty
	// FakeRequest is still enough for CallCount assertions.
	_ = json.NewDecoder(r.Body).Decode(&body)

	rec := FakeRequest{
		Authorization: r.Header.Get("Authorization"),
		Model:         body.Model,
		MaxTokens:     body.MaxTokens,
	}
	for _, m := range body.Messages {
		rec.Messages = append(rec.Messages, Message{Role: Role(m.Role), Content: m.Content})
	}
	if body.ResponseFormat != nil {
		rec.ResponseFormatType = body.ResponseFormat.Type
		rec.SchemaName = body.ResponseFormat.JSONSchema.Name
		rec.Schema = body.ResponseFormat.JSONSchema.Schema
		rec.Strict = body.ResponseFormat.JSONSchema.Strict
	}
	f.requests = append(f.requests, rec)

	call := f.nextCall
	f.nextCall++
	var reply FakeReply
	haveReply := call < len(f.replies)
	if haveReply {
		reply = f.replies[call]
	}
	f.mu.Unlock()

	if !haveReply {
		// More calls than scripted replies: surface as a server error so an
		// unexpected retry is a visible failure, not a silent success.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"fake: no scripted reply for this call"}`))
		return
	}

	switch {
	case reply.RawBody != "":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply.RawBody))
	case reply.Status >= 400:
		w.WriteHeader(reply.Status)
		_, _ = w.Write([]byte(reply.StatusBody))
	default:
		finish := reply.FinishReason
		if finish == "" {
			finish = "stop"
		}
		out := chatResponse{}
		out.Choices = append(out.Choices, struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		}{})
		out.Choices[0].Message.Content = reply.Content
		out.Choices[0].FinishReason = finish
		out.Usage.PromptTokens = reply.Usage.InputTokens
		out.Usage.CompletionTokens = reply.Usage.OutputTokens
		out.Usage.TotalTokens = reply.Usage.TotalTokens
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			// httptest can only fail here on a broken pipe; make it loud in
			// the test's server log rather than swallowing it.
			panic(fmt.Sprintf("fake: encoding reply: %v", err))
		}
	}
}
