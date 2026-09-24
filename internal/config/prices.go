package config

import "math"

// usdToGBPRate is the fixed USD->GBP rate the built-in model price table is
// converted at: the Bank of England XUDLUSS daily spot rate for 23 Sep
// 2026 (1.3265 USD per GBP, inverted and rounded to 4 dp). It is a cited
// spot rate for converting per-token list prices, distinct from the
// planning-envelope rate in internal/researcher/gemini/cost.go.
const usdToGBPRate = 0.7539

// ModelPrice is one row of the built-in price table, in both currencies so
// a caller can show its provenance.
type ModelPrice struct {
	InputGBPPerMTok  float64
	OutputGBPPerMTok float64
	InputUSDPerMTok  float64
	OutputUSDPerMTok float64
	USDToGBP         float64
	Source           string
	Checked          string
}

// modelPriceRow is the table's raw USD entry; LookupModelPrice derives the
// GBP fields from it at the fixed rate.
type modelPriceRow struct {
	inputUSD  float64
	outputUSD float64
	source    string
	checked   string
}

// modelPrices holds standard-tier list prices (not batch, priority/flex,
// or cached input; the short-context rate) for models known to be usable
// by the in-process worker, keyed by exact API model id. GBP is derived
// from the USD figures at usdToGBPRate. See docs/DECISIONS.md for sources,
// exclusions and the estimate's known biases.
var modelPrices = map[string]modelPriceRow{
	"gpt-5.5": {
		inputUSD: 5.00, outputUSD: 30.00,
		source:  "https://developers.openai.com/api/docs/models/gpt-5.5",
		checked: "2026-09-25",
	},
	"gpt-5.4-mini": {
		inputUSD: 0.75, outputUSD: 4.50,
		source:  "https://developers.openai.com/api/docs/models/gpt-5.4-mini",
		checked: "2026-09-25",
	},
	"gpt-5.4-nano": {
		inputUSD: 0.20, outputUSD: 1.25,
		source:  "https://developers.openai.com/api/docs/models/gpt-5.4-nano",
		checked: "2026-09-25",
	},
	"gpt-5.6-terra": {
		inputUSD: 2.00, outputUSD: 12.00,
		source:  "https://developers.openai.com/api/docs/models/gpt-5.6-terra",
		checked: "2026-09-25",
	},
	"gpt-5.6-luna": {
		inputUSD: 0.20, outputUSD: 1.20,
		source:  "https://developers.openai.com/api/docs/models/gpt-5.6-luna",
		checked: "2026-09-25",
	},
	"claude-fable-5-1": {
		inputUSD: 10.00, outputUSD: 50.00,
		source:  "https://platform.claude.com/docs/en/about-claude/pricing",
		checked: "2026-09-25",
	},
	"claude-opus-5-5": {
		inputUSD: 4.00, outputUSD: 20.00,
		source:  "https://platform.claude.com/docs/en/about-claude/pricing",
		checked: "2026-09-25",
	},
	"claude-sonnet-5": {
		inputUSD: 2.00, outputUSD: 10.00,
		source:  "https://platform.claude.com/docs/en/about-claude/pricing",
		checked: "2026-09-25",
	},
	"claude-haiku-4-5-20251001": {
		inputUSD: 1.00, outputUSD: 5.00,
		source:  "https://platform.claude.com/docs/en/about-claude/pricing",
		checked: "2026-09-25",
	},
}

// LookupModelPrice looks up a model's built-in list price by exact,
// case-sensitive API model id — no trimming, normalisation or prefix
// matching, since a near-miss silently seeding the wrong price is worse
// than a miss.
func LookupModelPrice(model string) (ModelPrice, bool) {
	row, ok := modelPrices[model]
	if !ok {
		return ModelPrice{}, false
	}
	return ModelPrice{
		InputGBPPerMTok:  roundGBP(row.inputUSD),
		OutputGBPPerMTok: roundGBP(row.outputUSD),
		InputUSDPerMTok:  row.inputUSD,
		OutputUSDPerMTok: row.outputUSD,
		USDToGBP:         usdToGBPRate,
		Source:           row.source,
		Checked:          row.checked,
	}, true
}

// roundGBP rounds a converted GBP price to 4 dp so the emitted JSON is
// clean rather than carrying a float's binary-conversion noise.
func roundGBP(usd float64) float64 {
	return math.Round(usd*usdToGBPRate*1e4) / 1e4
}

// SeedModelPrices fills the fleet price fields from the built-in table when
// both are still zero and the configured model is known, so
// fleet.ceiling_gbp works without price flags for a known model. Any
// explicit price — either field non-zero — leaves the config untouched,
// and ceiling presence plays no part: seeding is about the model and the
// prices alone.
func (c *ResearchConfig) SeedModelPrices() {
	if c.Agent != AgentWorker && c.Agent != AgentFleet {
		return
	}
	if c.Fleet.PriceInputGBPPerMTok != 0 || c.Fleet.PriceOutputGBPPerMTok != 0 {
		return
	}
	price, ok := LookupModelPrice(c.Fleet.ModelName)
	if !ok {
		return
	}
	c.Fleet.PriceInputGBPPerMTok = price.InputGBPPerMTok
	c.Fleet.PriceOutputGBPPerMTok = price.OutputGBPPerMTok
}
