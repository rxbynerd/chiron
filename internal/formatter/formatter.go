// Package formatter defines the Formatter seam: turning a completed
// Interaction into a portable Markdown report. v1 binds a markdown
// implementation; v2 reuses it with citation-pass awareness.
package formatter

import (
	"context"

	"github.com/rxbynerd/chiron/internal/types"
)

// Formatter renders an Interaction as a report: front matter, body,
// embedded charts, and a sources section.
type Formatter interface {
	Format(ctx context.Context, in *types.Interaction) (*types.Report, error)
}
