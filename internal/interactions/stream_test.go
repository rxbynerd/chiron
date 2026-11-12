package interactions

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	must := require.New(t)
	ev, err := s.Next()
	must.NoError(err)
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
			must := require.New(t)
			c := sseClient(t, tt.script, nil)
			s, err := c.Stream(context.Background(), "v1_abc", "")
			must.NoError(err)
			defer s.Close()

			// interaction.created: payload is the resource itself.
			ev := mustNext(t, s)
			must.Equal(EventInteractionCreated, ev.Type, "event 1 = %+v", ev)
			must.Equal("ev_1", ev.ID, "event 1 = %+v", ev)
			must.NotNil(ev.Interaction)
			must.Equal("v1_abc", ev.Interaction.ID)
			must.Equal(StatusInProgress, ev.Interaction.Status)

			// step.delta: text, with multi-line data joined by \n.
			ev = mustNext(t, s)
			must.Equal(EventStepDelta, ev.Type, "event 2 = %+v", ev)
			must.Equal("ev_2", ev.ID, "event 2 = %+v", ev)
			must.Equal(1, ev.Index)
			must.NotNil(ev.Delta)
			must.Equal(DeltaText, ev.Delta.Type)
			must.Equal("Hello", ev.Delta.Text)

			// step.delta: thought summary.
			ev = mustNext(t, s)
			must.NotNil(ev.Delta)
			must.Equal(DeltaThoughtSummary, ev.Delta.Type)
			must.Equal("Reading sources", ev.Delta.Text)

			// step.delta: image with base64 data.
			ev = mustNext(t, s)
			must.NotNil(ev.Delta)
			must.Equal(DeltaImage, ev.Delta.Type)
			must.Equal("image/png", ev.Delta.MIMEType)
			must.Equal("png-bytes", string(ev.Delta.Data))
			must.Equal(2, ev.Index)

			// interaction.status_update.
			ev = mustNext(t, s)
			must.Equal(EventInteractionStatusUpdate, ev.Type, "status update = %+v", ev)
			must.Equal("v1_abc", ev.InteractionID)
			must.Equal(StatusCompleted, ev.Status)

			// interaction.completed: payload nested under "interaction".
			ev = mustNext(t, s)
			must.Equal(EventInteractionCompleted, ev.Type, "event 6 = %+v", ev)
			must.Equal("ev_6", ev.ID, "event 6 = %+v", ev)
			must.NotNil(ev.Interaction)
			must.Equal(StatusCompleted, ev.Interaction.Status)
			must.Equal("ev_6", s.LastEventID())

			// Clean end of stream.
			_, err = s.Next()
			must.ErrorIs(err, io.EOF)
		})
	}
}

func TestStream_Attach_SendsResumeQuery(t *testing.T) {
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
			must := require.New(t)
			var got url.Values
			c := sseClient(t, "", &got)
			s, err := c.Stream(context.Background(), "v1_abc", tt.lastEventID)
			must.NoError(err)
			defer s.Close()
			must.Equal("true", got.Get("stream"))
			if tt.wantResume {
				must.Equal(tt.lastEventID, got.Get("last_event_id"))
				must.Equal(tt.lastEventID, s.LastEventID(), "LastEventID before any event")
			} else {
				must.False(got.Has("last_event_id"), "last_event_id sent on fresh attach: %v", got)
			}
		})
	}
}

func TestStream_Next_DecodesErrorEvent(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	c := sseClient(t, `event: error
id: ev_9
data: {"error":{"code":"https://errors.example/internal","message":"boom"}}

`, nil)
	s, err := c.Stream(context.Background(), "v1_abc", "")
	must.NoError(err)
	defer s.Close()
	ev := mustNext(t, s)
	must.Equal(EventError, ev.Type, "event = %+v", ev)
	must.NotNil(ev.Err, "event = %+v", ev)
	check.Equal("https://errors.example/internal", ev.Err.Code)
	check.Equal("boom", ev.Err.Message)
}

func TestStream_Next_ReadsEnvelopeFieldsFromData(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// No SSE event/id lines: event_type and event_id travel in the data
	// payload instead, and must still be honoured for resume.
	c := sseClient(t, `data: {"event_type":"interaction.status_update","event_id":"ev_7","interaction_id":"v1_abc","status":"failed"}

`, nil)
	s, err := c.Stream(context.Background(), "v1_abc", "")
	must.NoError(err)
	defer s.Close()
	ev := mustNext(t, s)
	check.Equal(EventInteractionStatusUpdate, ev.Type)
	check.Equal("ev_7", ev.ID)
	check.Equal(StatusFailed, ev.Status)
	check.Equal("ev_7", s.LastEventID())
}

func TestStream_ID_PersistsAcrossFrames(t *testing.T) {
	must := require.New(t)
	c := sseClient(t, `id: ev_1
event: interaction.status_update
data: {"interaction_id":"v1_abc","status":"in_progress"}

event: step.delta
data: {"index":0,"delta":{"type":"text","text":"hi"}}

`, nil)
	s, err := c.Stream(context.Background(), "v1_abc", "")
	must.NoError(err)
	defer s.Close()
	mustNext(t, s)
	ev := mustNext(t, s)
	must.Equal("ev_1", ev.ID, "id-less frame should keep the persisted ID")
}

func TestStream_Next_KeepsRawPayloadForUnknownType(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	c := sseClient(t, `event: step.tool_call
id: ev_2
data: {"index":0,"tool":"google_search"}

`, nil)
	s, err := c.Stream(context.Background(), "v1_abc", "")
	must.NoError(err)
	defer s.Close()
	ev := mustNext(t, s)
	check.Equal(EventType("step.tool_call"), ev.Type)
	check.Equal("ev_2", ev.ID)
	check.Equal(`{"index":0,"tool":"google_search"}`, string(ev.Raw)) //nolint:testifylint // Raw is the verbatim payload, so equivalent JSON is not enough.
}

func TestStream_Next_DiscardsTruncatedFinalEvent(t *testing.T) {
	must := require.New(t)
	// The final frame has no terminating blank line: per the SSE spec it
	// is discarded, and the stream ends with io.EOF.
	c := sseClient(t, `event: step.delta
id: ev_1
data: {"index":0,"delta":{"type":"text","text":"hi"}}

event: step.delta
id: ev_2
data: {"index":0,"delta":{"type":"text","text":"trunc`, nil)
	s, err := c.Stream(context.Background(), "v1_abc", "")
	must.NoError(err)
	defer s.Close()
	mustNext(t, s)
	_, err = s.Next()
	must.ErrorIs(err, io.EOF)
}

func TestStream_Next_ReportsMalformedJSON(t *testing.T) {
	must := require.New(t)
	c := sseClient(t, `event: step.delta
id: ev_1
data: {not json

`, nil)
	s, err := c.Stream(context.Background(), "v1_abc", "")
	must.NoError(err)
	defer s.Close()
	_, err = s.Next()
	must.ErrorContains(err, "decoding")
}

func TestStream_Attach_ReturnsAPIErrorOnHTTPError(t *testing.T) {
	must := require.New(t)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"https://errors.example/not-found","message":"gone"}}`)
	}))
	_, err := c.Stream(context.Background(), "v1_gone", "")
	var apiErr *APIError
	must.ErrorAs(err, &apiErr)
	must.Equal(http.StatusNotFound, apiErr.HTTPStatus)
}

func TestStream_Attach_RetriesTransientFailure(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer s.Close()
	must.EqualValues(2, attempts.Load())
}

func TestStream_Attach_RejectsWrongContentType(t *testing.T) {
	must := require.New(t)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, sampleInteraction)
	}))
	_, err := c.Stream(context.Background(), "v1_abc", "")
	must.ErrorContains(err, "content type")
}

func TestStreamEventTooLarge(t *testing.T) {
	must := require.New(t)
	c := sseClient(t, "data: {\"pad\":\""+strings.Repeat("x", 1024)+"\"}\n\n", nil,
		WithMaxEventBytes(128))
	s, err := c.Stream(context.Background(), "v1_abc", "")
	must.NoError(err)
	defer s.Close()
	_, err = s.Next()
	must.ErrorIs(err, bufio.ErrTooLong)
}

func TestStreamEventDataAccumulationBounded(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// Each data line is under the 128-byte bound, but the accumulated payload
	// (80 + 1 + 80 bytes) must trip it before the event is dispatched.
	line := strings.Repeat("x", 80)
	body := "event: step.delta\ndata: " + line + "\ndata: " + line + "\n\n"
	c := sseClient(t, body, nil, WithMaxEventBytes(128))
	s, err := c.Stream(context.Background(), "v1_abc", "")
	must.NoError(err)
	defer s.Close()
	ev, err := s.Next()
	check.Nil(ev, "an over-bound event must not be dispatched")
	check.ErrorContains(err, "exceeds 128-byte bound")
}
