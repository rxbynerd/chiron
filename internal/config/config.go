// Package config defines ResearchConfig: the single declarative
// configuration from which every concrete component of a research run is
// injected. It is JSON/YAML serialisable, flag-bindable, and composable —
// a base config (file or stdin) overlaid with explicitly set flags — so
// pipelines like
//
//	chiron research-config --agent deep-research-max \
//	  | chiron research-config --visualise \
//	  | chiron research --query "..."
//
// resolve deterministically: each stage only overrides what its flags
// explicitly set.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rxbynerd/chiron/internal/httpx"
)

// Agent tiers (PROPOSAL §3): the max tier is more comprehensive at roughly
// twice the cost. Mapping tiers to model identifiers is the Gemini
// adapter's concern, not config's.
//
// worker and fleet select Chiron's own in-process external-web research
// agents (V2-RESEARCH-AGENT §4): worker is a single search -> read ->
// synthesise loop, fleet is a lead orchestrator over bounded workers. Both
// draw their knobs from Fleet below; the deep-research tiers ignore it.
const (
	AgentDeepResearch    = "deep-research"
	AgentDeepResearchMax = "deep-research-max"
	AgentWorker          = "worker"
	AgentFleet           = "fleet"
)

// Memory bindings for the in-process research agents (V2-RESEARCH-AGENT §4).
// noop holds no Chiron-side context; inmemory is reserved for the in-process
// ContextStore and rejected until that store exists.
const (
	MemoryNoop     = "noop"
	MemoryInMemory = "inmemory"
)

// Knowledge store providers for the in-process research agents
// (docs/KNOWLEDGE.md). An empty provider disables recall and save-back.
const (
	KnowledgeBillet     = "billet"
	KnowledgeAlexandria = "alexandria"
)

// Knowledge recall limits: hits per recall by default and at most.
const (
	DefaultKnowledgeLimit = 5
	MaxKnowledgeLimit     = 20
)

// knowledgeSpace is Alexandria's space slug grammar; slugs are at most 64
// bytes.
var knowledgeSpace = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

const maxKnowledgeSpaceLen = 64

// DefaultSearchTool and DefaultSearchQueryArg are the reference web-search
// MCP backend's tool name and query argument key (docs/DECISIONS.md,
// 2026-09-25 "SP-A"), duplicated from search.Options' own defaults because
// config does not import the fleet packages.
const (
	DefaultSearchTool     = "search"
	DefaultSearchQueryArg = "query"
)

// searchIdentifier is the grammar for fleet.search_tool and
// fleet.search_query_arg: a vendor MCP search server may name its tool and
// argument differently, so these travel into a tools/call request rather
// than a fixed pair, and are bounded to what that request can safely carry.
var searchIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

const maxSearchIdentifierLen = 64

// Run output modes, mirroring Stirrup's output surface.
const (
	OutputText = "text"
	OutputJSON = "json"
	OutputNone = "none"
)

// DefaultAPIKeyRef is the default secret:// reference for the Gemini API
// key. Config carries references, never literal keys.
const DefaultAPIKeyRef = "secret://GEMINI_API_KEY"

// MaxTimeout is the hard wall-clock cap — the research agent's own
// 60-minute limit.
const MaxTimeout = 60 * time.Minute

// ResearchConfig declares one research run. Zero values mean "unset";
// Default supplies the documented defaults, and Decode overlays a base
// config on top of them, so an absent key never clobbers a default.
type ResearchConfig struct {
	// Query is the research question.
	Query string `json:"query,omitempty" yaml:"query,omitempty"`
	// Agent selects the tier: deep-research or deep-research-max.
	Agent string `json:"agent,omitempty" yaml:"agent,omitempty"`
	// Plan enables collaborative planning: review and refine the agent's
	// plan before spending money.
	Plan bool `json:"plan,omitempty" yaml:"plan,omitempty"`
	// AcceptPlan approves the first proposed plan without prompting —
	// the documented escape for running --plan without an interactive
	// terminal (the plan still lands in the stored interaction chain as
	// an audit trail).
	AcceptPlan bool `json:"accept_plan,omitempty" yaml:"accept_plan,omitempty"`
	// Model selects the follow-up Q&A model (chiron follow-up); empty
	// selects the adapter's documented default. Follow-up uses model,
	// not agent (docs/INTERACTIONS-API.md §3), so this is independent
	// of Agent.
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
	// Visualise asks the agent for charts (visualization: auto plus a
	// prompt nudge).
	Visualise bool `json:"visualise,omitempty" yaml:"visualise,omitempty"`
	// Stream streams thought summaries while awaiting; false polls
	// silently (--quiet).
	Stream bool `json:"stream" yaml:"stream"`
	// Tools overrides the agent's default tool set
	// (google_search, url_context, code_execution).
	Tools []string `json:"tools,omitempty" yaml:"tools,omitempty"`
	// MCP attaches remote MCP servers, name to URL.
	MCP map[string]string `json:"mcp,omitempty" yaml:"mcp,omitempty"`
	// FileSearch names internal corpus stores to search.
	FileSearch []string `json:"file_search,omitempty" yaml:"file_search,omitempty"`
	// Inputs are paths or URLs for multimodal document/image grounding.
	Inputs []string `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	// Template is the path to an output-format prompt template; empty
	// selects the built-in template.
	Template string `json:"template,omitempty" yaml:"template,omitempty"`
	// Output is the run output mode: text, json, or none.
	Output string `json:"output,omitempty" yaml:"output,omitempty"`
	// Out writes the Markdown report to this path instead of stdout.
	Out string `json:"out,omitempty" yaml:"out,omitempty"`
	// APIKeyRef is a secret:// reference to the API key — never a literal.
	APIKeyRef string `json:"api_key_ref,omitempty" yaml:"api_key_ref,omitempty"`
	// BudgetGBP caps the estimated cost; zero means uncapped.
	BudgetGBP float64 `json:"budget_gbp,omitempty" yaml:"budget_gbp,omitempty"`
	// Timeout is the wall-clock limit for the run (hard cap MaxTimeout).
	Timeout Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	// Fleet configures the in-process research agents (worker/fleet). It
	// is ignored by the deep-research tiers and validated only when Agent
	// selects worker or fleet, so a deep-research run stays valid with a
	// zero Fleet.
	Fleet FleetConfig `json:"fleet,omitzero" yaml:"fleet,omitempty"`
}

// FleetConfig holds the knobs for Chiron's in-process research agents
// (V2-RESEARCH-AGENT §4): the standard-model and search-MCP endpoints they
// call, the structural spend caps that bound a run, and the memory binding.
// Endpoints and key references are validated like CHIRON_GEMINI_BASE_URL —
// credentials travel to whatever endpoint is set, so an unvalidated override
// is a key-exfiltration and SSRF channel.
//
// Endpoint and key fields are optional at the config layer and required by
// the composition root when the agent runs; the caps carry documented
// defaults so a bare `--agent worker` run is already bounded.
type FleetConfig struct {
	// ModelEndpoint is the standard-model base URL. Absolute https://,
	// with http:// permitted for loopback test servers only. Distinct
	// from ResearchConfig.Model, which is the Gemini follow-up model.
	ModelEndpoint string `json:"model_endpoint,omitempty" yaml:"model_endpoint,omitempty"`
	// ModelName selects the standard frontier model the lead and workers
	// drive. The adapter has no default; the composition root requires it.
	ModelName string `json:"model_name,omitempty" yaml:"model_name,omitempty"`
	// ModelKeyRef is a secret:// reference to the standard-model API key —
	// never a literal.
	ModelKeyRef string `json:"model_key_ref,omitempty" yaml:"model_key_ref,omitempty"`
	// SearchEndpoint is the web-search MCP base URL, validated like
	// ModelEndpoint.
	SearchEndpoint string `json:"search_endpoint,omitempty" yaml:"search_endpoint,omitempty"`
	// SearchKeyRef is a secret:// reference to the search-MCP key — never
	// a literal.
	SearchKeyRef string `json:"search_key_ref,omitempty" yaml:"search_key_ref,omitempty"`
	// MaxTurns caps the search -> read -> synthesise turns of one worker.
	// Positive; the primary runaway-worker guard.
	MaxTurns int `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`
	// MaxTokens caps a worker's cumulative model tokens; zero means
	// uncapped on tokens (the turn and time caps still bound the loop).
	MaxTokens int `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	// CeilingGBP caps a worker's estimated model spend; zero means
	// uncapped on cost, mirroring BudgetGBP. The estimate is derived from
	// the price fields below, so a non-zero ceiling requires them. Token
	// and GBP ceilings are both kept: tokens bound a loop deterministically
	// with no price table, GBP expresses the operator's spend intent.
	CeilingGBP float64 `json:"ceiling_gbp,omitempty" yaml:"ceiling_gbp,omitempty"`
	// PriceInputGBPPerMTok and PriceOutputGBPPerMTok price a million prompt
	// and completion tokens of the configured model, in GBP. Zero leaves the
	// run's estimated cost at zero.
	PriceInputGBPPerMTok  float64 `json:"price_input_gbp_per_mtok,omitempty" yaml:"price_input_gbp_per_mtok,omitempty"`
	PriceOutputGBPPerMTok float64 `json:"price_output_gbp_per_mtok,omitempty" yaml:"price_output_gbp_per_mtok,omitempty"`
	// MaxPageBytes bounds the text of one fetched page, after HTML is
	// reduced to text, before it enters a worker's transcript. Positive.
	MaxPageBytes int `json:"max_page_bytes,omitempty" yaml:"max_page_bytes,omitempty"`
	// WorkerTimeout is the per-worker wall-clock limit. Positive.
	WorkerTimeout Duration `json:"worker_timeout,omitempty" yaml:"worker_timeout,omitempty"`
	// MaxWorkers caps how many workers the lead may dispatch in one run.
	// Positive; only meaningful for the fleet agent.
	MaxWorkers int `json:"max_workers,omitempty" yaml:"max_workers,omitempty"`
	// Concurrency caps how many workers run at once. Positive; must not
	// exceed MaxWorkers.
	Concurrency int `json:"concurrency,omitempty" yaml:"concurrency,omitempty"`
	// Memory selects the ContextStore binding: noop (inmemory is reserved).
	Memory string `json:"memory,omitempty" yaml:"memory,omitempty"`
	// KnowledgeProvider selects the knowledge store the worker recalls from:
	// empty (none), billet or alexandria.
	KnowledgeProvider string `json:"knowledge_provider,omitempty" yaml:"knowledge_provider,omitempty"`
	// KnowledgeEndpoint is the knowledge store base URL, validated like
	// ModelEndpoint; the composition root requires it with a provider.
	KnowledgeEndpoint string `json:"knowledge_endpoint,omitempty" yaml:"knowledge_endpoint,omitempty"`
	// KnowledgeKeyRef is a secret:// reference to the knowledge store key —
	// never a literal. Optional for billet, required for alexandria.
	KnowledgeKeyRef string `json:"knowledge_key_ref,omitempty" yaml:"knowledge_key_ref,omitempty"`
	// KnowledgeSpace is the Alexandria space slug recalls are scoped to.
	KnowledgeSpace string `json:"knowledge_space,omitempty" yaml:"knowledge_space,omitempty"`
	// KnowledgeLimit is the hits per recall, 1 to MaxKnowledgeLimit.
	KnowledgeLimit int `json:"knowledge_limit,omitempty" yaml:"knowledge_limit,omitempty"`
	// KnowledgeRemember saves each completed finding back to the store
	// (billet only).
	KnowledgeRemember bool `json:"knowledge_remember,omitempty" yaml:"knowledge_remember,omitempty"`
	// SearchTool is the MCP tool the search client invokes in tools/call.
	// Default DefaultSearchTool; override for a vendor search server that
	// names its tool differently.
	SearchTool string `json:"search_tool,omitempty" yaml:"search_tool,omitempty"`
	// SearchQueryArg is the argument key the search query is passed under
	// in the tools/call arguments object. Default DefaultSearchQueryArg;
	// override for a vendor search server that names its argument
	// differently.
	SearchQueryArg string `json:"search_query_arg,omitempty" yaml:"search_query_arg,omitempty"`
}

// Default returns the documented defaults (PROPOSAL §4.3). The Fleet
// defaults bound an in-process run out of the box; they are inert for the
// deep-research tiers, which never read them.
func Default() ResearchConfig {
	return ResearchConfig{
		Agent:     AgentDeepResearch,
		Stream:    true,
		Output:    OutputText,
		APIKeyRef: DefaultAPIKeyRef,
		Timeout:   Duration(30 * time.Minute),
		Fleet:     defaultFleet(),
	}
}

// defaultFleet returns the documented caps (V2-RESEARCH-AGENT §4): a small
// per-worker turn budget, a token cap that bounds spend without a price
// table, a page bound small relative to a model context window, a
// per-worker timeout well inside the run's own limit, a bounded fan-out,
// the no-op memory binding, and the recall limit a knowledge provider would
// use. Endpoints, key references and the provider stay empty.
func defaultFleet() FleetConfig {
	return FleetConfig{
		MaxTurns:       8,
		MaxTokens:      400_000,
		MaxPageBytes:   64 << 10,
		WorkerTimeout:  Duration(5 * time.Minute),
		MaxWorkers:     5,
		Concurrency:    3,
		Memory:         MemoryNoop,
		KnowledgeLimit: DefaultKnowledgeLimit,
		SearchTool:     DefaultSearchTool,
		SearchQueryArg: DefaultSearchQueryArg,
	}
}

// Decode reads a base ResearchConfig (JSON or YAML — JSON is a YAML
// subset, so one strict decoder covers both) overlaid on the defaults.
// Empty input yields the defaults, so an empty stdin pipe is harmless.
// Unknown keys are an error: configs are small and a silent typo would
// silently change a paid run.
func Decode(r io.Reader) (ResearchConfig, error) {
	cfg := Default()
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return Default(), nil
		}
		return ResearchConfig{}, fmt.Errorf("decode research config: %w", err)
	}
	return cfg, nil
}

// EncodeJSON writes the resolved config as indented JSON, the wire form
// emitted by `chiron research-config` for pipeline composition.
func (c ResearchConfig) EncodeJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("encode research config: %w", err)
	}
	return nil
}

// Validate checks the declarative invariants that hold regardless of
// command: enumerations, the timeout cap, and the secret-reference rule.
// Per-command requirements (such as research needing a query) belong to
// the command.
func (c ResearchConfig) Validate() error {
	switch c.Agent {
	case AgentDeepResearch, AgentDeepResearchMax:
	case AgentWorker, AgentFleet:
		if err := c.Fleet.validate(c.Agent); err != nil {
			return err
		}
		if err := c.rejectDeepResearchLevers(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("agent: %q is not one of %q, %q, %q or %q",
			c.Agent, AgentDeepResearch, AgentDeepResearchMax, AgentWorker, AgentFleet)
	}
	switch c.Output {
	case OutputText, OutputJSON, OutputNone:
	default:
		return fmt.Errorf("output: %q is not %q, %q or %q", c.Output, OutputText, OutputJSON, OutputNone)
	}
	if d := time.Duration(c.Timeout); d <= 0 || d > MaxTimeout {
		return fmt.Errorf("timeout: %s is outside (0, %s] — the agent's own limit", d, MaxTimeout)
	}
	if c.BudgetGBP < 0 {
		return fmt.Errorf("budget: %v GBP is negative", c.BudgetGBP)
	}
	if !strings.HasPrefix(c.APIKeyRef, "secret://") {
		return fmt.Errorf("api_key_ref: %q is not a secret:// reference — literal keys never live in config", c.APIKeyRef)
	}
	for name, url := range c.MCP {
		if name == "" || url == "" {
			return fmt.Errorf("mcp: server entries need both a name and a URL")
		}
		// MCP servers are remote by definition; a file: or javascript:
		// URL would only produce a confusing API error after the
		// request is built — reject it here with a local message.
		if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
			return fmt.Errorf("mcp: server %q URL %q must be http(s) — MCP servers are remote endpoints", name, url)
		}
	}
	return nil
}

// rejectDeepResearchLevers refuses the levers that only the Gemini
// deep-research adapter honours when an in-process agent is selected, so a
// spend or planning control is never silently ignored. Stream is exempt: it
// defaults on and has no spend meaning.
func (c ResearchConfig) rejectDeepResearchLevers() error {
	set := []struct {
		name string
		on   bool
		hint string
	}{
		{"budget", c.BudgetGBP != 0, "use fleet.ceiling_gbp with the price fields, or fleet.max_tokens"},
		{"plan", c.Plan, "the in-process agents have no plan phase"},
		{"accept_plan", c.AcceptPlan, "the in-process agents have no plan phase"},
		{"model", c.Model != "", "use fleet.model_name"},
		{"visualise", c.Visualise, "the in-process agents produce no charts"},
		{"tools", len(c.Tools) > 0, "the in-process agents use fleet.search_endpoint and web_fetch only"},
		{"mcp", len(c.MCP) > 0, "use fleet.search_endpoint or the fleet.knowledge_* fields"},
		{"file_search", len(c.FileSearch) > 0, "the in-process agents search the external web only"},
		{"inputs", len(c.Inputs) > 0, "the in-process agents take no input documents"},
		{"template", c.Template != "", "the in-process agents do not take an output template"},
	}
	for _, lever := range set {
		if lever.on {
			return fmt.Errorf("%s: applies to the deep-research tiers only and is not honoured by agent %q; %s", lever.name, c.Agent, lever.hint)
		}
	}
	return nil
}

// validate checks the in-process research knobs. It runs only when Agent
// is worker or fleet, so a deep-research run stays valid with a zero
// FleetConfig. Endpoints and key references are optional here (the
// composition root requires them); when present they must satisfy the same
// rules the Gemini base-URL override does. agent gates the fleet-only caps:
// a single worker has no fan-out, so MaxWorkers/Concurrency are enforced for
// fleet only.
func (f FleetConfig) validate(agent string) error {
	if err := validEndpoint("fleet.model_endpoint", f.ModelEndpoint); err != nil {
		return err
	}
	if err := validEndpoint("fleet.search_endpoint", f.SearchEndpoint); err != nil {
		return err
	}
	if err := validKeyRef("fleet.model_key_ref", f.ModelKeyRef); err != nil {
		return err
	}
	if err := validKeyRef("fleet.search_key_ref", f.SearchKeyRef); err != nil {
		return err
	}
	if err := validIdentifier("fleet.search_tool", f.SearchTool); err != nil {
		return err
	}
	if err := validIdentifier("fleet.search_query_arg", f.SearchQueryArg); err != nil {
		return err
	}
	if f.MaxTurns <= 0 {
		return fmt.Errorf("fleet.max_turns: %d is not positive — a worker needs at least one turn", f.MaxTurns)
	}
	if f.MaxTokens < 0 {
		return fmt.Errorf("fleet.max_tokens: %d is negative", f.MaxTokens)
	}
	if f.CeilingGBP < 0 {
		return fmt.Errorf("fleet.ceiling_gbp: %v GBP is negative", f.CeilingGBP)
	}
	if f.PriceInputGBPPerMTok < 0 || f.PriceOutputGBPPerMTok < 0 {
		return fmt.Errorf("fleet.price_input_gbp_per_mtok / fleet.price_output_gbp_per_mtok: %v / %v must not be negative",
			f.PriceInputGBPPerMTok, f.PriceOutputGBPPerMTok)
	}
	if f.CeilingGBP > 0 && f.PriceInputGBPPerMTok == 0 && f.PriceOutputGBPPerMTok == 0 {
		return errors.New("fleet.ceiling_gbp: a cost ceiling needs fleet.price_input_gbp_per_mtok and fleet.price_output_gbp_per_mtok to estimate spend against")
	}
	if f.MaxPageBytes <= 0 {
		return fmt.Errorf("fleet.max_page_bytes: %d is not positive", f.MaxPageBytes)
	}
	if d := time.Duration(f.WorkerTimeout); d <= 0 {
		return fmt.Errorf("fleet.worker_timeout: %s is not positive", d)
	}
	if agent == AgentFleet {
		if f.MaxWorkers <= 0 {
			return fmt.Errorf("fleet.max_workers: %d is not positive", f.MaxWorkers)
		}
		if f.Concurrency <= 0 {
			return fmt.Errorf("fleet.concurrency: %d is not positive", f.Concurrency)
		}
		if f.Concurrency > f.MaxWorkers {
			return fmt.Errorf("fleet.concurrency: %d exceeds fleet.max_workers %d", f.Concurrency, f.MaxWorkers)
		}
	}
	switch f.Memory {
	case MemoryNoop:
	case MemoryInMemory:
		return fmt.Errorf("fleet.memory: %q is not implemented yet; use %q", f.Memory, MemoryNoop)
	default:
		return fmt.Errorf("fleet.memory: %q is not %q or %q", f.Memory, MemoryNoop, MemoryInMemory)
	}
	return f.validateKnowledge()
}

// validateKnowledge checks the knowledge store fields. Without a provider
// every other knowledge field must be empty or default, so a stray endpoint
// is never silently ignored. The endpoint and key reference follow the same
// rules as the model and search pair: the key travels to the endpoint.
func (f FleetConfig) validateKnowledge() error {
	switch f.KnowledgeProvider {
	case "":
		stray := []struct {
			name string
			set  bool
		}{
			{"fleet.knowledge_endpoint", f.KnowledgeEndpoint != ""},
			{"fleet.knowledge_key_ref", f.KnowledgeKeyRef != ""},
			{"fleet.knowledge_space", f.KnowledgeSpace != ""},
			{"fleet.knowledge_limit", f.KnowledgeLimit != 0 && f.KnowledgeLimit != DefaultKnowledgeLimit},
			{"fleet.knowledge_remember", f.KnowledgeRemember},
		}
		for _, s := range stray {
			if s.set {
				return fmt.Errorf("%s: is set but fleet.knowledge_provider is empty; set a provider (%q or %q) or remove it",
					s.name, KnowledgeBillet, KnowledgeAlexandria)
			}
		}
		return nil
	case KnowledgeBillet, KnowledgeAlexandria:
	default:
		return fmt.Errorf("fleet.knowledge_provider: %q is not %q or %q", f.KnowledgeProvider, KnowledgeBillet, KnowledgeAlexandria)
	}
	if err := validEndpoint("fleet.knowledge_endpoint", f.KnowledgeEndpoint); err != nil {
		return err
	}
	if err := validKeyRef("fleet.knowledge_key_ref", f.KnowledgeKeyRef); err != nil {
		return err
	}
	if f.KnowledgeSpace != "" {
		if f.KnowledgeProvider != KnowledgeAlexandria {
			return fmt.Errorf("fleet.knowledge_space: applies to %q only; %q binds its namespace server-side", KnowledgeAlexandria, f.KnowledgeProvider)
		}
		if len(f.KnowledgeSpace) > maxKnowledgeSpaceLen || !knowledgeSpace.MatchString(f.KnowledgeSpace) {
			return fmt.Errorf("fleet.knowledge_space: %q is not a space slug (lower-case letters, digits and inner hyphens, at most %d)",
				f.KnowledgeSpace, maxKnowledgeSpaceLen)
		}
	}
	if f.KnowledgeLimit < 1 || f.KnowledgeLimit > MaxKnowledgeLimit {
		return fmt.Errorf("fleet.knowledge_limit: %d is outside 1..%d", f.KnowledgeLimit, MaxKnowledgeLimit)
	}
	if f.KnowledgeRemember && f.KnowledgeProvider != KnowledgeBillet {
		return fmt.Errorf("fleet.knowledge_remember: applies to %q only; %q has no save path Chiron can use", KnowledgeBillet, f.KnowledgeProvider)
	}
	return nil
}

// validEndpoint admits an unset endpoint (the composition root requires it)
// and otherwise applies httpx.ParseEndpoint: credentials go to whatever
// endpoint is configured (CWE-918, CWE-319).
func validEndpoint(field, raw string) error {
	if raw == "" {
		return nil
	}
	if _, err := httpx.ParseEndpoint(raw); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

// validKeyRef admits an unset reference and, when set, requires the
// secret:// form — literal keys never live in config. The message never
// echoes the value, so a mistakenly pasted literal cannot leak through a
// validation error.
func validKeyRef(field, ref string) error {
	if ref == "" {
		return nil
	}
	if !strings.HasPrefix(ref, "secret://") {
		return fmt.Errorf("%s: is not a secret:// reference — literal keys never live in config", field)
	}
	return nil
}

// validIdentifier requires a non-empty MCP tool name or argument key
// (fleet.search_tool, fleet.search_query_arg): letters, digits, underscore,
// dot or hyphen only, at most maxSearchIdentifierLen bytes. The message
// never echoes value, which may carry a pasted secret (docs/DECISIONS.md "SP-A").
func validIdentifier(field, value string) error {
	if value == "" || len(value) > maxSearchIdentifierLen || !searchIdentifier.MatchString(value) {
		return fmt.Errorf("%s: must be a non-empty identifier of letters, digits, underscore, dot or hyphen, at most %d bytes", field, maxSearchIdentifierLen)
	}
	return nil
}
