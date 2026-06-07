package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Stdio is the v1 Transport: one NDJSON line per event. Events default
// to stderr, not stdout — stdout belongs to the report when the
// stdout-markdown or stdout-json sink is bound, and run events must not
// corrupt a piped report (see docs/DECISIONS.md).
type Stdio struct {
	mu sync.Mutex
	w  io.Writer
}

var _ Transport = (*Stdio)(nil)

// NewStdio returns a Stdio transport writing NDJSON events to w. A nil
// w means os.Stderr, the default for CLI runs.
func NewStdio(w io.Writer) *Stdio {
	if w == nil {
		w = os.Stderr
	}
	return &Stdio{w: w}
}

// Emit writes ev as one NDJSON line. A zero Time is stamped with the
// current UTC time so consumers can always order events. Each event is
// written with a single Write call under a mutex, so concurrent emitters
// never interleave partial lines.
func (s *Stdio) Emit(ctx context.Context, ev Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("transport: marshal %q event: %w", ev.Kind, err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(line); err != nil {
		return fmt.Errorf("transport: write %q event: %w", ev.Kind, err)
	}
	return nil
}

// Close is a no-op: Stdio writes each event unbuffered and does not own
// its writer.
func (*Stdio) Close() error { return nil }
