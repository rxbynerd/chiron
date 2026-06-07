package sink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/rxbynerd/chiron/internal/types"
)

// StdoutJSON writes the whole RunResult as one JSON document — the
// machine-consumption surface (--output json) that lets Stirrup's eval
// system drive and judge Chiron (PROPOSAL §4.3). The report's Markdown
// and chart assets travel inline as base64 per encoding/json's []byte
// convention.
type StdoutJSON struct {
	w io.Writer
}

var _ ReportSink = (*StdoutJSON)(nil)

// NewStdoutJSON returns a sink writing the RunResult JSON to w. A nil w
// means os.Stdout, the default for CLI runs.
func NewStdoutJSON(w io.Writer) *StdoutJSON {
	if w == nil {
		w = os.Stdout
	}
	return &StdoutJSON{w: w}
}

// Write implements ReportSink.
func (s *StdoutJSON) Write(ctx context.Context, result *types.RunResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if result == nil {
		return errors.New("sink: no run result to write")
	}
	if err := json.NewEncoder(s.w).Encode(result); err != nil {
		return fmt.Errorf("sink: encoding run result: %w", err)
	}
	return nil
}
