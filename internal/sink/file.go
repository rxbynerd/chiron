package sink

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rxbynerd/chiron/internal/types"
)

// File writes the report to a path (--out) and its chart assets next to
// it, where the document's relative image links resolve. The formatter
// performs no IO by design (docs/DECISIONS.md); asset placement is this
// sink's job.
type File struct {
	path string
}

var _ ReportSink = (*File)(nil)

// NewFile returns a sink writing the report to path.
func NewFile(path string) *File {
	return &File{path: path}
}

// Write implements ReportSink. Assets land first so that a report file,
// once present, never references charts that are still being written.
// Asset names must be bare file names: anything path-shaped is rejected
// rather than resolved outside the report's directory.
func (f *File) Write(ctx context.Context, result *types.RunResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if result == nil || result.Report == nil {
		return errors.New("sink: no report to write")
	}
	if f.path == "" {
		return errors.New("sink: file sink needs a path")
	}
	dir := filepath.Dir(f.path)
	for _, a := range result.Report.Assets {
		if a.Name == "" || a.Name != filepath.Base(a.Name) {
			return fmt.Errorf("sink: asset name %q is not a bare file name", a.Name)
		}
		if err := os.WriteFile(filepath.Join(dir, a.Name), a.Data, 0o644); err != nil {
			return fmt.Errorf("sink: writing asset %s: %w", a.Name, err)
		}
	}
	if err := os.WriteFile(f.path, result.Report.Markdown, 0o644); err != nil {
		return fmt.Errorf("sink: writing report: %w", err)
	}
	return nil
}
