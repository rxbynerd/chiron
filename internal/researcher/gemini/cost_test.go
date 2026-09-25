package gemini

import (
	"testing"

	"github.com/rxbynerd/chiron/internal/types"
)

func TestPlanningEstimatesAreTierMidpoints(t *testing.T) {
	// The pre-run figure the budget gate compares against --budget.
	cases := []struct {
		tier string
		want float64
	}{
		{TierDeepResearch, 2.00 * 0.79},
		{TierDeepResearchMax, 5.00 * 0.79},
		{"", 0},
		{"deep-research-ultra", 0},
	}
	for _, tt := range cases {
		if got := estimatedCostGBP(tt.tier); got != tt.want {
			t.Errorf("estimatedCostGBP(%q) = %v, want %v", tt.tier, got, tt.want)
		}
	}
}

func TestDerivedCostReproducesPublishedEnvelopes(t *testing.T) {
	// The justification for pricing the research agents at the flagship
	// model's list rates: doing so lands both tiers inside the per-tier
	// cost envelopes Google publishes for them
	// (docs/INTERACTIONS-API.md §7). If a future rate change breaks
	// that agreement, the rate card is no longer the one the agents
	// bill at and the table needs revisiting — this test fails first.
	cases := []struct {
		name          string
		usage         types.Usage
		lowUSD, hiUSD float64
	}{
		{
			// deep-research: ~250k input (50–70% cached), ~60k output,
			// ~80 searches. Envelope $1.00–$3.00.
			name: "deep-research typical",
			usage: types.Usage{
				InputTokens: 250_000, CachedTokens: 150_000,
				OutputTokens: 60_000, ThoughtTokens: 20_000,
				ToolUseTokens: 40_000, SearchCount: 80,
			},
			lowUSD: 1.00, hiUSD: 3.00,
		},
		{
			// deep-research-max: ~900k input (50–70% cached), ~80k
			// output, ~160 searches. Envelope $3.00–$7.00.
			name: "deep-research-max typical",
			usage: types.Usage{
				InputTokens: 900_000, CachedTokens: 540_000,
				OutputTokens: 80_000, ThoughtTokens: 40_000,
				ToolUseTokens: 150_000, SearchCount: 160,
			},
			lowUSD: 3.00, hiUSD: 7.00,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			gbp := derivedCostGBP(tt.usage)
			usd := gbp / usdToGBPPlanningRate
			if usd < tt.lowUSD || usd > tt.hiUSD {
				t.Errorf("derived cost = £%.2f (~$%.2f), outside the published $%.2f–$%.2f envelope",
					gbp, usd, tt.lowUSD, tt.hiUSD)
			}
		})
	}
}

func TestDerivedCostTracksUsage(t *testing.T) {
	// The defect this replaced: a run reporting twice the tokens must
	// not report the same cost.
	small := types.Usage{InputTokens: 250_000, OutputTokens: 30_000, SearchCount: 40}
	large := types.Usage{InputTokens: 705_318, OutputTokens: 25_271, ToolUseTokens: 217_780, ThoughtTokens: 69_044, SearchCount: 97}
	if derivedCostGBP(large) <= derivedCostGBP(small) {
		t.Errorf("a heavier run costs %v, want more than the lighter run's %v",
			derivedCostGBP(large), derivedCostGBP(small))
	}
	if got := derivedCostGBP(types.Usage{}); got != 0 {
		t.Errorf("empty usage = %v, want 0 — no counters, no claim", got)
	}
	// Poll and reconnect counts are Chiron-side telemetry, not spend.
	if got := derivedCostGBP(types.Usage{PollCount: 40, ReconnectCount: 3}); got != 0 {
		t.Errorf("polling alone = %v, want 0", got)
	}
}

func TestDerivedCostPricesEachCounterAtItsRate(t *testing.T) {
	// One counter at a time, so a rate transposed between input,
	// cached, output and search fails here rather than hiding inside a
	// plausible-looking total.
	cases := []struct {
		name    string
		usage   types.Usage
		wantUSD float64
	}{
		{"uncached input", types.Usage{InputTokens: 1_000_000}, 2.00},
		{"cached input at the cached rate", types.Usage{InputTokens: 1_000_000, CachedTokens: 1_000_000}, 0.20},
		{"output", types.Usage{OutputTokens: 1_000_000}, 12.00},
		{"thoughts bill as output", types.Usage{ThoughtTokens: 1_000_000}, 12.00},
		{"tool use bills as input", types.Usage{ToolUseTokens: 1_000_000}, 2.00},
		{"searches", types.Usage{SearchCount: 1000}, 14.00},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.wantUSD * usdToGBPPlanningRate
			if got := derivedCostGBP(tt.usage); !within(got, want, 0.005) {
				t.Errorf("cost = %v, want ~%v ($%.2f at the planning FX rate)", got, want, tt.wantUSD)
			}
		})
	}
}

func TestDerivedCostSurvivesIncoherentCounters(t *testing.T) {
	// A beta API's counters must not be able to produce a negative or
	// nonsensical cost: cached tokens exceeding the input total are
	// clamped, and negatives are floored.
	if got := derivedCostGBP(types.Usage{InputTokens: 1000, CachedTokens: 9_000_000}); got < 0 {
		t.Errorf("cached > input = %v, want a non-negative cost", got)
	}
	if got := derivedCostGBP(types.Usage{InputTokens: -5000, OutputTokens: -1, SearchCount: -3}); got != 0 {
		t.Errorf("negative counters = %v, want 0", got)
	}
}

func within(got, want, tol float64) bool {
	d := got - want
	return d < tol && d > -tol
}
