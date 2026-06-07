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
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Agent tiers (PROPOSAL §3): the max tier is more comprehensive at roughly
// twice the cost. Mapping tiers to model identifiers is the Gemini
// adapter's concern, not config's.
const (
	AgentDeepResearch    = "deep-research"
	AgentDeepResearchMax = "deep-research-max"
)

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
}

// Default returns the documented defaults (PROPOSAL §4.3).
func Default() ResearchConfig {
	return ResearchConfig{
		Agent:     AgentDeepResearch,
		Stream:    true,
		Output:    OutputText,
		APIKeyRef: DefaultAPIKeyRef,
		Timeout:   Duration(30 * time.Minute),
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
	default:
		return fmt.Errorf("agent: %q is not %q or %q", c.Agent, AgentDeepResearch, AgentDeepResearchMax)
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
	}
	return nil
}
