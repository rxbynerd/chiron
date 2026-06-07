package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/rxbynerd/chiron/internal/secret"
)

// JSONL is the local-debugging Tracer binding: one JSON object per line,
// spans written on End and metrics as they are recorded. Every string
// payload — span names, attribute keys and values, error messages,
// metric names — is routed through secret.Scrub before it is written, so
// a credential cannot transit the trace file any more than the logger.
type JSONL struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewJSONL returns a tracer writing newline-delimited JSON to w.
func NewJSONL(w io.Writer) *JSONL {
	return &JSONL{enc: json.NewEncoder(w)}
}

// NewJSONLFile opens path for appending (created 0600: trace payloads
// are scrubbed but still nobody else's business) and returns the tracer
// plus a close function for the caller to defer.
func NewJSONLFile(path string) (*JSONL, func() error, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("trace: opening %s: %w", path, err)
	}
	return NewJSONL(f), f.Close, nil
}

// StartSpan implements Tracer. Spans started from the returned context
// nest beneath this one and share its trace ID.
func (t *JSONL) StartSpan(ctx context.Context, name string) (context.Context, Span) {
	s := &jsonlSpan{
		t:      t,
		spanID: newID(8),
		name:   secret.Scrub(name),
		start:  time.Now(),
		attrs:  make(map[string]any),
	}
	if parent, ok := ctx.Value(jsonlKey{}).(*jsonlSpan); ok {
		s.traceID = parent.traceID
		s.parentID = parent.spanID
	} else {
		s.traceID = newID(16)
	}
	return context.WithValue(ctx, jsonlKey{}, s), s
}

// Metric implements Tracer, writing one metric line attributed to the
// current span if the context carries one.
func (t *JSONL) Metric(ctx context.Context, name string, value float64) {
	line := metricLine{
		Type:  "metric",
		Name:  secret.Scrub(name),
		Value: value,
		Time:  time.Now(),
	}
	if s, ok := ctx.Value(jsonlKey{}).(*jsonlSpan); ok {
		line.TraceID = s.traceID
		line.SpanID = s.spanID
	}
	t.write(line)
}

// write serialises one line under the tracer lock. Errors are dropped:
// tracing is best-effort and must never fail the run it observes.
func (t *JSONL) write(line any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.enc.Encode(line)
}

type jsonlKey struct{}

type jsonlSpan struct {
	t        *JSONL
	traceID  string
	spanID   string
	parentID string
	name     string
	start    time.Time

	mu    sync.Mutex
	attrs map[string]any
	ended bool
}

// SetAttr implements Span.
func (s *jsonlSpan) SetAttr(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.attrs[secret.Scrub(key)] = scrubValue(value)
}

// End implements Span, writing the span line. Ending twice is a no-op.
func (s *jsonlSpan) End(err error) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	end := time.Now()
	line := spanLine{
		Type:       "span",
		TraceID:    s.traceID,
		SpanID:     s.spanID,
		ParentID:   s.parentID,
		Name:       s.name,
		Start:      s.start,
		End:        end,
		DurationMS: float64(end.Sub(s.start)) / float64(time.Millisecond),
	}
	if len(s.attrs) > 0 {
		line.Attrs = s.attrs
	}
	if err != nil {
		line.Error = secret.Scrub(err.Error())
	}
	s.mu.Unlock()
	s.t.write(line)
}

type spanLine struct {
	Type       string         `json:"type"`
	TraceID    string         `json:"trace_id"`
	SpanID     string         `json:"span_id"`
	ParentID   string         `json:"parent_id,omitempty"`
	Name       string         `json:"name"`
	Start      time.Time      `json:"start"`
	End        time.Time      `json:"end"`
	DurationMS float64        `json:"duration_ms"`
	Attrs      map[string]any `json:"attrs,omitempty"`
	Error      string         `json:"error,omitempty"`
}

type metricLine struct {
	Type    string    `json:"type"`
	TraceID string    `json:"trace_id,omitempty"`
	SpanID  string    `json:"span_id,omitempty"`
	Name    string    `json:"name"`
	Value   float64   `json:"value"`
	Time    time.Time `json:"time"`
}

// scrubValue routes strings through the scrubber, passes non-string
// scalars as-is (a credential cannot hide in a number), and stringifies
// then scrubs everything else, mirroring the slog handler's posture.
func scrubValue(v any) any {
	switch x := v.(type) {
	case string:
		return secret.Scrub(x)
	case bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64,
		time.Time, time.Duration:
		return x
	default:
		return secret.Scrub(fmt.Sprint(x))
	}
}

// newID returns n random bytes hex-encoded. crypto/rand.Read is
// documented (Go 1.24+) to always succeed.
func newID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var _ Tracer = (*JSONL)(nil)
