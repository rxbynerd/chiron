package interactions

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// EventType discriminates SSE events (docs/INTERACTIONS-API.md §5).
// Consumers must tolerate types beyond these — the API also emits tool
// call/result events — which Next surfaces with Raw populated.
type EventType string

// Event types.
const (
	EventInteractionCreated      EventType = "interaction.created"
	EventStepDelta               EventType = "step.delta"
	EventInteractionStatusUpdate EventType = "interaction.status_update"
	EventInteractionCompleted    EventType = "interaction.completed"
	EventError                   EventType = "error"
)

// DeltaType discriminates step.delta payloads. The API also emits tool
// call/result delta types; consumers must tolerate values beyond these.
type DeltaType string

// Delta types.
const (
	DeltaText           DeltaType = "text"
	DeltaThoughtSummary DeltaType = "thought_summary_delta"
	DeltaImage          DeltaType = "image"
)

// Delta is one incremental content update within a step.delta event.
type Delta struct {
	Type     DeltaType `json:"type"`
	Text     string    `json:"text,omitempty"`
	MIMEType string    `json:"mime_type,omitempty"`
	Data     []byte    `json:"data,omitempty"`
}

// Event is one server-sent event. Which payload fields are set depends
// on Type; Raw always carries the verbatim data payload, so event types
// this package does not (yet) model are surfaced rather than dropped.
type Event struct {
	// ID is the event identifier used for resume (?last_event_id=). Per
	// the SSE convention it persists from the most recent event that
	// carried one.
	ID   string
	Type EventType

	// Interaction is set for interaction.created and
	// interaction.completed. The completion event may omit content — do
	// a plain Get for the final resource (docs/INTERACTIONS-API.md §5).
	Interaction *Interaction
	// Index and Delta are set for step.delta.
	Index int
	Delta *Delta
	// InteractionID and Status are set for interaction.status_update.
	InteractionID string
	Status        Status
	// Err is set for error events.
	Err *APIError

	// Raw is the verbatim data payload.
	Raw json.RawMessage
}

// eventEnvelope is the union of the documented event payload fields
// (docs/INTERACTIONS-API.md §5), decoded tolerantly: absent fields stay
// zero and unknown fields are ignored. event_type/event_id are accepted
// here as well as in the SSE event/id fields, because the reference does
// not pin down which side of the frame they travel on.
type eventEnvelope struct {
	EventType     string       `json:"event_type"`
	EventID       string       `json:"event_id"`
	Interaction   *Interaction `json:"interaction"`
	InteractionID string       `json:"interaction_id"`
	Status        Status       `json:"status"`
	Index         int          `json:"index"`
	Delta         *Delta       `json:"delta"`
	Error         *APIError    `json:"error"`
}

// Stream attaches to a stored interaction's event stream:
// GET /v1beta/interactions/{id}?stream=true[&last_event_id=...]
// (docs/INTERACTIONS-API.md §5). Pass lastEventID "" to attach from the
// start, or the last seen event ID to resume from the next chunk after
// it — resume is via the query parameter, not the Last-Event-ID header,
// which the docs do not promise is honoured.
//
// The caller drives reconnect policy: on a dropped stream, call Stream
// again with LastEventID(). No overall timeout applies — the stream
// lives until ctx is cancelled, the server ends it, or Close is called.
func (c *Client) Stream(ctx context.Context, id, lastEventID string) (*Stream, error) {
	path, err := interactionPath(id)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("stream", "true")
	if lastEventID != "" {
		query.Set("last_event_id", lastEventID)
	}
	resp, err := c.do(ctx, http.MethodGet, path, query, nil, "text/event-stream")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, errorFromResponse(resp)
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "text/event-stream" {
		resp.Body.Close()
		return nil, fmt.Errorf("interactions: streaming: unexpected content type %q", resp.Header.Get("Content-Type"))
	}
	bufSize := min(int64(64<<10), c.maxEventBytes)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, bufSize), int(c.maxEventBytes))
	return &Stream{
		body:          resp.Body,
		scanner:       scanner,
		lastEventID:   lastEventID,
		maxEventBytes: c.maxEventBytes,
	}, nil
}

// Stream is one attached text/event-stream connection. It is a parsing
// primitive: reconnect-with-resume policy belongs to the caller.
type Stream struct {
	body          io.ReadCloser
	scanner       *bufio.Scanner
	lastEventID   string
	maxEventBytes int64
}

// LastEventID returns the most recent event ID seen — or, before any
// event arrives, the value the stream was resumed with. Feed it back to
// Client.Stream to resume after a drop.
func (s *Stream) LastEventID() string { return s.lastEventID }

// Close releases the underlying connection.
func (s *Stream) Close() error { return s.body.Close() }

// Next blocks for the next event. It returns io.EOF when the server ends
// the stream cleanly (discarding any unterminated partial event, per the
// SSE spec); any other error means the stream broke, and the caller may
// reconnect with Client.Stream(ctx, id, s.LastEventID()).
func (s *Stream) Next() (*Event, error) {
	var (
		eventType string
		frameID   string
		data      []byte
		haveData  bool
	)
	for s.scanner.Scan() {
		line := strings.TrimSuffix(s.scanner.Text(), "\r")
		switch {
		case line == "":
			if !haveData {
				// Nothing to dispatch (keep-alive blank lines or a
				// dataless frame); reset and keep reading.
				eventType, frameID = "", ""
				continue
			}
			return s.buildEvent(eventType, frameID, data)
		case strings.HasPrefix(line, ":"):
			// Comment / keep-alive line.
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				eventType = value
			case "data":
				if haveData {
					data = append(data, '\n')
				}
				data = append(data, value...)
				haveData = true
				// The scanner bounds individual lines; this bounds the
				// payload accumulated across multiple data: lines of one
				// event, so fragmenting cannot bypass maxEventBytes.
				if int64(len(data)) > s.maxEventBytes {
					return nil, fmt.Errorf("interactions: SSE event data exceeds %d-byte bound", s.maxEventBytes)
				}
			case "id":
				// Per the SSE spec, ignore ids containing NUL.
				if !strings.ContainsRune(value, 0) {
					frameID = value
					s.lastEventID = value
				}
			default:
				// "retry" and unknown fields are ignored.
			}
		}
	}
	if err := s.scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("interactions: SSE event exceeds %d-byte bound: %w", s.maxEventBytes, err)
		}
		return nil, fmt.Errorf("interactions: reading stream: %w", err)
	}
	return nil, io.EOF
}

// buildEvent decodes one dispatched frame into an Event.
func (s *Stream) buildEvent(sseType, frameID string, data []byte) (*Event, error) {
	var env eventEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("interactions: decoding %q event: %w", sseType, err)
	}
	if frameID == "" && env.EventID != "" {
		// The frame carried its event_id in the JSON payload rather
		// than the SSE id field; track it for resume all the same.
		s.lastEventID = env.EventID
	}
	typ := EventType(sseType)
	if typ == "" {
		typ = EventType(env.EventType)
	}
	ev := &Event{
		ID:   s.lastEventID,
		Type: typ,
		Raw:  json.RawMessage(data),
	}
	switch typ {
	case EventInteractionCreated, EventInteractionCompleted:
		in := env.Interaction
		if in == nil {
			// Tolerate the resource as the payload itself rather than
			// nested under an "interaction" key — the reference shows
			// only "interaction resource" for these events.
			in = new(Interaction)
			if err := json.Unmarshal(data, in); err != nil {
				return nil, fmt.Errorf("interactions: decoding %q event: %w", typ, err)
			}
		}
		ev.Interaction = in
	case EventStepDelta:
		ev.Index = env.Index
		ev.Delta = env.Delta
	case EventInteractionStatusUpdate:
		ev.InteractionID = env.InteractionID
		ev.Status = env.Status
	case EventError:
		if env.Error != nil {
			ev.Err = env.Error
		} else {
			ev.Err = &APIError{Message: snippet(data, 256)}
		}
	default:
		// Unknown event type: Raw carries the payload; nothing else to
		// decode. Forward-compatible by design.
	}
	return ev, nil
}
