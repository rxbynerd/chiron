package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestResearchConfigSeedsKnownModelPrices: research-config resolves a
// worker config with a known model and a ceiling but no price flags into
// the seeded GBP prices from the built-in table, so the command output is
// exactly what the worker would run with.
func TestResearchConfigSeedsKnownModelPrices(t *testing.T) {
	stdout, _, err := execute(t, "research-config",
		"--agent", "worker",
		"--fleet-model-name", "gpt-5.5",
		"--fleet-ceiling", "2")
	if err != nil {
		t.Fatalf("research-config: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	fleet, _ := cfg["fleet"].(map[string]any)
	if fleet == nil {
		t.Fatalf("fleet block missing: %s", stdout)
	}
	if got := fleet["price_input_gbp_per_mtok"]; got != 3.7695 {
		t.Errorf("price_input_gbp_per_mtok = %v, want 3.7695", got)
	}
	if got := fleet["price_output_gbp_per_mtok"]; got != 22.617 {
		t.Errorf("price_output_gbp_per_mtok = %v, want 22.617", got)
	}
}

// TestResearchConfigExplicitPricesWin: an explicit price flag is never
// clobbered by the built-in table, even for a known model.
func TestResearchConfigExplicitPricesWin(t *testing.T) {
	stdout, _, err := execute(t, "research-config",
		"--agent", "worker",
		"--fleet-model-name", "gpt-5.5",
		"--fleet-ceiling", "2",
		"--fleet-price-input", "1.1",
		"--fleet-price-output", "2.2")
	if err != nil {
		t.Fatalf("research-config: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	fleet, _ := cfg["fleet"].(map[string]any)
	if got := fleet["price_input_gbp_per_mtok"]; got != 1.1 {
		t.Errorf("price_input_gbp_per_mtok = %v, want the explicit 1.1", got)
	}
	if got := fleet["price_output_gbp_per_mtok"]; got != 2.2 {
		t.Errorf("price_output_gbp_per_mtok = %v, want the explicit 2.2", got)
	}
}

// TestResearchConfigUnknownModelWithCeilingFails: a ceiling on a model
// absent from the built-in table fails validation naming both price
// fields, the price table, and the model id — before any network call.
func TestResearchConfigUnknownModelWithCeilingFails(t *testing.T) {
	_, _, err := execute(t, "research-config",
		"--agent", "worker",
		"--fleet-model-name", "not-a-real-model",
		"--fleet-ceiling", "2")
	if err == nil {
		t.Fatal("an unknown model with a ceiling and no prices validated")
	}
	for _, want := range []string{"fleet.price_input_gbp_per_mtok", "fleet.price_output_gbp_per_mtok", "price table", "not-a-real-model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}
