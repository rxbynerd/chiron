package config

import (
	"bytes"
	"strings"
	"testing"
)

// TestDecodeBaseRejectsFleetEndpoints: a base config naming any fleet
// endpoint beside its key reference is refused at load time, whatever its
// agent, with an error that names the field, the reason and the flag and
// environment variable to use instead, and never echoes the value.
func TestDecodeBaseRejectsFleetEndpoints(t *testing.T) {
	const token = "sk-live-0123456789abcdefghijklmn"
	for _, tt := range []struct {
		name  string
		base  string
		field string
		flag  string
		env   string
	}{
		{"model", "agent: worker\nfleet:\n  model_endpoint: https://gateway.example/" + token + "\n  model_key_ref: secret://MODEL_KEY\n",
			"fleet.model_endpoint", "--fleet-model-endpoint", EnvFleetModelEndpoint},
		{"search", "agent: worker\nfleet:\n  search_endpoint: https://search.example/" + token + "\n  search_key_ref: secret://SEARCH_KEY\n",
			"fleet.search_endpoint", "--fleet-search-endpoint", EnvFleetSearchEndpoint},
		{"knowledge", "agent: worker\nfleet:\n  knowledge_provider: alexandria\n  knowledge_endpoint: https://kb.example/" + token + "\n  knowledge_key_ref: secret://KB_KEY\n",
			"fleet.knowledge_endpoint", "--fleet-knowledge-endpoint", EnvFleetKnowledgeEndpoint},
		{"json form", `{"agent":"worker","fleet":{"model_endpoint":"https://gateway.example/` + token + `","model_key_ref":"secret://MODEL_KEY"}}`,
			"fleet.model_endpoint", "--fleet-model-endpoint", EnvFleetModelEndpoint},
		{"deep-research agent", "agent: deep-research\nfleet:\n  model_endpoint: https://gateway.example/" + token + "\n  model_key_ref: secret://MODEL_KEY\n",
			"fleet.model_endpoint", "--fleet-model-endpoint", EnvFleetModelEndpoint},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeBase(strings.NewReader(tt.base))
			if err == nil {
				t.Fatal("a base config naming an endpoint was accepted")
			}
			for _, want := range []string{tt.field + ":", "base config", "credentials are sent", tt.flag, tt.env} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), ".example") {
				t.Errorf("error echoes the endpoint: %q", err)
			}
		})
	}
}

// TestDecodeBaseAcceptsKeyRefsWithoutEndpoints: a base may still carry the
// key references and every other fleet field; only the endpoints move.
func TestDecodeBaseAcceptsKeyRefsWithoutEndpoints(t *testing.T) {
	const base = "agent: worker\nfleet:\n  model_name: gpt-5.5\n  model_key_ref: secret://MODEL_KEY\n  search_key_ref: secret://SEARCH_KEY\n" +
		"  knowledge_provider: alexandria\n  knowledge_key_ref: secret://KB_KEY\n  max_turns: 4\n"
	cfg, err := DecodeBase(strings.NewReader(base))
	if err != nil {
		t.Fatalf("DecodeBase: %v", err)
	}
	f := cfg.Fleet
	if f.ModelKeyRef != "secret://MODEL_KEY" || f.SearchKeyRef != "secret://SEARCH_KEY" || f.KnowledgeKeyRef != "secret://KB_KEY" || f.MaxTurns != 4 {
		t.Errorf("base fields lost: %+v", f)
	}
}

// TestDecodeBaseKeepsDecodeErrors: a malformed base still fails as a decode
// error, and an empty one still yields the defaults.
func TestDecodeBaseKeepsDecodeErrors(t *testing.T) {
	if _, err := DecodeBase(strings.NewReader("agnet: worker\n")); err == nil || !strings.Contains(err.Error(), "decode research config") {
		t.Errorf("err = %v, want the decode error", err)
	}
	cfg, err := DecodeBase(strings.NewReader(""))
	if err != nil || cfg.Agent != AgentDeepResearch {
		t.Errorf("empty base = %+v, %v; want the defaults", cfg.Agent, err)
	}
}

// TestFleetEndpointsMatchFlagsAndKeys pins the provenance table to the flag
// surface and the wire keys: each entry's flag writes its Value, and its
// Field names the key the value encodes under.
func TestFleetEndpointsMatchFlagsAndKeys(t *testing.T) {
	var probe FleetConfig
	endpoints := FleetEndpoints(&probe)
	if len(endpoints) != 3 {
		t.Fatalf("FleetEndpoints returned %d entries, want model, search and knowledge", len(endpoints))
	}
	for i, e := range endpoints {
		t.Run(e.Flag, func(t *testing.T) {
			cfg := Default()
			fs := newFlagSet(t)
			if err := fs.Parse([]string{"--" + e.Flag, "https://flag.example"}); err != nil {
				t.Fatalf("parse --%s: %v", e.Flag, err)
			}
			if err := ApplyFlags(&cfg, fs); err != nil {
				t.Fatalf("ApplyFlags: %v", err)
			}
			if got := *FleetEndpoints(&cfg.Fleet)[i].Value; got != "https://flag.example" {
				t.Errorf("--%s wrote %q to the entry's Value", e.Flag, got)
			}

			var buf bytes.Buffer
			if err := cfg.EncodeJSON(&buf); err != nil {
				t.Fatalf("EncodeJSON: %v", err)
			}
			key := `"` + strings.TrimPrefix(e.Field, "fleet.") + `": "https://flag.example"`
			if !strings.Contains(buf.String(), key) {
				t.Errorf("encoded config lacks %s:\n%s", key, buf.String())
			}
			if !strings.HasPrefix(e.Env, "CHIRON_FLEET_") {
				t.Errorf("Env = %q, want the CHIRON_FLEET_ namespace", e.Env)
			}
		})
	}
}
