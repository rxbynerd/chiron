package gemini

import (
	"math"

	"github.com/rxbynerd/chiron/internal/types"
)

// Cost figures come in two flavours here, and they must not be confused:
//
//   - The PLANNING estimate (estimatedCostGBP) is a per-tier constant
//     known before the run. It is what the --budget gate compares
//     against the cap, because nothing else exists before the spend.
//   - The DERIVED estimate (derivedCostGBP) prices the usage counters
//     the API actually reported for the run, at published list rates.
//     It is what the finished run reports — the cost summary, the
//     report front matter and the estimated_cost_gbp metric.
//
// Neither is billing: real pricing, live FX and attribution belong to
// Stint (PROPOSAL §2). The derived figure is a list-rate estimate that
// moves with the run; the planning figure is a fixed pre-run guess.

// Planning cost estimates. Google publishes a per-tier cost envelope in
// USD (docs/INTERACTIONS-API.md §7, last_verified 2026-06-07):
//
//	deep-research      $1.00–$3.00  (~80 searches)
//	deep-research-max  $3.00–$7.00  (~160 searches)
//
// Chiron takes the midpoint of the envelope and converts at a fixed
// planning rate.
const (
	// usdToGBPPlanningRate is the fixed conversion used for cost
	// figures. 0.79 reproduces the £0.80–£5.50 envelope PROPOSAL §2
	// quotes for the same USD range, keeping the two documents
	// consistent. A live FX rate is Stint's concern, not Chiron's.
	usdToGBPPlanningRate = 0.79

	// Midpoints of the published per-tier USD envelopes.
	estimateUSDDeepResearch    = 2.00
	estimateUSDDeepResearchMax = 5.00
)

// estimatedCostGBP is the planning estimate for one task of the given
// tier — the pre-run figure the budget gate uses. Unknown tiers
// estimate zero; tier validation happens in New.
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

// List rates for the derived estimate (docs/INTERACTIONS-API.md §7,
// last_verified 2026-08-06). The deep research agents have no rate card
// of their own: Google bills them at "standard Gemini list rates,
// including input, output, and intermediate input / reasoning tokens
// generated during agentic loops", plus the tools they use at their own
// rates. Chiron prices at the paid-tier rates for
// gemini-3.1-pro-preview — the model this adapter already pins for
// follow-up Q&A (DefaultFollowUpModel) — because that is the flagship
// tier the research agents run on, and because the resulting figures
// reproduce both published per-tier envelopes (pinned by a test in
// cost_test.go).
// Rates are per million tokens, USD, paid tier. Thought tokens bill as
// output ("output, including thinking") and tool-use tokens as input —
// the intermediate input the agentic loop generates.
//
// The published rate card has two columns, the higher one for prompts
// over 200k tokens. Chiron always prices at the standard column: the
// threshold is per request, and a research task issues many requests
// whose individual prompts are far smaller than the run's token total,
// so choosing the column from that total would systematically
// over-price. The consequence is a figure that under-reports a run
// whose individual requests did cross the threshold. That direction is
// acceptable here because this figure only reports a finished run — the
// --budget gate that guards spend uses the tier planning estimate
// above, which is deliberately the more conservative of the two.
const (
	inputUSDPerMTok  = 2.00
	cachedUSDPerMTok = 0.20
	outputUSDPerMTok = 12.00

	// searchUSD is grounding with Google Search at $14 per 1,000
	// requests. Chiron prices every search: the published free
	// allowance (5,000 requests/month, shared across models) is an
	// account-level balance Chiron cannot see, so assuming it would
	// under-report.
	searchUSD = 14.0 / 1000.0
)

// derivedCostGBP prices reported usage at list rates, rounded to the
// penny. A run that reported no usage — a failure before any tokens
// were spent, or an API that omitted the counters — costs zero here
// rather than inheriting a tier constant it never earned.
//
// Cached tokens are treated as a subset of the input total (the Gemini
// convention) and rebilled at the cached rate; a total that contradicts
// that is clamped rather than rejected, since a beta API's counters
// must not be able to produce a negative cost.
func derivedCostGBP(u types.Usage) float64 {
	input := nonNegative(u.InputTokens)
	cached := min(nonNegative(u.CachedTokens), input)
	uncached := input - cached

	usd := perMillion(uncached+nonNegative(u.ToolUseTokens), inputUSDPerMTok) +
		perMillion(cached, cachedUSDPerMTok) +
		perMillion(nonNegative(u.OutputTokens)+nonNegative(u.ThoughtTokens), outputUSDPerMTok) +
		float64(nonNegative(u.SearchCount))*searchUSD

	return math.Round(usd*usdToGBPPlanningRate*100) / 100
}

// perMillion prices a token count against a per-million-token rate.
func perMillion(tokens int, usdPerMTok float64) float64 {
	return float64(tokens) / 1_000_000 * usdPerMTok
}

func nonNegative(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
