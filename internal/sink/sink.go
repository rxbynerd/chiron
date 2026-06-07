// Package sink defines the ReportSink seam: where the final report goes.
// v1 binds stdout-markdown, file, and stdout-json sinks; the shape is
// unchanged in v2.
package sink

import (
	"context"

	"github.com/rxbynerd/chiron/internal/types"
)

// ReportSink writes the final report of a run. Implementations decide the
// destination (stdout, file, object store) and the wire form (markdown,
// JSON envelope).
type ReportSink interface {
	Write(ctx context.Context, result *types.RunResult) error
}
