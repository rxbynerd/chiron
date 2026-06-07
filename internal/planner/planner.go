// Package planner defines the collaborative-planning seam and the
// interactive session that drives it (PROPOSAL §3, INTERACTIONS-API §7).
// The three-step flow is: propose a plan (an interaction created with
// collaborative_planning enabled), refine it as often as the user asks
// (new interactions chained via previous_interaction_id, still
// planning), then approve. Approval is deliberately not part of this
// seam: the approved plan's interaction id chains into an ordinary
// research run — a create with previous_interaction_id and
// collaborative_planning off — so the run core needs no planning mode.
package planner

import (
	"context"
)

// Plan is one proposed research plan: the stored interaction that
// carries it — the handle the next refinement or the approval chains
// from — and its text as the agent rendered it.
type Plan struct {
	InteractionID string
	Text          string
}

// Planner produces and refines research plans before any research money
// is spent. Each call creates a paid (much cheaper than a full run, but
// not free) planning interaction, which is why Session bounds its
// rounds.
type Planner interface {
	// Propose asks the agent to plan the research for query.
	Propose(ctx context.Context, query string) (*Plan, error)
	// Refine asks for a revised plan against a previous plan
	// interaction, steered by the user's feedback.
	Refine(ctx context.Context, previousID, feedback string) (*Plan, error)
}
