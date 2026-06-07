package config

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

func TestDecodeEmptyInputYieldsDefaults(t *testing.T) {
	cfg, err := Decode(strings.NewReader(""))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("got %+v, want defaults %+v", cfg, Default())
	}
}

func TestDecodeOverlaysDefaults(t *testing.T) {
	// Absent keys must keep their defaults; present keys must override.
	cfg, err := Decode(strings.NewReader("agent: deep-research-max\nstream: false\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.Agent != AgentDeepResearchMax {
		t.Errorf("agent: got %q", cfg.Agent)
	}
	if cfg.Stream {
		t.Error("stream: explicit false was clobbered")
	}
	if cfg.Output != OutputText || cfg.APIKeyRef != DefaultAPIKeyRef {
		t.Errorf("absent keys lost defaults: %+v", cfg)
	}
}

func TestDecodeRejectsUnknownKeys(t *testing.T) {
	if _, err := Decode(strings.NewReader("agnet: deep-research\n")); err == nil {
		t.Error("typo key was accepted silently")
	}
}

func TestJSONRoundTripThroughDecode(t *testing.T) {
	// research-config emits JSON; the next pipeline stage decodes it.
	in := Default()
	in.Query = "10BASE-T1L PHY vendors"
	in.Agent = AgentDeepResearchMax
	in.MCP = map[string]string{"corp": "https://mcp.internal"}
	in.Timeout = Duration(45 * time.Minute)

	var buf bytes.Buffer
	if err := in.EncodeJSON(&buf); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	out, err := Decode(&buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.Query != in.Query || out.Agent != in.Agent || out.Timeout != in.Timeout || out.MCP["corp"] != "https://mcp.internal" {
		t.Errorf("round trip lost data: %+v", out)
	}
}

func TestApplyFlagsOverlaysOnlySetFlags(t *testing.T) {
	base := Default()
	base.Agent = AgentDeepResearchMax
	base.Query = "from the pipe"

	fs := newFlagSet(t)
	if err := fs.Parse([]string{"--visualise", "--quiet"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ApplyFlags(&base, fs); err != nil {
		t.Fatalf("ApplyFlags: %v", err)
	}

	if !base.Visualise {
		t.Error("visualise flag not applied")
	}
	if base.Stream {
		t.Error("--quiet must mean stream=false")
	}
	// Unset flags must not reset piped base values to flag defaults.
	if base.Agent != AgentDeepResearchMax || base.Query != "from the pipe" {
		t.Errorf("unset flags clobbered the base: %+v", base)
	}
}

func TestApplyFlagsPlanningAndModelLevers(t *testing.T) {
	cfg := Default()
	fs := newFlagSet(t)
	if err := fs.Parse([]string{"--plan", "--accept-plan", "--model", "gemini-3.1-pro-preview"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ApplyFlags(&cfg, fs); err != nil {
		t.Fatalf("ApplyFlags: %v", err)
	}
	if !cfg.Plan || !cfg.AcceptPlan {
		t.Errorf("plan levers not applied: %+v", cfg)
	}
	if cfg.Model != "gemini-3.1-pro-preview" {
		t.Errorf("model = %q", cfg.Model)
	}
}

func TestApplyFlagsParsesMCP(t *testing.T) {
	cfg := Default()
	fs := newFlagSet(t)
	if err := fs.Parse([]string{"--mcp", "corp=https://mcp.internal", "--mcp", "docs=https://docs.internal"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ApplyFlags(&cfg, fs); err != nil {
		t.Fatalf("ApplyFlags: %v", err)
	}
	if cfg.MCP["corp"] != "https://mcp.internal" || cfg.MCP["docs"] != "https://docs.internal" {
		t.Errorf("mcp: %+v", cfg.MCP)
	}

	fs = newFlagSet(t)
	if err := fs.Parse([]string{"--mcp", "missing-equals"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ApplyFlags(&cfg, fs); err == nil {
		t.Error("malformed --mcp entry was accepted")
	}
}

func TestValidate(t *testing.T) {
	good := Default()
	if err := good.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}

	cases := map[string]func(*ResearchConfig){
		"unknown agent":     func(c *ResearchConfig) { c.Agent = "deep-thought" },
		"unknown output":    func(c *ResearchConfig) { c.Output = "xml" },
		"timeout above cap": func(c *ResearchConfig) { c.Timeout = Duration(2 * time.Hour) },
		"zero timeout":      func(c *ResearchConfig) { c.Timeout = 0 },
		"negative budget":   func(c *ResearchConfig) { c.BudgetGBP = -1 },
		"literal api key":   func(c *ResearchConfig) { c.APIKeyRef = "AIzaSyLiteralKey" },
	}
	for name, mutate := range cases {
		cfg := Default()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
}

func newFlagSet(t *testing.T) *pflag.FlagSet {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs)
	return fs
}
