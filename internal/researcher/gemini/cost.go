package gemini

import "github.com/rxbynerd/chiron/internal/interactions"

// Planning cost estimates. Google publishes a per-tier cost envelope in
// USD (docs/INTERACTIONS-API.md §7, last_verified 2026-06-07):
//
//	deep-research      $1.00–$3.00  (~80 searches)
//	deep-research-max  $3.00–$7.00  (~160 searches)
//
// Chiron takes the midpoint of the envelope and converts at a fixed
// planning rate. This is a pre-run planning figure for the report's
// front matter and the budget lever — NOT billing: real pricing and
// attribution belong to Stint (PROPOSAL §2), which is also where a live
// FX rate would live.
const (
	// usdToGBPPlanningRate is the fixed conversion used for planning
	// figures. 0.79 reproduces the £0.80–£5.50 envelope PROPOSAL §2
	// quotes for the same USD range, keeping the two documents
	// consistent.
	usdToGBPPlanningRate = 0.79

	// Midpoints of the published per-tier USD envelopes.
	estimateUSDDeepResearch    = 2.00
	estimateUSDDeepResearchMax = 5.00
)

// estimatedCostGBP is the planning estimate for one task of the given
// tier. Unknown tiers estimate zero; tier validation happens in New.
func estimatedCostGBP(tier string) float64 {
	switch tier {
	case TierDeepResearch:
		return estimateUSDDeepResearch * usdToGBPPlanningRate
	case TierDeepResearchMax:
		return estimateUSDDeepResearchMax * usdToGBPPlanningRate
	default:
		return 0
	}
}

// estimateForAgentID maps a wire agent identifier back to its tier
// estimate — for resumed interactions (chiron get), where the tier is
// read off the interaction itself rather than configured. Follow-up
// interactions carry a model id instead and estimate zero: model-priced
// Q&A is outside the tier table, and inventing a figure for it would
// put a fabricated cost in the report's front matter.
func estimateForAgentID(agentID string) float64 {
	switch agentID {
	case interactions.AgentDeepResearch:
		return estimatedCostGBP(TierDeepResearch)
	case interactions.AgentDeepResearchMax:
		return estimatedCostGBP(TierDeepResearchMax)
	default:
		return 0
	}
}
