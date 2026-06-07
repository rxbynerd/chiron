package sink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/rxbynerd/chiron/internal/types"
)

// StdoutMarkdown writes the report's Markdown document to a writer —
// stdout for CLI runs. Chart assets cannot be placed on a byte stream,
// so they are not written; use the file sink (--out) when the report
// embeds charts, or stdout-json, which carries the assets inline.
type StdoutMarkdown struct {
	w io.Writer
}

var _ ReportSink = (*StdoutMarkdown)(nil)

// NewStdoutMarkdown returns a sink writing the Markdown document to w.
// A nil w means os.Stdout, the default for CLI runs.
func NewStdoutMarkdown(w io.Writer) *StdoutMarkdown {
	if w == nil {
		w = os.Stdout
	}
	return &StdoutMarkdown{w: w}
}

// Write implements ReportSink.
func (s *StdoutMarkdown) Write(ctx context.Context, result *types.RunResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if result == nil || result.Report == nil {
		return errors.New("sink: no report to write")
	}
	if _, err := s.w.Write(result.Report.Markdown); err != nil {
		return fmt.Errorf("sink: writing report: %w", err)
	}
	return nil
}
