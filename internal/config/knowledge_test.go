package config

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// withKnowledge returns validWorker with the given provider and endpoint.
func withKnowledge(provider, endpoint string) ResearchConfig {
	cfg := validWorker()
	cfg.Fleet.KnowledgeProvider = provider
	cfg.Fleet.KnowledgeEndpoint = endpoint
	return cfg
}

func TestValidateKnowledge(t *testing.T) {
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
		{"billet with key and remember", func(c *ResearchConfig) {
			*c = withKnowledge(KnowledgeBillet, "https://billet.internal/")
			c.Fleet.KnowledgeKeyRef = "secret://BILLET_KEY"
			c.Fleet.KnowledgeRemember = true
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
				cfg := validWorker()
				tt.mutate(&cfg)
				cfg.Agent = agent
				err := cfg.Validate()
				if tt.wantErr == "" {
					if err != nil {
						t.Errorf("Validate: %v", err)
					}
					return
				}
				if err == nil || !strings.HasPrefix(err.Error(), tt.wantErr) {
					t.Errorf("Validate = %v, want an error starting %q", err, tt.wantErr)
				}
			})
		}
	}
}

// TestValidateKnowledgeIgnoredForDeepResearch: the knowledge fields belong to
// the fleet block, which the deep-research tiers never validate.
func TestValidateKnowledgeIgnoredForDeepResearch(t *testing.T) {
	cfg := Default()
	cfg.Fleet.KnowledgeEndpoint = "https://billet.internal/"
	if err := cfg.Validate(); err != nil {
		t.Errorf("deep-research with a stray knowledge field: %v", err)
	}
}

// TestValidateKnowledgeNeverEchoesSecrets: a literal key or an endpoint's
// userinfo never appears in a validation error.
func TestValidateKnowledgeNeverEchoesSecrets(t *testing.T) {
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
			cfg := validWorker()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("validated")
			}
			if strings.Contains(err.Error(), literal) {
				t.Errorf("validation error leaked the literal: %v", err)
			}
		})
	}
}

// TestKnowledgeRoundTrip: the knowledge fields survive EncodeJSON -> decode
// under their documented keys, so a pipeline stage cannot drop one.
func TestKnowledgeRoundTrip(t *testing.T) {
	in := withKnowledge(KnowledgeAlexandria, "https://alexandria.example")
	in.Fleet.KnowledgeKeyRef = "secret://ALEXANDRIA_KEY"
	in.Fleet.KnowledgeSpace = "notes"
	in.Fleet.KnowledgeLimit = 9
	in.Fleet.KnowledgeRemember = true

	var buf bytes.Buffer
	if err := in.EncodeJSON(&buf); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	for _, key := range []string{`"knowledge_provider"`, `"knowledge_endpoint"`, `"knowledge_key_ref"`, `"knowledge_space"`, `"knowledge_limit"`, `"knowledge_remember"`} {
		if !strings.Contains(buf.String(), key) {
			t.Errorf("encoded config lacks %s:\n%s", key, buf.String())
		}
	}
	out, err := decode(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(out.Fleet, in.Fleet) {
		t.Errorf("round trip lost data:\n got %+v\nwant %+v", out.Fleet, in.Fleet)
	}

	yamlIn := "agent: worker\nfleet:\n  knowledge_provider: billet\n  knowledge_endpoint: http://127.0.0.1:8140/\n  knowledge_remember: true\n"
	cfg, err := decode(strings.NewReader(yamlIn))
	if err != nil {
		t.Fatalf("decode YAML: %v", err)
	}
	if f := cfg.Fleet; f.KnowledgeProvider != KnowledgeBillet || !f.KnowledgeRemember || f.KnowledgeLimit != DefaultKnowledgeLimit {
		t.Errorf("YAML knowledge block = %+v, want billet with remember and the default limit", f)
	}
}

// TestMCPLeverHintNamesKnowledge: the rejected mcp lever points at both
// replacements for the in-process agents.
func TestMCPLeverHintNamesKnowledge(t *testing.T) {
	cfg := validWorker()
	cfg.MCP = map[string]string{"billet": "https://billet.internal/"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "use fleet.search_endpoint or the fleet.knowledge_* fields") {
		t.Errorf("Validate = %v, want the knowledge hint", err)
	}
}
