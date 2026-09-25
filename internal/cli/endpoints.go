package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/httpx"
	"github.com/rxbynerd/chiron/internal/transport"
)

// applyEndpointEnv fills each fleet endpoint whose flag is unset from its
// CHIRON_FLEET_*_ENDPOINT variable; a set flag wins. The knowledge variable
// is read only with a knowledge provider. Errors name the variable, never
// the value.
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

// endpointsPayload is the delta event naming where a worker run sends its
// credentials, by scheme and host only: a path or query can carry a token.
type endpointsPayload struct {
	Endpoints endpointOrigins `json:"endpoints"`
}

// endpointOrigins holds scheme://host for each destination. Knowledge is
// omitted without a knowledge provider.
type endpointOrigins struct {
	Model     string `json:"model"`
	Search    string `json:"search"`
	Knowledge string `json:"knowledge,omitempty"`
}

// emitEndpoints emits the one endpoints event of a worker run, so its
// destinations are visible on stderr before the first credential-bearing
// request. Emission is best effort: a broken event stream must not abort a
// paid run.
func emitEndpoints(ctx context.Context, tr transport.Transport, fc config.FleetConfig) {
	origins := endpointOrigins{Model: origin(fc.ModelEndpoint), Search: origin(fc.SearchEndpoint)}
	if fc.KnowledgeProvider != "" {
		origins.Knowledge = origin(fc.KnowledgeEndpoint)
	}
	payload, err := json.Marshal(endpointsPayload{Endpoints: origins})
	if err != nil {
		return
	}
	_ = tr.Emit(ctx, transport.Event{Kind: transport.KindDelta, Payload: payload})
}

// origin reduces an endpoint that has already passed httpx.ParseEndpoint to
// scheme://host.
func origin(raw string) string {
	u, err := httpx.ParseEndpoint(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// refuseEndpointFlags stops research-config from emitting a fleet endpoint:
// its output is the next stage's base config, which config.DecodeBase
// refuses.
func refuseEndpointFlags(flags *pflag.FlagSet) error {
	for _, e := range config.FleetEndpoints(&config.FleetConfig{}) {
		if flags.Changed(e.Flag) {
			return fmt.Errorf("research-config: --%s cannot travel through a pipeline, because the next stage refuses a base config that chooses where credentials are sent; pass --%s to the final chiron research stage or set %s for it",
				e.Flag, e.Flag, e.Env)
		}
	}
	return nil
}
