// Package fleet holds Chiron's own in-process research agents behind the
// Researcher seam: Worker, a bounded search -> read -> synthesise loop over
// one standard model with a web-search MCP tool and web_fetch, and Fleet,
// the placeholder for the lead orchestrator that will fan out many workers
// (docs/V2-RESEARCH-AGENT.md §6). To the run core both are just Researchers.
package fleet

import (
	"context"
	"errors"
	"fmt"

	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/types"
)

// ErrNotImplemented is returned by every Fleet method: the fleet lead
// orchestrator is not implemented yet.
var ErrNotImplemented = errors.New("the fleet researcher is not implemented yet; use --agent worker")

// Fleet is the placeholder fleet Researcher. It satisfies the Researcher
// interface so the composition root can name it, but carries no
// orchestrator.
type Fleet struct{}

var _ researcher.Researcher = Fleet{}

// New returns the placeholder fleet researcher.
func New() Fleet { return Fleet{} }

// Start always fails: the fleet orchestrator is not implemented.
func (Fleet) Start(_ context.Context, _ researcher.Task) (string, error) {
	return "", fmt.Errorf("fleet: start: %w", ErrNotImplemented)
}

// Await always fails: the fleet orchestrator is not implemented.
func (Fleet) Await(_ context.Context, _ string) error {
	return fmt.Errorf("fleet: await: %w", ErrNotImplemented)
}

// Result always fails: the fleet orchestrator is not implemented.
func (Fleet) Result(_ context.Context, _ string) (*types.Interaction, error) {
	return nil, fmt.Errorf("fleet: result: %w", ErrNotImplemented)
}
