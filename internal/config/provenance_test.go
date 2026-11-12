package config

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDecodeBase_RejectsFleetEndpoints: a base config naming any fleet
// endpoint beside its key reference is refused at load time, whatever its
// agent, with an error that names the field, the reason and the flag and
// environment variable to use instead, and never echoes the value.
func TestDecodeBase_RejectsFleetEndpoints(t *testing.T) {
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
			must := require.New(t)
			_, err := DecodeBase(strings.NewReader(tt.base))
			must.Error(err, "a base config naming an endpoint was accepted")
			for _, want := range []string{tt.field + ":", "base config", "credentials are sent", tt.flag, tt.env} {
				must.ErrorContains(err, want)
			}
			must.NotContains(err.Error(), token, "error echoes the endpoint")
			must.NotContains(err.Error(), ".example", "error echoes the endpoint")
		})
	}
}

// TestDecodeBase_AcceptsKeyRefsWithoutEndpoints: a base may still carry the
// key references and every other fleet field; only the endpoints move.
func TestDecodeBase_AcceptsKeyRefsWithoutEndpoints(t *testing.T) {
	must := require.New(t)
	const base = "agent: worker\nfleet:\n  model_name: gpt-5.5\n  model_key_ref: secret://MODEL_KEY\n  search_key_ref: secret://SEARCH_KEY\n" +
		"  knowledge_provider: alexandria\n  knowledge_key_ref: secret://KB_KEY\n  max_turns: 4\n"
	cfg, err := DecodeBase(strings.NewReader(base))
	must.NoError(err)

	f := cfg.Fleet
	must.Equal("secret://MODEL_KEY", f.ModelKeyRef)
	must.Equal("secret://SEARCH_KEY", f.SearchKeyRef)
	must.Equal("secret://KB_KEY", f.KnowledgeKeyRef)
	must.Equal(4, f.MaxTurns)
}

// TestDecodeBase_DecodeErrorsAndDefaults: a malformed base still fails as a
// decode error, and an empty one still yields the defaults.
func TestDecodeBase_DecodeErrorsAndDefaults(t *testing.T) {
	t.Run("malformed base fails to decode", func(t *testing.T) {
		must := require.New(t)
		_, err := DecodeBase(strings.NewReader("agnet: worker\n"))
		must.ErrorContains(err, "decode research config")
	})

	t.Run("empty base yields defaults", func(t *testing.T) {
		must := require.New(t)
		cfg, err := DecodeBase(strings.NewReader(""))
		must.NoError(err)
		must.Equal(AgentDeepResearch, cfg.Agent)
	})
}

// TestFleetEndpoints_MatchFlagsAndKeys pins the provenance table to the flag
// surface and the wire keys: each entry's flag writes its Value, and its
// Field names the key the value encodes under.
func TestFleetEndpoints_MatchFlagsAndKeys(t *testing.T) {
	must := require.New(t)
	var probe FleetConfig
	endpoints := FleetEndpoints(&probe)
	must.Len(endpoints, 4, "want model, search, knowledge and exa entries")

	for i, tt := range endpoints {
		t.Run(tt.Flag, func(t *testing.T) {
			must := require.New(t)
			cfg := Default()
			fs := newFlagSet(t)
			must.NoError(fs.Parse([]string{"--" + tt.Flag, "https://flag.example"}))
			must.NoError(ApplyFlags(&cfg, fs))
			must.Equal("https://flag.example", *FleetEndpoints(&cfg.Fleet)[i].Value)

			var buf bytes.Buffer
			must.NoError(cfg.EncodeJSON(&buf))
			key := `"` + strings.TrimPrefix(tt.Field, "fleet.") + `": "https://flag.example"`
			must.Contains(buf.String(), key)

			must.True(strings.HasPrefix(tt.Env, "CHIRON_FLEET_"), "Env = %q, want the CHIRON_FLEET_ namespace", tt.Env)
		})
	}
}

// TestFleetEndpoints_CoverEveryDestinationField: every FleetConfig field whose
// name or key suggests a destination is in FleetEndpoints, so DecodeBase
// refuses it in a base config and the environment fallback can supply it.
func TestFleetEndpoints_CoverEveryDestinationField(t *testing.T) {
	must := require.New(t)
	covered := map[string]bool{}
	for _, e := range FleetEndpoints(&FleetConfig{}) {
		covered[strings.TrimPrefix(e.Field, "fleet.")] = true
	}
	namesDestination := func(s string) bool {
		s = strings.ToLower(s)
		return strings.Contains(s, "endpoint") || strings.Contains(s, "url") || strings.Contains(s, "host")
	}

	var uncovered []string
	ty := reflect.TypeOf(FleetConfig{})
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		key := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if (namesDestination(f.Name) || namesDestination(key)) && !covered[key] {
			uncovered = append(uncovered, fmt.Sprintf("FleetConfig.%s (fleet.%s)", f.Name, key))
		}
	}
	must.Empty(uncovered, "destination fields missing from FleetEndpoints")
}
