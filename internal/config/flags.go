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
	fs.String("agent", d.Agent, fmt.Sprintf("agent tier: %q or %q", AgentDeepResearch, AgentDeepResearchMax))
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
	fs.Float64("budget", 0, "estimated-cost cap in GBP; unset means uncapped")
	fs.Duration("timeout", time.Duration(d.Timeout), fmt.Sprintf("wall-clock timeout (hard cap %s)", MaxTimeout))
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
