package config

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withKnowledge returns validWorker with the given provider and endpoint.
func withKnowledge(provider, endpoint string) ResearchConfig {
	cfg := validWorker()
	cfg.Fleet.KnowledgeProvider = provider
	cfg.Fleet.KnowledgeEndpoint = endpoint
	return cfg
}

func TestKnowledge_Validate_FieldRules(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mutate  func(*ResearchConfig)
		wantErr string
	}{
		{"no provider, defaults", func(c *ResearchConfig) {}, ""},
		{"no provider, zero limit", func(c *ResearchConfig) { c.Fleet.KnowledgeLimit = 0 }, ""},
		{"billet keyless", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeBillet, "http://127.0.0.1:8140/")
		}, ""},
		{"billet with key", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeBillet, "https://billet.internal/")
			c.Fleet.KnowledgeKeyRef = "secret://BILLET_KEY"
		}, ""},
		{"alexandria with space and limit bounds", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeAlexandria, "https://alexandria.example")
			c.Fleet.KnowledgeKeyRef = "secret://ALEXANDRIA_KEY"
			c.Fleet.KnowledgeSpace = "team-notes-2"
			c.Fleet.KnowledgeLimit = MaxKnowledgeLimit
		}, ""},
		{"limit one", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeBillet, "https://billet.internal/")
			c.Fleet.KnowledgeLimit = 1
		}, ""},
		{"provider enum", func(c *ResearchConfig) {
			*c = withKnowledge("paddock", "https://billet.internal/")
		}, "fleet.knowledge_provider:"},
		{"stray endpoint", func(c *ResearchConfig) { c.Fleet.KnowledgeEndpoint = "https://billet.internal/" }, "fleet.knowledge_endpoint: is set but"},
		{"stray key ref", func(c *ResearchConfig) { c.Fleet.KnowledgeKeyRef = "secret://K" }, "fleet.knowledge_key_ref: is set but"},
		{"stray space", func(c *ResearchConfig) { c.Fleet.KnowledgeSpace = "notes" }, "fleet.knowledge_space: is set but"},
		{"stray limit", func(c *ResearchConfig) { c.Fleet.KnowledgeLimit = 7 }, "fleet.knowledge_limit: is set but"},
		{"stray remember", func(c *ResearchConfig) { c.Fleet.KnowledgeRemember = true }, "fleet.knowledge_remember: is set but"},
		{"endpoint cleartext non-loopback", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeBillet, "http://billet.internal/")
		}, "fleet.knowledge_endpoint:"},
		{"endpoint metadata", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeBillet, "http://169.254.169.254/")
		}, "fleet.knowledge_endpoint:"},
		{"endpoint userinfo", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeAlexandria, "https://u:p@alexandria.example")
		}, "fleet.knowledge_endpoint:"},
		{"endpoint query", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeAlexandria, "https://alexandria.example/?token=x")
		}, "fleet.knowledge_endpoint:"},
		{"key literal", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeBillet, "https://billet.internal/")
			c.Fleet.KnowledgeKeyRef = "alx_literal"
		}, "fleet.knowledge_key_ref:"},
		{"space with billet", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeBillet, "https://billet.internal/")
			c.Fleet.KnowledgeSpace = "notes"
		}, "fleet.knowledge_space: applies to"},
		{"space upper case", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeAlexandria, "https://alexandria.example")
			c.Fleet.KnowledgeSpace = "Notes"
		}, "fleet.knowledge_space:"},
		{"space leading hyphen", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeAlexandria, "https://alexandria.example")
			c.Fleet.KnowledgeSpace = "-notes"
		}, "fleet.knowledge_space:"},
		{"space trailing hyphen", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeAlexandria, "https://alexandria.example")
			c.Fleet.KnowledgeSpace = "notes-"
		}, "fleet.knowledge_space:"},
		{"space underscore", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeAlexandria, "https://alexandria.example")
			c.Fleet.KnowledgeSpace = "team_notes"
		}, "fleet.knowledge_space:"},
		{"space too long", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeAlexandria, "https://alexandria.example")
			c.Fleet.KnowledgeSpace = strings.Repeat("a", maxKnowledgeSpaceLen+1)
		}, "fleet.knowledge_space:"},
		{"limit zero with provider", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeBillet, "https://billet.internal/")
			c.Fleet.KnowledgeLimit = 0
		}, "fleet.knowledge_limit:"},
		{"limit above max", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeBillet, "https://billet.internal/")
			c.Fleet.KnowledgeLimit = MaxKnowledgeLimit + 1
		}, "fleet.knowledge_limit:"},
		{"remember with alexandria", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeAlexandria, "https://alexandria.example")
			c.Fleet.KnowledgeRemember = true
		}, "fleet.knowledge_remember:"},
	} {
		for _, agent := range []string{AgentWorker, AgentFleet} {
			t.Run(tt.name+"/"+agent, func(t *testing.T) {
				must := require.New(t)
				cfg := validWorker()
				tt.mutate(&cfg)
				cfg = withAgent(cfg, agent)
				err := cfg.Validate()
				if tt.wantErr == "" {
					must.NoError(err)
					return
				}
				must.Error(err)
				must.True(strings.HasPrefix(err.Error(), tt.wantErr),
					"Validate = %v, want an error starting %q", err, tt.wantErr)
			})
		}
	}
}

// TestKnowledge_Validate_RememberIsWorkerOnly: Billet save-back is valid for
// the single worker and refused for the fleet, whose workers never save a
// finding back, with an error that names the field, the flag and the agent
// that can.
func TestKnowledge_Validate_RememberIsWorkerOnly(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	remember := withKnowledge(KnowledgeBillet, "https://billet.internal/")
	remember.Fleet.KnowledgeRemember = true

	must.NoError(withAgent(remember, AgentWorker).Validate(), "worker with knowledge_remember")
	err := withAgent(remember, AgentFleet).Validate()
	must.Error(err, "fleet with knowledge_remember validated, want it refused")
	for _, want := range []string{"fleet.knowledge_remember:", "--fleet-knowledge-remember", `"worker"`} {
		check.ErrorContains(err, want)
	}
}

// TestKnowledge_Validate_IgnoredForDeepResearch: the knowledge fields belong to
// the fleet block, which the deep-research tiers never validate.
func TestKnowledge_Validate_IgnoredForDeepResearch(t *testing.T) {
	must := require.New(t)

	cfg := Default()
	cfg.Fleet.KnowledgeEndpoint = "https://billet.internal/"
	must.NoError(cfg.Validate(), "deep-research with a stray knowledge field")
}

// TestKnowledge_Validate_NeverEchoesSecrets: a literal key or an endpoint's
// userinfo never appears in a validation error.
func TestKnowledge_Validate_NeverEchoesSecrets(t *testing.T) {
	const literal = "alx_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6"
	for _, tt := range []struct {
		name   string
		mutate func(*ResearchConfig)
	}{
		{"key literal", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeAlexandria, "https://alexandria.example")
			c.Fleet.KnowledgeKeyRef = literal
		}},
		{"stray key literal", func(c *ResearchConfig) { c.Fleet.KnowledgeKeyRef = literal }},
		{"endpoint userinfo", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeAlexandria, "https://user:"+literal+"@alexandria.example")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			cfg := validWorker()
			tt.mutate(&cfg)
			err := cfg.Validate()
			must.Error(err)
			must.NotContains(err.Error(), literal, "validation error leaked the literal")
		})
	}
}

// TestKnowledge_RoundTrip_JSONAndYAML: the knowledge fields survive EncodeJSON
// -> decode under their documented keys, so a pipeline stage cannot drop one.
func TestKnowledge_RoundTrip_JSONAndYAML(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	in := withKnowledge(KnowledgeAlexandria, "https://alexandria.example")
	in.Fleet.KnowledgeKeyRef = "secret://ALEXANDRIA_KEY"
	in.Fleet.KnowledgeSpace = "notes"
	in.Fleet.KnowledgeLimit = 9
	in.Fleet.KnowledgeRemember = true

	var buf bytes.Buffer
	must.NoError(in.EncodeJSON(&buf))
	for _, key := range []string{`"knowledge_provider"`, `"knowledge_endpoint"`, `"knowledge_key_ref"`, `"knowledge_space"`, `"knowledge_limit"`, `"knowledge_remember"`} {
		check.Contains(buf.String(), key, "encoded config lacks %s:\n%s", key, buf.String())
	}
	out, err := decode(&buf)
	must.NoError(err)
	must.Equal(in.Fleet, out.Fleet, "round trip lost data")

	yamlIn := "agent: worker\nfleet:\n  knowledge_provider: billet\n  knowledge_endpoint: http://127.0.0.1:8140/\n  knowledge_remember: true\n"
	cfg, err := decode(strings.NewReader(yamlIn))
	must.NoError(err)
	f := cfg.Fleet
	must.Equal(KnowledgeBillet, f.KnowledgeProvider, "YAML knowledge_provider")
	must.True(f.KnowledgeRemember, "YAML knowledge_remember")
	must.Equal(DefaultKnowledgeLimit, f.KnowledgeLimit, "YAML knowledge_limit default")
}

// TestValidate_MCPLever_HintNamesKnowledge: the rejected mcp lever points at
// both replacements for the in-process agents.
func TestValidate_MCPLever_HintNamesKnowledge(t *testing.T) {
	must := require.New(t)

	cfg := validWorker()
	cfg.MCP = map[string]string{"billet": "https://billet.internal/"}
	must.ErrorContains(cfg.Validate(), "use fleet.search_endpoint or the fleet.knowledge_* fields")
}
