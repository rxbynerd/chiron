package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestApplyFlags_FleetFlags_ChangeOnlyTheirOwnField pins each fleet flag: set
// alone, only its field may change from Default().Fleet. A transposed pflag
// binding must fail here, not several layers up the CLI stack. The top-level
// levers follow the same pattern.
func TestApplyFlags_FleetFlags_ChangeOnlyTheirOwnField(t *testing.T) {
	for _, tt := range []struct {
		name  string
		args  []string
		check func(must *require.Assertions, f FleetConfig)
	}{
		{"model endpoint", []string{"--fleet-model-endpoint", "https://model.example"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal("https://model.example", f.ModelEndpoint)
		}},
		{"model name", []string{"--fleet-model-name", "gpt-5.5"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal("gpt-5.5", f.ModelName)
		}},
		{"model key ref", []string{"--fleet-model-key-ref", "secret://MODEL_KEY"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal("secret://MODEL_KEY", f.ModelKeyRef)
		}},
		{"search endpoint", []string{"--fleet-search-endpoint", "https://search.example"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal("https://search.example", f.SearchEndpoint)
		}},
		{"search key ref", []string{"--fleet-search-key-ref", "secret://SEARCH_KEY"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal("secret://SEARCH_KEY", f.SearchKeyRef)
		}},
		{"search tool", []string{"--fleet-search-tool", "web_search"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal("web_search", f.SearchTool)
		}},
		{"search query arg", []string{"--fleet-search-query-arg", "q"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal("q", f.SearchQueryArg)
		}},
		{"max turns", []string{"--fleet-max-turns", "12"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal(12, f.MaxTurns)
		}},
		{"max tokens", []string{"--fleet-max-tokens", "123456"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal(123456, f.MaxTokens)
		}},
		// Float fields use a zero delta: the comparison stays exact.
		{"ceiling", []string{"--fleet-ceiling", "2.25"}, func(must *require.Assertions, f FleetConfig) {
			must.InDelta(2.25, f.CeilingGBP, 0)
		}},
		{"price input", []string{"--fleet-price-input", "1.5"}, func(must *require.Assertions, f FleetConfig) {
			must.InDelta(1.5, f.PriceInputGBPPerMTok, 0)
		}},
		{"price output", []string{"--fleet-price-output", "12"}, func(must *require.Assertions, f FleetConfig) {
			must.InDelta(12, f.PriceOutputGBPPerMTok, 0)
		}},
		{"max page bytes", []string{"--fleet-max-page-bytes", "4096"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal(4096, f.MaxPageBytes)
		}},
		{"worker timeout", []string{"--fleet-worker-timeout", "90s"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal(90*time.Second, time.Duration(f.WorkerTimeout))
		}},
		{"max workers", []string{"--fleet-max-workers", "7"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal(7, f.MaxWorkers)
		}},
		{"concurrency", []string{"--fleet-concurrency", "4"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal(4, f.Concurrency)
		}},
		{"memory", []string{"--fleet-memory", MemoryInMemory}, func(must *require.Assertions, f FleetConfig) {
			must.Equal(MemoryInMemory, f.Memory)
		}},
		{"knowledge provider", []string{"--fleet-knowledge-provider", KnowledgeBillet}, func(must *require.Assertions, f FleetConfig) {
			must.Equal(KnowledgeBillet, f.KnowledgeProvider)
		}},
		{"knowledge endpoint", []string{"--fleet-knowledge-endpoint", "https://billet.internal/"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal("https://billet.internal/", f.KnowledgeEndpoint)
		}},
		{"knowledge key ref", []string{"--fleet-knowledge-key-ref", "secret://KB_KEY"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal("secret://KB_KEY", f.KnowledgeKeyRef)
		}},
		{"knowledge space", []string{"--fleet-knowledge-space", "notes"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal("notes", f.KnowledgeSpace)
		}},
		{"knowledge limit", []string{"--fleet-knowledge-limit", "11"}, func(must *require.Assertions, f FleetConfig) {
			must.Equal(11, f.KnowledgeLimit)
		}},
		{"knowledge remember", []string{"--fleet-knowledge-remember"}, func(must *require.Assertions, f FleetConfig) {
			must.True(f.KnowledgeRemember)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			cfg := Default()
			fs := newFlagSet(t)
			must.NoError(fs.Parse(tt.args), "parse")
			must.NoError(ApplyFlags(&cfg, fs), "ApplyFlags")
			tt.check(must, cfg.Fleet)

			// Only the flag under test may diverge from the defaults:
			// reset that one field and the whole config must equal Default().
			rest := cfg
			rest.Fleet = Default().Fleet
			must.Equal(Default(), rest, "an unrelated field changed")
		})
	}
}

// TestApplyFlags_FleetBlock_UnsetFlagsKeepBase is the overlay contract for the
// nested block: a piped base Fleet must survive when no fleet flag is set.
func TestApplyFlags_FleetBlock_UnsetFlagsKeepBase(t *testing.T) {
	must := require.New(t)
	base := Default()
	base.Agent = AgentFleet
	base.Fleet.ModelName = "from-the-pipe"
	base.Fleet.MaxTurns = 3

	fs := newFlagSet(t)
	must.NoError(fs.Parse([]string{"--visualise"}), "parse")
	must.NoError(ApplyFlags(&base, fs), "ApplyFlags")
	must.Equal("from-the-pipe", base.Fleet.ModelName)
	must.Equal(3, base.Fleet.MaxTurns)
}

// TestFleetSearchFlags_Usage_OmitsDuplicateDefault covers --fleet-search-tool
// and --fleet-search-query-arg, which carry a non-zero default that pflag's
// stock help template already annotates. The usage string must not repeat it.
func TestFleetSearchFlags_Usage_OmitsDuplicateDefault(t *testing.T) {
	fs := newFlagSet(t)
	for _, name := range []string{"fleet-search-tool", "fleet-search-query-arg"} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(fs.Lookup(name).Usage, "(default")
		})
	}
}
