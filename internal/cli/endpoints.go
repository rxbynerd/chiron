package cli

import (
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/httpx"
)

// applyEndpointEnv fills each fleet endpoint whose flag was not set from its
// CHIRON_FLEET_*_ENDPOINT variable, validated with httpx.ParseEndpoint; the
// error names the variable, never its value. An explicitly set flag wins over
// the variable. The knowledge variable is read only when a knowledge
// provider is configured, so a deployment may export it for runs that do not
// recall. A base config cannot carry an endpoint (config.DecodeBase), so
// afterwards every endpoint came from a flag or the environment.
func applyEndpointEnv(fc *config.FleetConfig, flags *pflag.FlagSet) error {
	for _, e := range config.FleetEndpoints(fc) {
		if flags.Changed(e.Flag) {
			continue
		}
		if e.Value == &fc.KnowledgeEndpoint && fc.KnowledgeProvider == "" {
			continue
		}
		raw := os.Getenv(e.Env)
		if raw == "" {
			continue
		}
		if _, err := httpx.ParseEndpoint(raw); err != nil {
			return fmt.Errorf("%s %w", e.Env, err)
		}
		*e.Value = raw
	}
	return nil
}
