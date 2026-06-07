// Package fleet is the v2 Researcher seam: a stirrup-fleet orchestrator
// that plans, decomposes, and delegates a research question across
// parallel Stirrup jobs (internal sources) and Gemini Deep Research
// workers (external sources), per PROPOSAL.md §6. To the run core it is
// just another Researcher — the orchestration complexity stays behind
// Start/Await/Result.
//
// v1 declares the seam only: every method returns ErrNotImplemented.
package fleet

import (
	"context"
	"errors"
	"fmt"

	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/types"
)

// ErrNotImplemented is returned by every Fleet method: the stirrup-fleet
// orchestrator is a v2 binding, not implemented in v1.
var ErrNotImplemented = errors.New("stirrup-fleet researcher is a v2 seam, not implemented in v1")

// Fleet is the placeholder stirrup-fleet Researcher. It compiles and
// satisfies the Researcher interface so v2 can bind it from
// ResearchConfig without touching the core, but it carries no
// orchestrator.
type Fleet struct{}

var _ researcher.Researcher = Fleet{}

// New returns the placeholder fleet researcher.
func New() Fleet { return Fleet{} }

// Start always fails: the fleet orchestrator is a v2 seam.
func (Fleet) Start(_ context.Context, _ researcher.Task) (string, error) {
	return "", fmt.Errorf("fleet: start: %w", ErrNotImplemented)
}

// Await always fails: the fleet orchestrator is a v2 seam.
func (Fleet) Await(_ context.Context, _ string) error {
	return fmt.Errorf("fleet: await: %w", ErrNotImplemented)
}

// Result always fails: the fleet orchestrator is a v2 seam.
func (Fleet) Result(_ context.Context, _ string) (*types.Interaction, error) {
	return nil, fmt.Errorf("fleet: result: %w", ErrNotImplemented)
}
