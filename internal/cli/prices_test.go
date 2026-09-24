package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
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

// TestResearchSeededPriceTripsCostCeiling proves a table-seeded price
// reaches a live --agent worker run, not just the config JSON: no
// --fleet-price-input/--fleet-price-output flag is given, so the run can
// only stop on the ceiling if SeedModelPrices' output survived into the
// worker's Caps. gpt-5.5's seeded rate (3.7695 GBP/MTok input) puts one
// million-token turn's cost just under a £5 ceiling and two turns just
// over it; --fleet-max-tokens 0 disables the default 400000-token cap so
// the cost ceiling is what actually stops the run.
func TestResearchSeededPriceTripsCostCeiling(t *testing.T) {
	var replies []model.FakeReply
	for i := 0; i < 5; i++ {
		replies = append(replies, model.FakeReply{
			Content:      `{"action":"search","query":"again"}`,
			FinishReason: "stop",
			Usage:        model.Usage{InputTokens: 1_000_000, OutputTokens: 0, TotalTokens: 1_000_000},
		})
	}
	modelSrv := model.NewFakeServer(replies...)
	defer modelSrv.Close()
	searchSrv := search.NewFakeServer(nil)
	defer searchSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")

	stdout, stderr, err := execute(t,
		"research", "--query", "why is the sky blue",
		"--agent", "worker", "-o", "text",
		"--fleet-model-endpoint", modelSrv.URL(),
		"--fleet-model-name", "gpt-5.5",
		"--fleet-model-key-ref", "secret://MODEL_KEY",
		"--fleet-search-endpoint", searchSrv.URL(),
		"--fleet-max-tokens", "0",
		"--fleet-ceiling", "5",
	)
	exitErr, ok := errors.AsType[*ExitError](err)
	if !ok || exitErr.Code != ExitResearchFailed {
		t.Fatalf("err = %v, want ExitError code %d\nstderr: %s", err, ExitResearchFailed, stderr)
	}
	if !strings.Contains(stdout, "£5.00 cost ceiling") {
		t.Errorf("stdout does not report the seeded-price cost ceiling stopping the run:\n%s", stdout)
	}
	if got := modelSrv.CallCount(); got != 2 {
		t.Errorf("model call count = %d, want 2 (turn 1: £3.7695 < £5; turn 2: £7.539 >= £5, stop before turn 3)", got)
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
