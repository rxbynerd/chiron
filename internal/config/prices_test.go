package config

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// TestModelPricesTableIsWellFormed checks the invariants every row must
// hold: a positive USD price, a GBP figure the fixed rate actually
// produces, an https source, and a checked date that parses.
func TestModelPricesTableIsWellFormed(t *testing.T) {
	for id, row := range modelPrices {
		t.Run(id, func(t *testing.T) {
			price, ok := LookupModelPrice(id)
			if !ok {
				t.Fatalf("LookupModelPrice(%q) missed its own table entry", id)
			}
			if row.inputUSD <= 0 || row.outputUSD <= 0 {
				t.Errorf("USD prices must be positive: input %v output %v", row.inputUSD, row.outputUSD)
			}
			if diff := math.Abs(price.InputGBPPerMTok - row.inputUSD*usdToGBPRate); diff > 0.00005 {
				t.Errorf("input GBP = %v, want within 0.00005 of USD*rate (%v)", price.InputGBPPerMTok, row.inputUSD*usdToGBPRate)
			}
			if diff := math.Abs(price.OutputGBPPerMTok - row.outputUSD*usdToGBPRate); diff > 0.00005 {
				t.Errorf("output GBP = %v, want within 0.00005 of USD*rate (%v)", price.OutputGBPPerMTok, row.outputUSD*usdToGBPRate)
			}
			if !strings.HasPrefix(price.Source, "https://") {
				t.Errorf("source = %q, want an https URL", price.Source)
			}
			if _, err := time.Parse("2006-01-02", price.Checked); err != nil {
				t.Errorf("checked = %q does not parse as a date: %v", price.Checked, err)
			}
		})
	}
}

// TestLookupModelPriceGPT55PinsLiterals pins the gpt-5.5 row's GBP figures
// as hand-computed literals, so a change to the rate or the row is caught
// even if the within-tolerance check above would still pass.
func TestLookupModelPriceGPT55PinsLiterals(t *testing.T) {
	price, ok := LookupModelPrice("gpt-5.5")
	if !ok {
		t.Fatal("gpt-5.5 missed the built-in table")
	}
	if price.InputGBPPerMTok != 3.7695 {
		t.Errorf("input GBP/MTok = %v, want 3.7695", price.InputGBPPerMTok)
	}
	if price.OutputGBPPerMTok != 22.617 {
		t.Errorf("output GBP/MTok = %v, want 22.617", price.OutputGBPPerMTok)
	}
}

// TestLookupModelPriceExactCaseSensitiveMatch: an unknown id and a case
// variant of a known id both miss — the lookup never trims, normalises or
// prefix-matches.
func TestLookupModelPriceExactCaseSensitiveMatch(t *testing.T) {
	for _, id := range []string{"gpt-5.5-mini", "GPT-5.5", "claude-haiku-4-5"} {
		t.Run(id, func(t *testing.T) {
			if _, ok := LookupModelPrice(id); ok {
				t.Errorf("LookupModelPrice(%q) hit; want a miss", id)
			}
		})
	}
}

// TestExampleWorkerConfigValidatesAfterSeeding decodes the shipped worker
// base config and runs it through the same seed-then-validate path
// resolveConfig uses, so a table row the example relies on (gpt-5.5) being
// renamed or removed is caught here rather than only at run time.
func TestExampleWorkerConfigValidatesAfterSeeding(t *testing.T) {
	f, err := os.Open("../../examples/researchconfig/worker.yaml")
	if err != nil {
		t.Fatalf("open example config: %v", err)
	}
	defer f.Close()
	cfg, err := Decode(f)
	if err != nil {
		t.Fatalf("decode example config: %v", err)
	}
	cfg.SeedModelPrices()
	if err := cfg.Validate(); err != nil {
		t.Errorf("examples/researchconfig/worker.yaml no longer validates after seeding: %v", err)
	}
}

// TestSeedModelPrices covers the seeding contract: both fields seeded only
// for a known model, worker/fleet agent, with both prices still zero.
func TestSeedModelPrices(t *testing.T) {
	for _, tt := range []struct {
		name       string
		agent      string
		modelName  string
		priceIn    float64
		priceOut   float64
		wantSeeded bool
	}{
		{"known model, both unset", AgentWorker, "gpt-5.5", 0, 0, true},
		{"known model, both explicit", AgentWorker, "gpt-5.5", 1.2, 9.6, false},
		{"known model, input explicit only", AgentWorker, "gpt-5.5", 1.2, 0, false},
		{"known model, output explicit only", AgentWorker, "gpt-5.5", 0, 9.6, false},
		{"unknown model", AgentWorker, "not-a-real-model", 0, 0, false},
		{"deep-research agent, known model", AgentDeepResearch, "gpt-5.5", 0, 0, false},
		{"fleet agent, known model", AgentFleet, "gpt-5.5", 0, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Agent = tt.agent
			cfg.Fleet.ModelName = tt.modelName
			cfg.Fleet.PriceInputGBPPerMTok = tt.priceIn
			cfg.Fleet.PriceOutputGBPPerMTok = tt.priceOut

			cfg.SeedModelPrices()

			if !tt.wantSeeded {
				if cfg.Fleet.PriceInputGBPPerMTok != tt.priceIn || cfg.Fleet.PriceOutputGBPPerMTok != tt.priceOut {
					t.Errorf("prices changed to %v/%v, want unchanged %v/%v",
						cfg.Fleet.PriceInputGBPPerMTok, cfg.Fleet.PriceOutputGBPPerMTok, tt.priceIn, tt.priceOut)
				}
				return
			}
			want, _ := LookupModelPrice(tt.modelName)
			if cfg.Fleet.PriceInputGBPPerMTok != want.InputGBPPerMTok || cfg.Fleet.PriceOutputGBPPerMTok != want.OutputGBPPerMTok {
				t.Errorf("seeded prices = %v/%v, want %v/%v",
					cfg.Fleet.PriceInputGBPPerMTok, cfg.Fleet.PriceOutputGBPPerMTok, want.InputGBPPerMTok, want.OutputGBPPerMTok)
			}
		})
	}
}
