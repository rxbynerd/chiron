package config

import (
	"fmt"

	"github.com/spf13/pflag"
)

// Web providers for the in-process research agents' two web roles. Search
// defaults to an MCP search server (fleet.search_endpoint) and fetch to
// Chiron's own SSRF-guarded client; exa serves either role, or both,
// through Exa's REST API. An empty value selects the default.
const (
	SearchMCP   = "mcp"
	SearchExa   = "exa"
	FetchDirect = "direct"
	FetchExa    = "exa"
)

// EnvFleetExaEndpoint supplies fleet.exa_endpoint when its flag is unset.
const EnvFleetExaEndpoint = "CHIRON_FLEET_EXA_ENDPOINT"

// maxExaPageCharacters is Exa's limit on one page's text, in characters.
const maxExaPageCharacters = 1_000_000

// SearchProviderOrDefault returns the search provider, SearchMCP when unset.
func (f FleetConfig) SearchProviderOrDefault() string {
	if f.SearchProvider == "" {
		return SearchMCP
	}
	return f.SearchProvider
}

// FetchProviderOrDefault returns the fetch provider, FetchDirect when unset.
func (f FleetConfig) FetchProviderOrDefault() string {
	if f.FetchProvider == "" {
		return FetchDirect
	}
	return f.FetchProvider
}

// UsesExa reports whether either web role is served by Exa, which is when
// fleet.exa_endpoint and fleet.exa_key_ref apply.
func (f FleetConfig) UsesExa() bool {
	return f.SearchProviderOrDefault() == SearchExa || f.FetchProviderOrDefault() == FetchExa
}

// validateWeb checks the web provider fields. The Exa endpoint and key are
// scoped to the provider rather than the role, so a key is bound by name to
// the destination it is sent to; they must be unset unless a role uses Exa.
// The MCP search fields must be unset or default when Exa searches, so a
// stray value is never silently ignored.
func (f FleetConfig) validateWeb() error {
	switch f.SearchProviderOrDefault() {
	case SearchMCP:
		if f.SearchSendObjective {
			return fmt.Errorf("fleet.search_send_objective: applies to search provider %q only", SearchExa)
		}
	case SearchExa:
		stray := []struct {
			name string
			set  bool
		}{
			{"fleet.search_endpoint", f.SearchEndpoint != ""},
			{"fleet.search_key_ref", f.SearchKeyRef != ""},
			{"fleet.search_tool", f.SearchTool != DefaultSearchTool},
			{"fleet.search_query_arg", f.SearchQueryArg != DefaultSearchQueryArg},
		}
		for _, s := range stray {
			if s.set {
				return fmt.Errorf("%s: applies to search provider %q only, but fleet.search_provider is %q; remove it, or name the Exa endpoint and key with fleet.exa_endpoint and fleet.exa_key_ref",
					s.name, SearchMCP, SearchExa)
			}
		}
	default:
		return fmt.Errorf("fleet.search_provider: %q is not %q or %q", f.SearchProvider, SearchMCP, SearchExa)
	}
	switch f.FetchProviderOrDefault() {
	case FetchDirect:
	case FetchExa:
		if f.MaxPageBytes > maxExaPageCharacters {
			return fmt.Errorf("fleet.max_page_bytes: %d exceeds the %d characters Exa returns for one page", f.MaxPageBytes, maxExaPageCharacters)
		}
	default:
		return fmt.Errorf("fleet.fetch_provider: %q is not %q or %q", f.FetchProvider, FetchDirect, FetchExa)
	}
	if !f.UsesExa() {
		for _, s := range []struct {
			name string
			set  bool
		}{
			{"fleet.exa_endpoint", f.ExaEndpoint != ""},
			{"fleet.exa_key_ref", f.ExaKeyRef != ""},
		} {
			if s.set {
				return fmt.Errorf("%s: is set but neither fleet.search_provider nor fleet.fetch_provider is %q; select Exa for a role or remove it",
					s.name, SearchExa)
			}
		}
		return nil
	}
	if err := validEndpoint("fleet.exa_endpoint", f.ExaEndpoint); err != nil {
		return err
	}
	return validKeyRef("fleet.exa_key_ref", f.ExaKeyRef)
}

// registerWebFlags registers the web provider flags.
func registerWebFlags(fs *pflag.FlagSet) {
	fs.String("fleet-search-provider", "", fmt.Sprintf("web search provider: %q (an MCP server at --fleet-search-endpoint; default) or %q", SearchMCP, SearchExa))
	fs.String("fleet-fetch-provider", "", fmt.Sprintf("web fetch provider: %q (Chiron fetches pages itself; default) or %q", FetchDirect, FetchExa))
	fs.String("fleet-exa-endpoint", "", "Exa API base URL, normally https://api.exa.ai (absolute https://, http:// loopback only; overrides "+EnvFleetExaEndpoint+"; refused in a base config)")
	fs.String("fleet-exa-key-ref", "", "secret:// reference to the Exa key (never a literal; serves both roles)")
	fs.Bool("fleet-search-send-objective", false, "send the research objective to the search provider with each query (exa only)")
}

// applyWebFlag overlays name onto cfg when it is an explicitly set web
// provider flag.
func applyWebFlag(cfg *ResearchConfig, fs *pflag.FlagSet, name string) {
	switch name {
	case "fleet-search-provider":
		cfg.Fleet.SearchProvider = mustString(fs, name)
	case "fleet-fetch-provider":
		cfg.Fleet.FetchProvider = mustString(fs, name)
	case "fleet-exa-endpoint":
		cfg.Fleet.ExaEndpoint = mustString(fs, name)
	case "fleet-exa-key-ref":
		cfg.Fleet.ExaKeyRef = mustString(fs, name)
	case "fleet-search-send-objective":
		cfg.Fleet.SearchSendObjective = mustBool(fs, name)
	}
}
