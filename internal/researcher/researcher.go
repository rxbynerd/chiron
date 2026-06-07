// Package researcher defines the Researcher seam: the only model-bearing
// component in Chiron. v1 binds a hand-rolled Gemini Deep Research adapter;
// v2 binds a stirrup-fleet orchestrator. The run core depends only on this
// interface, so swapping implementations never touches the loop.
package researcher

import (
	"context"

	"github.com/rxbynerd/chiron/internal/types"
)

// Task describes one research task to start.
type Task struct {
	// Query is the research question.
	Query string
	// PreviousInteractionID, when set, asks a follow-up against a completed
	// interaction (chiron follow-up).
	PreviousInteractionID string
}

// Researcher runs a research task to an Interaction. The split into
// start/await/result keeps the run core a deterministic state machine and
// makes resume natural: Await and Result accept any interaction ID,
// including one recovered from a crashed run (chiron get <id>).
type Researcher interface {
	// Start begins a research task and returns the interaction ID — the
	// resume handle, emitted to the user as soon as it is known.
	Start(ctx context.Context, task Task) (id string, err error)

	// Await blocks until the interaction completes or fails, by polling or
	// streaming as the implementation is configured.
	Await(ctx context.Context, id string) error

	// Result retrieves the interaction in its current state.
	Result(ctx context.Context, id string) (*types.Interaction, error)
}
