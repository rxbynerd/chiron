package interactions

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// sseClient wires a Client to a server that answers every request with
// the given raw SSE body, optionally capturing the request query.
func sseClient(t *testing.T, body string, gotQuery *url.Values, opts ...Option) *Client {
	t.Helper()
	return newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotQuery != nil {
			*gotQuery = r.URL.Query()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}), opts...)
}

func mustNext(t *testing.T, s *Stream) *Event {
	t.Helper()
	ev, err := s.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return ev
}

const eventScript = `: keep-alive

event: interaction.created
id: ev_1
data: {"id":"v1_abc","object":"interaction","status":"in_progress"}

event: step.delta
id: ev_2
data: {"index":1,"delta":{"type":"text",
data: "text":"Hello"}}

event: step.delta
id: ev_3
data: {"index":1,"delta":{"type":"thought_summary_delta","text":"Reading sources"}}

event: step.delta
id: ev_4
data: {"index":2,"delta":{"type":"image","mime_type":"image/png","data":"cG5nLWJ5dGVz"}}

event: interaction.status_update
id: ev_5
data: {"interaction_id":"v1_abc","status":"completed"}

event: interaction.completed
id: ev_6
data: {"interaction":{"id":"v1_abc","status":"completed"}}

`

func TestStreamEventSequence(t *testing.T) {
	tests := []struct {
		name   string
		script string
	}{
		{"lf", eventScript},
		{"crlf", strings.ReplaceAll(eventScript, "\n", "\r\n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := sseClient(t, tt.script, nil)
			s, err := c.Stream(context.Background(), "v1_abc", "")
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			defer s.Close()

			// interaction.created: payload is the resource itself.
			ev := mustNext(t, s)
			if ev.Type != EventInteractionCreated || ev.ID != "ev_1" {
				t.Fatalf("event 1 = %+v", ev)
			}
			if ev.Interaction == nil || ev.Interaction.ID != "v1_abc" || ev.Interaction.Status != StatusInProgress {
				t.Errorf("created interaction = %+v", ev.Interaction)
			}

			// step.delta: text, with multi-line data joined by \n.
			ev = mustNext(t, s)
			if ev.Type != EventStepDelta || ev.ID != "ev_2" || ev.Index != 1 {
				t.Fatalf("event 2 = %+v", ev)
			}
			if ev.Delta == nil || ev.Delta.Type != DeltaText || ev.Delta.Text != "Hello" {
				t.Errorf("text delta = %+v", ev.Delta)
			}

			// step.delta: thought summary.
			ev = mustNext(t, s)
			if ev.Delta == nil || ev.Delta.Type != DeltaThoughtSummary || ev.Delta.Text != "Reading sources" {
				t.Errorf("thought delta = %+v", ev.Delta)
			}

			// step.delta: image with base64 data.
			ev = mustNext(t, s)
			if ev.Delta == nil || ev.Delta.Type != DeltaImage || ev.Delta.MIMEType != "image/png" ||
				string(ev.Delta.Data) != "png-bytes" {
				t.Errorf("image delta = %+v", ev.Delta)
			}
			if ev.Index != 2 {
				t.Errorf("image delta index = %d, want 2", ev.Index)
			}

			// interaction.status_update.
			ev = mustNext(t, s)
			if ev.Type != EventInteractionStatusUpdate || ev.InteractionID != "v1_abc" || ev.Status != StatusCompleted {
				t.Errorf("status update = %+v", ev)
			}

			// interaction.completed: payload nested under "interaction".
			ev = mustNext(t, s)
			if ev.Type != EventInteractionCompleted || ev.ID != "ev_6" {
				t.Fatalf("event 6 = %+v", ev)
			}
			if ev.Interaction == nil || ev.Interaction.Status != StatusCompleted {
				t.Errorf("completed interaction = %+v", ev.Interaction)
			}
			if s.LastEventID() != "ev_6" {
				t.Errorf("LastEventID = %q, want ev_6", s.LastEventID())
			}

			// Clean end of stream.
			if _, err := s.Next(); !errors.Is(err, io.EOF) {
				t.Errorf("after last event Next() = %v, want io.EOF", err)
			}
		})
	}
}

func TestStreamResumeQuery(t *testing.T) {
	tests := []struct {
		name        string
		lastEventID string
		wantResume  bool
	}{
		{"attach from start", "", false},
		{"resume after drop", "ev_2", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got url.Values
			c := sseClient(t, "", &got)
			s, err := c.Stream(context.Background(), "v1_abc", tt.lastEventID)
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			defer s.Close()
			if got.Get("stream") != "true" {
				t.Errorf("stream query = %q, want true", got.Get("stream"))
			}
			if tt.wantResume {
				if got.Get("last_event_id") != tt.lastEventID {
					t.Errorf("last_event_id = %q, want %q", got.Get("last_event_id"), tt.lastEventID)
				}
				if s.LastEventID() != tt.lastEventID {
					t.Errorf("LastEventID = %q before any event, want resume value", s.LastEventID())
				}
			} else if got.Has("last_event_id") {
				t.Errorf("last_event_id sent on fresh attach: %v", got)
			}
		})
	}
}

func TestStreamErrorEvent(t *testing.T) {
	c := sseClient(t, `event: error
id: ev_9
data: {"error":{"code":"https://errors.example/internal","message":"boom"}}

`, nil)
	s, err := c.Stream(context.Background(), "v1_abc", "")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	ev := mustNext(t, s)
	if ev.Type != EventError || ev.Err == nil {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Err.Code != "https://errors.example/internal" || ev.Err.Message != "boom" {
		t.Errorf("Err = %+v", ev.Err)
	}
}

func TestStreamEnvelopeFieldsInJSON(t *testing.T) {
	// No SSE event/id lines: event_type and event_id travel in the data
	// payload instead, and must still be honoured for resume.
	c := sseClient(t, `data: {"event_type":"interaction.status_update","event_id":"ev_7","interaction_id":"v1_abc","status":"failed"}

`, nil)
	s, err := c.Stream(context.Background(), "v1_abc", "")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	ev := mustNext(t, s)
	if ev.Type != EventInteractionStatusUpdate || ev.ID != "ev_7" || ev.Status != StatusFailed {
		t.Errorf("event = %+v", ev)
	}
	if s.LastEventID() != "ev_7" {
		t.Errorf("LastEventID = %q, want ev_7", s.LastEventID())
	}
}

func TestStreamIDPersistsAcrossFrames(t *testing.T) {
	c := sseClient(t, `id: ev_1
event: interaction.status_update
data: {"interaction_id":"v1_abc","status":"in_progress"}

event: step.delta
data: {"index":0,"delta":{"type":"text","text":"hi"}}

`, nil)
	s, err := c.Stream(context.Background(), "v1_abc", "")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	mustNext(t, s)
	ev := mustNext(t, s)
	if ev.ID != "ev_1" {
		t.Errorf("id-less frame event ID = %q, want persisted ev_1", ev.ID)
	}
}

func TestStreamUnknownEventType(t *testing.T) {
	c := sseClient(t, `event: step.tool_call
id: ev_2
data: {"index":0,"tool":"google_search"}

`, nil)
	s, err := c.Stream(context.Background(), "v1_abc", "")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	ev := mustNext(t, s)
	if ev.Type != EventType("step.tool_call") || ev.ID != "ev_2" {
		t.Errorf("event = %+v", ev)
	}
	if string(ev.Raw) != `{"index":0,"tool":"google_search"}` {
		t.Errorf("Raw = %s", ev.Raw)
	}
}

func TestStreamDiscardsTruncatedFinalEvent(t *testing.T) {
	// The final frame has no terminating blank line: per the SSE spec it
	// is discarded, and the stream ends with io.EOF.
	c := sseClient(t, `event: step.delta
id: ev_1
data: {"index":0,"delta":{"type":"text","text":"hi"}}

event: step.delta
id: ev_2
data: {"index":0,"delta":{"type":"text","text":"trunc`, nil)
	s, err := c.Stream(context.Background(), "v1_abc", "")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	mustNext(t, s)
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next() = %v, want io.EOF for truncated frame", err)
	}
}

func TestStreamMalformedJSON(t *testing.T) {
	c := sseClient(t, `event: step.delta
id: ev_1
data: {not json

`, nil)
	s, err := c.Stream(context.Background(), "v1_abc", "")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	if _, err := s.Next(); err == nil || !strings.Contains(err.Error(), "decoding") {
		t.Errorf("Next() = %v, want decode error", err)
	}
}

func TestStreamAttachHTTPError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"https://errors.example/not-found","message":"gone"}}`)
	}))
	_, err := c.Stream(context.Background(), "v1_gone", "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusNotFound {
		t.Errorf("error = %v, want APIError 404", err)
	}
}

func TestStreamAttachRetriesTransient(t *testing.T) {
	var attempts atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: ev_1\ndata: {\"event_type\":\"interaction.created\",\"id\":\"v1_abc\"}\n\n")
	}))
	s, err := c.Stream(context.Background(), "v1_abc", "")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

func TestStreamWrongContentType(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, sampleInteraction)
	}))
	_, err := c.Stream(context.Background(), "v1_abc", "")
	if err == nil || !strings.Contains(err.Error(), "content type") {
		t.Errorf("error = %v, want content-type complaint", err)
	}
}

func TestStreamEventTooLarge(t *testing.T) {
	c := sseClient(t, "data: {\"pad\":\""+strings.Repeat("x", 1024)+"\"}\n\n", nil,
		WithMaxEventBytes(128))
	s, err := c.Stream(context.Background(), "v1_abc", "")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	if _, err := s.Next(); !errors.Is(err, bufio.ErrTooLong) {
		t.Errorf("Next() = %v, want bufio.ErrTooLong", err)
	}
}
