package config

import (
	"bytes"
	"strings"
	"testing"
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

func TestValidateWebAccepts(t *testing.T) {
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
			cfg := validWorker()
			tt.mutate(&cfg)
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

func TestValidateWebRejects(t *testing.T) {
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
			cfg := validExa()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "exa-literal-key") {
				t.Errorf("error echoes the literal key: %v", err)
			}
		})
	}
}

// TestWebFlagsApply: each web provider flag writes its field, and a config
// that sets none encodes without the new keys, so research-config output is
// unchanged for existing pipelines.
func TestWebFlagsApply(t *testing.T) {
	cfg := Default()
	fs := newFlagSet(t)
	if err := fs.Parse([]string{
		"--fleet-search-provider", "exa", "--fleet-fetch-provider", "exa",
		"--fleet-exa-endpoint", "https://api.exa.ai", "--fleet-exa-key-ref", "secret://EXA",
		"--fleet-search-send-objective",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ApplyFlags(&cfg, fs); err != nil {
		t.Fatalf("ApplyFlags: %v", err)
	}
	f := cfg.Fleet
	if f.SearchProvider != SearchExa || f.FetchProvider != FetchExa || f.ExaEndpoint != "https://api.exa.ai" ||
		f.ExaKeyRef != "secret://EXA" || !f.SearchSendObjective {
		t.Errorf("flags not applied: %+v", f)
	}

	var buf bytes.Buffer
	if err := Default().EncodeJSON(&buf); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	for _, key := range []string{"search_provider", "fetch_provider", "exa_endpoint", "exa_key_ref", "search_send_objective"} {
		if strings.Contains(buf.String(), key) {
			t.Errorf("default config encodes %s:\n%s", key, buf.String())
		}
	}
}

// TestFleetEndpointsNeeded: each endpoint variable is read only when the
// configuration uses its endpoint.
func TestFleetEndpointsNeeded(t *testing.T) {
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
			got := needed(tt.f)
			for field, want := range tt.want {
				if got[field] != want {
					t.Errorf("%s needed = %v, want %v", field, got[field], want)
				}
			}
		})
	}
}

func TestDecodeBaseRejectsExaEndpoint(t *testing.T) {
	_, err := DecodeBase(strings.NewReader("agent: worker\nfleet:\n  search_provider: exa\n  exa_endpoint: https://exa.example\n  exa_key_ref: secret://EXA\n"))
	if err == nil || !strings.Contains(err.Error(), "fleet.exa_endpoint:") || !strings.Contains(err.Error(), EnvFleetExaEndpoint) {
		t.Fatalf("err = %v, want the exa endpoint refused with its variable named", err)
	}
	if strings.Contains(err.Error(), "exa.example") {
		t.Errorf("error echoes the endpoint: %v", err)
	}
}
