package config

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// validExa is validWorker with Exa serving both web roles.
func validExa() ResearchConfig {
	cfg := validWorker()
	cfg.Fleet.SearchEndpoint = ""
	cfg.Fleet.SearchKeyRef = ""
	cfg.Fleet.SearchProvider = SearchExa
	cfg.Fleet.FetchProvider = FetchExa
	cfg.Fleet.ExaEndpoint = "https://api.exa.ai"
	cfg.Fleet.ExaKeyRef = "secret://EXA_API_KEY"
	return cfg
}

func TestConfig_Validate_AcceptsWebProviders(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*ResearchConfig)
	}{
		{"defaults", func(*ResearchConfig) {}},
		{"explicit defaults", func(c *ResearchConfig) { c.Fleet.SearchProvider, c.Fleet.FetchProvider = SearchMCP, FetchDirect }},
		{"exa for both roles", func(c *ResearchConfig) { *c = validExa() }},
		{"exa search with the objective", func(c *ResearchConfig) { *c = validExa(); c.Fleet.SearchSendObjective = true }},
		{"exa fetch beside mcp search", func(c *ResearchConfig) {
			c.Fleet.FetchProvider = FetchExa
			c.Fleet.ExaEndpoint, c.Fleet.ExaKeyRef = "https://api.exa.ai", "secret://EXA_API_KEY"
		}},
		{"exa fetch at the character limit", func(c *ResearchConfig) { *c = validExa(); c.Fleet.MaxPageBytes = 1_000_000 }},
		{"exa endpoint left to the environment", func(c *ResearchConfig) { *c = validExa(); c.Fleet.ExaEndpoint = "" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			cfg := validWorker()
			tt.mutate(&cfg)
			must.NoError(cfg.Validate())
		})
	}
}

func TestConfig_Validate_RejectsWebProviders(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*ResearchConfig)
		want   string
	}{
		{"unknown search provider", func(c *ResearchConfig) { c.Fleet.SearchProvider = "brave" }, "fleet.search_provider"},
		{"unknown fetch provider", func(c *ResearchConfig) { c.Fleet.FetchProvider = "firecrawl" }, "fleet.fetch_provider"},
		{"search endpoint with exa search", func(c *ResearchConfig) { c.Fleet.SearchEndpoint = "https://search.example" }, "fleet.search_endpoint: applies to search provider"},
		{"search key with exa search", func(c *ResearchConfig) { c.Fleet.SearchKeyRef = "secret://SEARCH" }, "fleet.search_key_ref: applies to search provider"},
		{"search tool with exa search", func(c *ResearchConfig) { c.Fleet.SearchTool = "web_search" }, "fleet.search_tool: applies to search provider"},
		{"query arg with exa search", func(c *ResearchConfig) { c.Fleet.SearchQueryArg = "q" }, "fleet.search_query_arg: applies to search provider"},
		{"objective with mcp search", func(c *ResearchConfig) {
			*c = validWorker()
			c.Fleet.SearchSendObjective = true
		}, "fleet.search_send_objective"},
		{"exa endpoint without exa", func(c *ResearchConfig) {
			*c = validWorker()
			c.Fleet.ExaEndpoint = "https://api.exa.ai"
		}, "fleet.exa_endpoint: is set but neither"},
		{"exa key without exa", func(c *ResearchConfig) {
			*c = validWorker()
			c.Fleet.ExaKeyRef = "secret://EXA_API_KEY"
		}, "fleet.exa_key_ref: is set but neither"},
		{"cleartext exa endpoint", func(c *ResearchConfig) { c.Fleet.ExaEndpoint = "http://api.exa.ai" }, "fleet.exa_endpoint"},
		{"literal exa key", func(c *ResearchConfig) { c.Fleet.ExaKeyRef = "exa-literal-key" }, "fleet.exa_key_ref: is not a secret:// reference"},
		{"page bound over exa's limit", func(c *ResearchConfig) { c.Fleet.MaxPageBytes = 1_000_001 }, "fleet.max_page_bytes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			cfg := validExa()
			tt.mutate(&cfg)
			err := cfg.Validate()
			must.ErrorContains(err, tt.want)
			must.NotContains(err.Error(), "exa-literal-key")
		})
	}
}

// TestWebFlags_Apply_SetsFieldsAndOmitsUnsetKeys: each web provider flag writes
// its field, and a config that sets none encodes without the new keys, so
// research-config output is unchanged for existing pipelines.
func TestWebFlags_Apply_SetsFieldsAndOmitsUnsetKeys(t *testing.T) {
	must := require.New(t)
	cfg := Default()
	fs := newFlagSet(t)
	must.NoError(fs.Parse([]string{
		"--fleet-search-provider", "exa", "--fleet-fetch-provider", "exa",
		"--fleet-exa-endpoint", "https://api.exa.ai", "--fleet-exa-key-ref", "secret://EXA",
		"--fleet-search-send-objective",
	}))
	must.NoError(ApplyFlags(&cfg, fs))

	f := cfg.Fleet
	must.Equal(SearchExa, f.SearchProvider)
	must.Equal(FetchExa, f.FetchProvider)
	must.Equal("https://api.exa.ai", f.ExaEndpoint)
	must.Equal("secret://EXA", f.ExaKeyRef)
	must.True(f.SearchSendObjective)

	var buf bytes.Buffer
	must.NoError(Default().EncodeJSON(&buf))
	for _, key := range []string{"search_provider", "fetch_provider", "exa_endpoint", "exa_key_ref", "search_send_objective"} {
		t.Run(key, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(buf.String(), key)
		})
	}
}

// TestFleetEndpoints_Needed_FollowsProviderChoice: each endpoint variable is
// read only when the configuration uses its endpoint.
func TestFleetEndpoints_Needed_FollowsProviderChoice(t *testing.T) {
	needed := func(f FleetConfig) map[string]bool {
		out := map[string]bool{}
		for _, e := range FleetEndpoints(&f) {
			out[e.Field] = e.Needed == nil || e.Needed(&f)
		}
		return out
	}
	for _, tt := range []struct {
		name string
		f    FleetConfig
		want map[string]bool
	}{
		{"defaults", FleetConfig{}, map[string]bool{
			"fleet.model_endpoint": true, "fleet.search_endpoint": true, "fleet.knowledge_endpoint": false, "fleet.exa_endpoint": false}},
		{"exa search", FleetConfig{SearchProvider: SearchExa}, map[string]bool{
			"fleet.model_endpoint": true, "fleet.search_endpoint": false, "fleet.knowledge_endpoint": false, "fleet.exa_endpoint": true}},
		{"exa fetch with knowledge", FleetConfig{FetchProvider: FetchExa, KnowledgeProvider: KnowledgeBillet}, map[string]bool{
			"fleet.model_endpoint": true, "fleet.search_endpoint": true, "fleet.knowledge_endpoint": true, "fleet.exa_endpoint": true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got := needed(tt.f)
			for field, want := range tt.want {
				must.Equal(want, got[field], "%s needed", field)
			}
		})
	}
}

func TestDecodeBase_RejectsExaEndpoint(t *testing.T) {
	must := require.New(t)
	_, err := DecodeBase(strings.NewReader("agent: worker\nfleet:\n  search_provider: exa\n  exa_endpoint: https://exa.example\n  exa_key_ref: secret://EXA\n"))
	must.ErrorContains(err, "fleet.exa_endpoint:")
	must.ErrorContains(err, EnvFleetExaEndpoint)
	must.NotContains(err.Error(), "exa.example")
}

// TestDecodeBase_RefusesSendObjective: a shared config cannot choose to send
// the research question to a search provider; only the flag can.
func TestDecodeBase_RefusesSendObjective(t *testing.T) {
	must := require.New(t)
	_, err := DecodeBase(strings.NewReader("agent: worker\nfleet:\n  search_provider: exa\n  search_send_objective: true\n"))
	must.ErrorContains(err, "fleet.search_send_objective: must not come from a base config")
	must.ErrorContains(err, "--"+FlagSearchSendObjective)

	_, err = DecodeBase(strings.NewReader("agent: worker\nfleet:\n  search_provider: exa\n  search_send_objective: false\n"))
	must.NoError(err, "a base leaving the objective off was refused")
}
