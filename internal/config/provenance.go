package config

import (
	"fmt"
	"io"
)

// Environment variables that supply a fleet endpoint whose flag is unset.
// Only the composition root reads them; config names them in its errors.
const (
	EnvFleetModelEndpoint     = "CHIRON_FLEET_MODEL_ENDPOINT"
	EnvFleetSearchEndpoint    = "CHIRON_FLEET_SEARCH_ENDPOINT"
	EnvFleetKnowledgeEndpoint = "CHIRON_FLEET_KNOWLEDGE_ENDPOINT"
)

// FleetEndpoint is one credential-bearing fleet endpoint with the flag and
// environment variable that may supply it. A base config may never: whoever
// controls a shared config could otherwise choose both a key reference and
// the host that key is sent to.
type FleetEndpoint struct {
	// Field is the config key, such as fleet.model_endpoint.
	Field string
	// Flag is the flag name without its dashes, such as fleet-model-endpoint.
	Flag string
	// Env is the environment variable, such as CHIRON_FLEET_MODEL_ENDPOINT.
	Env string
	// Value points at the field in the FleetConfig passed to FleetEndpoints.
	Value *string
}

// FleetEndpoints returns f's credential-bearing endpoints: model, search and
// knowledge, in that order.
func FleetEndpoints(f *FleetConfig) []FleetEndpoint {
	return []FleetEndpoint{
		{Field: "fleet.model_endpoint", Flag: "fleet-model-endpoint", Env: EnvFleetModelEndpoint, Value: &f.ModelEndpoint},
		{Field: "fleet.search_endpoint", Flag: "fleet-search-endpoint", Env: EnvFleetSearchEndpoint, Value: &f.SearchEndpoint},
		{Field: "fleet.knowledge_endpoint", Flag: "fleet-knowledge-endpoint", Env: EnvFleetKnowledgeEndpoint, Value: &f.KnowledgeEndpoint},
	}
}

// DecodeBase strictly decodes a base config (a --config file or piped stdin,
// JSON or YAML) over the defaults and refuses one naming any fleet endpoint,
// whatever its agent. Flags overlay the base afterwards, so a flag setting
// the same endpoint cannot rescue it. The error never echoes the value.
func DecodeBase(r io.Reader) (ResearchConfig, error) {
	cfg, err := decode(r)
	if err != nil {
		return ResearchConfig{}, err
	}
	for _, e := range FleetEndpoints(&cfg.Fleet) {
		if *e.Value != "" {
			return ResearchConfig{}, fmt.Errorf("%s: must not come from a base config (--config or piped stdin), because a shared config must not choose where credentials are sent; pass --%s or set %s instead",
				e.Field, e.Flag, e.Env)
		}
	}
	return cfg, nil
}
