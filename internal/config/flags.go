package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

// RegisterFlags registers the research flags (PROPOSAL §4.3) on fs with
// their documented defaults. Flag defaults exist for help text; resolution
// always starts from Default() and ApplyFlags overlays only the flags the
// user explicitly set, so a piped base config is never clobbered by a
// default.
func RegisterFlags(fs *pflag.FlagSet) {
	d := Default()
	fs.String("query", "", "research question (alternatively the positional argument)")
	fs.String("agent", d.Agent, fmt.Sprintf("agent: %q, %q, %q or %q", AgentDeepResearch, AgentDeepResearchMax, AgentWorker, AgentFleet))
	fs.Bool("plan", false, "collaborative planning: review and refine the plan before spending")
	fs.Bool("accept-plan", false, "with --plan, approve the first proposed plan without prompting")
	fs.String("model", "", "follow-up Q&A model (chiron follow-up; adapter default if unset)")
	fs.Bool("visualise", false, "ask the agent to produce charts")
	fs.Bool("stream", d.Stream, "stream thought summaries while awaiting")
	fs.Bool("quiet", false, "poll silently instead of streaming")
	fs.StringSlice("tools", nil, "tool set, comma-separated (defaults to the API's own)")
	fs.StringArray("mcp", nil, "remote MCP server as name=url (repeatable)")
	fs.StringArray("file-search", nil, "internal corpus store to search (repeatable)")
	fs.StringArray("input", nil, "document or image grounding, path or URL (repeatable)")
	fs.String("template", "", "output-format prompt template path (built-in if unset)")
	fs.StringP("output", "o", d.Output, fmt.Sprintf("run output: %q, %q or %q", OutputText, OutputJSON, OutputNone))
	fs.String("out", "", "write the Markdown report to this path instead of stdout")
	fs.String("api-key-ref", d.APIKeyRef, "secret:// reference to the API key (never a literal)")
	fs.Float64("budget", 0, "estimated-cost cap in GBP for the deep-research tiers; unset means uncapped (worker/fleet: --fleet-ceiling)")
	fs.Duration("timeout", time.Duration(d.Timeout), fmt.Sprintf("wall-clock timeout (hard cap %s)", MaxTimeout))

	// In-process research knobs (--agent worker/fleet), prefixed fleet- to
	// stay clear of the deep-research levers they sit beside; defaults come
	// from Default().Fleet.
	fs.String("fleet-model-endpoint", "", "standard-model base URL (absolute https://, http:// loopback only)")
	fs.String("fleet-model-name", "", "standard frontier model name (required for worker/fleet)")
	fs.String("fleet-model-key-ref", "", "secret:// reference to the standard-model key (never a literal)")
	fs.String("fleet-search-endpoint", "", "web-search MCP base URL (absolute https://, http:// loopback only)")
	fs.String("fleet-search-key-ref", "", "secret:// reference to the search-MCP key (never a literal)")
	fs.String("fleet-search-tool", d.Fleet.SearchTool, fmt.Sprintf("MCP tool the search client invokes (default %q)", DefaultSearchTool))
	fs.String("fleet-search-query-arg", d.Fleet.SearchQueryArg, fmt.Sprintf("argument key the search query is passed under (default %q)", DefaultSearchQueryArg))
	fs.Int("fleet-max-turns", d.Fleet.MaxTurns, "per-worker search->read->synthesise turn cap")
	fs.Int("fleet-max-tokens", d.Fleet.MaxTokens, "per-worker model token ceiling; 0 means uncapped")
	fs.Float64("fleet-ceiling", d.Fleet.CeilingGBP, "per-worker estimated-cost ceiling in GBP; 0 means uncapped (needs the fleet-price flags)")
	fs.Float64("fleet-price-input", d.Fleet.PriceInputGBPPerMTok, "model price in GBP per million prompt tokens (for --fleet-ceiling)")
	fs.Float64("fleet-price-output", d.Fleet.PriceOutputGBPPerMTok, "model price in GBP per million completion tokens (for --fleet-ceiling)")
	fs.Int("fleet-max-page-bytes", d.Fleet.MaxPageBytes, "bound on one fetched page's text before it enters the transcript")
	fs.Duration("fleet-worker-timeout", time.Duration(d.Fleet.WorkerTimeout), "per-worker wall-clock timeout")
	fs.Int("fleet-max-workers", d.Fleet.MaxWorkers, "maximum workers the fleet lead may dispatch")
	fs.Int("fleet-concurrency", d.Fleet.Concurrency, "maximum workers running at once (<= fleet-max-workers)")
	fs.String("fleet-memory", d.Fleet.Memory, fmt.Sprintf("ContextStore binding: %q (%q is reserved and not yet implemented)", MemoryNoop, MemoryInMemory))
	fs.String("fleet-knowledge-provider", "", fmt.Sprintf("knowledge store the worker recalls from: %q or %q (unset disables recall)", KnowledgeBillet, KnowledgeAlexandria))
	fs.String("fleet-knowledge-endpoint", "", "knowledge store base URL (absolute https://, http:// loopback only)")
	fs.String("fleet-knowledge-key-ref", "", "secret:// reference to the knowledge store key (never a literal; required for alexandria)")
	fs.String("fleet-knowledge-space", "", "Alexandria space slug to scope recalls to (alexandria only)")
	fs.Int("fleet-knowledge-limit", d.Fleet.KnowledgeLimit, fmt.Sprintf("hits per recall, 1..%d", MaxKnowledgeLimit))
	fs.Bool("fleet-knowledge-remember", false, "save each completed finding back to the knowledge store (billet only)")
}

// ApplyFlags overlays the flags the user explicitly set onto cfg. Unset
// flags leave cfg untouched — the merge semantics that make pipeline
// composition work. --quiet is sugar for --stream=false; commands mark
// the pair mutually exclusive.
func ApplyFlags(cfg *ResearchConfig, fs *pflag.FlagSet) error {
	var applyErr error
	fail := func(err error) {
		if applyErr == nil {
			applyErr = err
		}
	}

	fs.Visit(func(f *pflag.Flag) {
		switch f.Name {
		case "query":
			cfg.Query = mustString(fs, f.Name)
		case "agent":
			cfg.Agent = mustString(fs, f.Name)
		case "plan":
			cfg.Plan = mustBool(fs, f.Name)
		case "accept-plan":
			cfg.AcceptPlan = mustBool(fs, f.Name)
		case "model":
			cfg.Model = mustString(fs, f.Name)
		case "visualise":
			cfg.Visualise = mustBool(fs, f.Name)
		case "stream":
			cfg.Stream = mustBool(fs, f.Name)
		case "quiet":
			cfg.Stream = !mustBool(fs, f.Name)
		case "tools":
			cfg.Tools, _ = fs.GetStringSlice(f.Name)
		case "mcp":
			entries, _ := fs.GetStringArray(f.Name)
			mcp, err := parseMCP(entries)
			if err != nil {
				fail(err)
				return
			}
			cfg.MCP = mcp
		case "file-search":
			cfg.FileSearch, _ = fs.GetStringArray(f.Name)
		case "input":
			cfg.Inputs, _ = fs.GetStringArray(f.Name)
		case "template":
			cfg.Template = mustString(fs, f.Name)
		case "output":
			cfg.Output = mustString(fs, f.Name)
		case "out":
			cfg.Out = mustString(fs, f.Name)
		case "api-key-ref":
			cfg.APIKeyRef = mustString(fs, f.Name)
		case "budget":
			cfg.BudgetGBP, _ = fs.GetFloat64(f.Name)
		case "timeout":
			d, _ := fs.GetDuration(f.Name)
			cfg.Timeout = Duration(d)
		case "fleet-model-endpoint":
			cfg.Fleet.ModelEndpoint = mustString(fs, f.Name)
		case "fleet-model-name":
			cfg.Fleet.ModelName = mustString(fs, f.Name)
		case "fleet-model-key-ref":
			cfg.Fleet.ModelKeyRef = mustString(fs, f.Name)
		case "fleet-search-endpoint":
			cfg.Fleet.SearchEndpoint = mustString(fs, f.Name)
		case "fleet-search-key-ref":
			cfg.Fleet.SearchKeyRef = mustString(fs, f.Name)
		case "fleet-search-tool":
			cfg.Fleet.SearchTool = mustString(fs, f.Name)
		case "fleet-search-query-arg":
			cfg.Fleet.SearchQueryArg = mustString(fs, f.Name)
		case "fleet-max-turns":
			cfg.Fleet.MaxTurns, _ = fs.GetInt(f.Name)
		case "fleet-max-tokens":
			cfg.Fleet.MaxTokens, _ = fs.GetInt(f.Name)
		case "fleet-ceiling":
			cfg.Fleet.CeilingGBP, _ = fs.GetFloat64(f.Name)
		case "fleet-price-input":
			cfg.Fleet.PriceInputGBPPerMTok, _ = fs.GetFloat64(f.Name)
		case "fleet-price-output":
			cfg.Fleet.PriceOutputGBPPerMTok, _ = fs.GetFloat64(f.Name)
		case "fleet-max-page-bytes":
			cfg.Fleet.MaxPageBytes, _ = fs.GetInt(f.Name)
		case "fleet-worker-timeout":
			d, _ := fs.GetDuration(f.Name)
			cfg.Fleet.WorkerTimeout = Duration(d)
		case "fleet-max-workers":
			cfg.Fleet.MaxWorkers, _ = fs.GetInt(f.Name)
		case "fleet-concurrency":
			cfg.Fleet.Concurrency, _ = fs.GetInt(f.Name)
		case "fleet-memory":
			cfg.Fleet.Memory = mustString(fs, f.Name)
		case "fleet-knowledge-provider":
			cfg.Fleet.KnowledgeProvider = mustString(fs, f.Name)
		case "fleet-knowledge-endpoint":
			cfg.Fleet.KnowledgeEndpoint = mustString(fs, f.Name)
		case "fleet-knowledge-key-ref":
			cfg.Fleet.KnowledgeKeyRef = mustString(fs, f.Name)
		case "fleet-knowledge-space":
			cfg.Fleet.KnowledgeSpace = mustString(fs, f.Name)
		case "fleet-knowledge-limit":
			cfg.Fleet.KnowledgeLimit, _ = fs.GetInt(f.Name)
		case "fleet-knowledge-remember":
			cfg.Fleet.KnowledgeRemember = mustBool(fs, f.Name)
		}
	})

	return applyErr
}

// parseMCP turns repeated name=url entries into the MCP map.
func parseMCP(entries []string) (map[string]string, error) {
	mcp := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, url, ok := strings.Cut(entry, "=")
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("mcp: %q is not name=url", entry)
		}
		mcp[name] = url
	}
	return mcp, nil
}

func mustString(fs *pflag.FlagSet, name string) string {
	v, _ := fs.GetString(name)
	return v
}

func mustBool(fs *pflag.FlagSet, name string) bool {
	v, _ := fs.GetBool(name)
	return v
}
