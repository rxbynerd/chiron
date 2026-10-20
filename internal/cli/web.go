package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/fetch"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa"
	"github.com/rxbynerd/chiron/internal/secret"
)

// requireWeb checks the web provider fields the composition root needs
// before anything is resolved or dialled: a role served by Exa needs the Exa
// endpoint and key. The MCP search endpoint is required with the other
// endpoints. Config validation has already checked their form.
func requireWeb(fc config.FleetConfig) error {
	if !fc.UsesExa() {
		return nil
	}
	if fc.ExaEndpoint == "" {
		return fmt.Errorf("research: fleet.exa_endpoint is required when a web role uses Exa; pass --fleet-exa-endpoint %s or set %s",
			exa.DefaultEndpoint, config.EnvFleetExaEndpoint)
	}
	if fc.ExaKeyRef == "" {
		return errors.New("research: fleet.exa_key_ref is required when a web role uses Exa")
	}
	return nil
}

// buildWeb builds the web search and fetch providers the configuration
// selects, with the closer that ends the MCP search session (nil when search
// is not MCP). Every key reference it needs is resolved before any client is
// built. An MCP search call is bounded by callTimeout; an Exa call, either
// role, and a direct fetch by fetchTimeout. CHIRON_FETCH_ALLOW_LOOPBACK is
// read only when Chiron fetches pages itself.
//
// When both roles use Exa they share one client, so a per-provider limit
// such as a rate limiter applies across both.
func buildWeb(ctx context.Context, fc config.FleetConfig, resolver secret.Resolver, callTimeout, fetchTimeout time.Duration) (web.Searcher, web.Fetcher, io.Closer, error) {
	if err := requireWeb(fc); err != nil {
		return nil, nil, nil, err
	}
	var exaKey, searchKey string
	var err error
	if fc.UsesExa() {
		if exaKey, err = resolver.Resolve(ctx, fc.ExaKeyRef); err != nil {
			return nil, nil, nil, err
		}
	}
	// An MCP search server may be keyless; its key is resolved only when a
	// reference is set, so an empty one sends no Authorization header.
	if fc.SearchProviderOrDefault() == config.SearchMCP && fc.SearchKeyRef != "" {
		if searchKey, err = resolver.Resolve(ctx, fc.SearchKeyRef); err != nil {
			return nil, nil, nil, err
		}
	}
	allowLoopback := false
	if fc.FetchProviderOrDefault() == config.FetchDirect {
		if allowLoopback, err = fetchAllowLoopbackFromEnv(); err != nil {
			return nil, nil, nil, err
		}
	}

	var exaClient *exa.Client
	if fc.UsesExa() {
		// fleet.max_page_bytes bounds the text in bytes after the worker
		// reduces it; Exa bounds it in characters, which is never fewer.
		exaClient, err = exa.New(exa.Options{
			Endpoint:       fc.ExaEndpoint,
			APIKey:         exaKey,
			RequestTimeout: fetchTimeout,
			MaxCharacters:  fc.MaxPageBytes,
			SendObjective:  fc.SearchSendObjective,
		})
		if err != nil {
			return nil, nil, nil, err
		}
	}

	var fetcher web.Fetcher
	switch fc.FetchProviderOrDefault() {
	case config.FetchExa:
		fetcher = exaClient
	default:
		direct, err := fetch.New(fetch.Options{
			RequestTimeout:  fetchTimeout,
			MaxContentBytes: fetchMaxContentBytes,
			AllowLoopback:   allowLoopback,
		})
		if err != nil {
			return nil, nil, nil, err
		}
		fetcher = direct
	}

	switch fc.SearchProviderOrDefault() {
	case config.SearchExa:
		return exaClient, fetcher, nil, nil
	default:
		sc, err := search.New(search.Options{
			Endpoint:       fc.SearchEndpoint,
			APIKey:         searchKey,
			RequestTimeout: callTimeout,
			ToolName:       fc.SearchTool,
			QueryArgKey:    fc.SearchQueryArg,
		})
		if err != nil {
			return nil, nil, nil, err
		}
		return sc, fetcher, sc, nil
	}
}

// webProviderName is the provider recorded on the Interaction's tool list
// for a role: empty for the role's default, else the provider's name.
func webProviderName(provider, def string) string {
	if provider == def {
		return ""
	}
	return provider
}
