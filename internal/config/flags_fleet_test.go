package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestApplyFlagsFleetLevers pins each fleet flag: set alone, only its
// field may change from Default().Fleet — a transposed pflag binding must
// fail here, not several layers up the CLI stack. The pattern mirrors
// TestApplyFlagsOutputAndCostLevers for the top-level levers.
func TestApplyFlagsFleetLevers(t *testing.T) {
	for _, tt := range []struct {
		name   string
		args   []string
		assert func(t *testing.T, f FleetConfig)
	}{
		{"model-endpoint", []string{"--fleet-model-endpoint", "https://model.example"}, func(t *testing.T, f FleetConfig) {
			if f.ModelEndpoint != "https://model.example" {
				t.Errorf("ModelEndpoint = %q", f.ModelEndpoint)
			}
		}},
		{"model-name", []string{"--fleet-model-name", "gpt-5.5"}, func(t *testing.T, f FleetConfig) {
			if f.ModelName != "gpt-5.5" {
				t.Errorf("ModelName = %q", f.ModelName)
			}
		}},
		{"model-key-ref", []string{"--fleet-model-key-ref", "secret://MODEL_KEY"}, func(t *testing.T, f FleetConfig) {
			if f.ModelKeyRef != "secret://MODEL_KEY" {
				t.Errorf("ModelKeyRef = %q", f.ModelKeyRef)
			}
		}},
		{"search-endpoint", []string{"--fleet-search-endpoint", "https://search.example"}, func(t *testing.T, f FleetConfig) {
			if f.SearchEndpoint != "https://search.example" {
				t.Errorf("SearchEndpoint = %q", f.SearchEndpoint)
			}
		}},
		{"search-key-ref", []string{"--fleet-search-key-ref", "secret://SEARCH_KEY"}, func(t *testing.T, f FleetConfig) {
			if f.SearchKeyRef != "secret://SEARCH_KEY" {
				t.Errorf("SearchKeyRef = %q", f.SearchKeyRef)
			}
		}},
		{"search-tool", []string{"--fleet-search-tool", "web_search"}, func(t *testing.T, f FleetConfig) {
			if f.SearchTool != "web_search" {
				t.Errorf("SearchTool = %q", f.SearchTool)
			}
		}},
		{"search-query-arg", []string{"--fleet-search-query-arg", "q"}, func(t *testing.T, f FleetConfig) {
			if f.SearchQueryArg != "q" {
				t.Errorf("SearchQueryArg = %q", f.SearchQueryArg)
			}
		}},
		{"max-turns", []string{"--fleet-max-turns", "12"}, func(t *testing.T, f FleetConfig) {
			if f.MaxTurns != 12 {
				t.Errorf("MaxTurns = %d", f.MaxTurns)
			}
		}},
		{"max-tokens", []string{"--fleet-max-tokens", "123456"}, func(t *testing.T, f FleetConfig) {
			if f.MaxTokens != 123456 {
				t.Errorf("MaxTokens = %d", f.MaxTokens)
			}
		}},
		{"ceiling", []string{"--fleet-ceiling", "2.25"}, func(t *testing.T, f FleetConfig) {
			if f.CeilingGBP != 2.25 {
				t.Errorf("CeilingGBP = %v", f.CeilingGBP)
			}
		}},
		{"price-input", []string{"--fleet-price-input", "1.5"}, func(t *testing.T, f FleetConfig) {
			if f.PriceInputGBPPerMTok != 1.5 {
				t.Errorf("PriceInputGBPPerMTok = %v", f.PriceInputGBPPerMTok)
			}
		}},
		{"price-output", []string{"--fleet-price-output", "12"}, func(t *testing.T, f FleetConfig) {
			if f.PriceOutputGBPPerMTok != 12 {
				t.Errorf("PriceOutputGBPPerMTok = %v", f.PriceOutputGBPPerMTok)
			}
		}},
		{"max-page-bytes", []string{"--fleet-max-page-bytes", "4096"}, func(t *testing.T, f FleetConfig) {
			if f.MaxPageBytes != 4096 {
				t.Errorf("MaxPageBytes = %d", f.MaxPageBytes)
			}
		}},
		{"worker-timeout", []string{"--fleet-worker-timeout", "90s"}, func(t *testing.T, f FleetConfig) {
			if time.Duration(f.WorkerTimeout) != 90*time.Second {
				t.Errorf("WorkerTimeout = %v", f.WorkerTimeout)
			}
		}},
		{"max-workers", []string{"--fleet-max-workers", "7"}, func(t *testing.T, f FleetConfig) {
			if f.MaxWorkers != 7 {
				t.Errorf("MaxWorkers = %d", f.MaxWorkers)
			}
		}},
		{"concurrency", []string{"--fleet-concurrency", "4"}, func(t *testing.T, f FleetConfig) {
			if f.Concurrency != 4 {
				t.Errorf("Concurrency = %d", f.Concurrency)
			}
		}},
		{"memory", []string{"--fleet-memory", MemoryInMemory}, func(t *testing.T, f FleetConfig) {
			if f.Memory != MemoryInMemory {
				t.Errorf("Memory = %q", f.Memory)
			}
		}},
		{"knowledge-provider", []string{"--fleet-knowledge-provider", KnowledgeBillet}, func(t *testing.T, f FleetConfig) {
			if f.KnowledgeProvider != KnowledgeBillet {
				t.Errorf("KnowledgeProvider = %q", f.KnowledgeProvider)
			}
		}},
		{"knowledge-endpoint", []string{"--fleet-knowledge-endpoint", "https://billet.internal/"}, func(t *testing.T, f FleetConfig) {
			if f.KnowledgeEndpoint != "https://billet.internal/" {
				t.Errorf("KnowledgeEndpoint = %q", f.KnowledgeEndpoint)
			}
		}},
		{"knowledge-key-ref", []string{"--fleet-knowledge-key-ref", "secret://KB_KEY"}, func(t *testing.T, f FleetConfig) {
			if f.KnowledgeKeyRef != "secret://KB_KEY" {
				t.Errorf("KnowledgeKeyRef = %q", f.KnowledgeKeyRef)
			}
		}},
		{"knowledge-space", []string{"--fleet-knowledge-space", "notes"}, func(t *testing.T, f FleetConfig) {
			if f.KnowledgeSpace != "notes" {
				t.Errorf("KnowledgeSpace = %q", f.KnowledgeSpace)
			}
		}},
		{"knowledge-limit", []string{"--fleet-knowledge-limit", "11"}, func(t *testing.T, f FleetConfig) {
			if f.KnowledgeLimit != 11 {
				t.Errorf("KnowledgeLimit = %d", f.KnowledgeLimit)
			}
		}},
		{"knowledge-remember", []string{"--fleet-knowledge-remember"}, func(t *testing.T, f FleetConfig) {
			if !f.KnowledgeRemember {
				t.Errorf("KnowledgeRemember = %v", f.KnowledgeRemember)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			fs := newFlagSet(t)
			if err := fs.Parse(tt.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := ApplyFlags(&cfg, fs); err != nil {
				t.Fatalf("ApplyFlags: %v", err)
			}
			tt.assert(t, cfg.Fleet)

			// Only the flag under test may diverge from the defaults:
			// reset that one field and the whole config must equal Default().
			rest := cfg
			rest.Fleet = Default().Fleet
			if !reflect.DeepEqual(rest, Default()) {
				t.Errorf("an unrelated field changed: %+v", cfg)
			}
		})
	}
}

// TestApplyFlagsFleetUnsetLeavesBase: the overlay contract for the nested
// block — a piped base Fleet must survive when no fleet flag is set.
func TestApplyFlagsFleetUnsetLeavesBase(t *testing.T) {
	base := Default()
	base.Agent = AgentFleet
	base.Fleet.ModelName = "from-the-pipe"
	base.Fleet.MaxTurns = 3

	fs := newFlagSet(t)
	if err := fs.Parse([]string{"--visualise"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ApplyFlags(&base, fs); err != nil {
		t.Fatalf("ApplyFlags: %v", err)
	}
	if base.Fleet.ModelName != "from-the-pipe" || base.Fleet.MaxTurns != 3 {
		t.Errorf("unset fleet flags clobbered the base: %+v", base.Fleet)
	}
}

// TestFleetSearchFlagUsageHasNoDuplicateDefault: --fleet-search-tool and
// --fleet-search-query-arg carry a non-zero default, which pflag's stock
// help template already annotates — the usage string must not repeat it.
func TestFleetSearchFlagUsageHasNoDuplicateDefault(t *testing.T) {
	fs := newFlagSet(t)
	for _, name := range []string{"fleet-search-tool", "fleet-search-query-arg"} {
		if usage := fs.Lookup(name).Usage; strings.Contains(usage, "(default") {
			t.Errorf("--%s usage %q duplicates pflag's own default annotation", name, usage)
		}
	}
}
