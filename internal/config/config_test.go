package config

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

func TestDecodeEmptyInputYieldsDefaults(t *testing.T) {
	cfg, err := decode(strings.NewReader(""))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("got %+v, want defaults %+v", cfg, Default())
	}
}

func TestDecodeOverlaysDefaults(t *testing.T) {
	// Absent keys must keep their defaults; present keys must override.
	cfg, err := decode(strings.NewReader("agent: deep-research-max\nstream: false\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
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
	if _, err := decode(strings.NewReader("agnet: deep-research\n")); err == nil {
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
	out, err := decode(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
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
		// C2-CODE-3: MCP servers are remote endpoints; non-web schemes
		// must fail locally, not as a confusing API error.
		"mcp file url":       func(c *ResearchConfig) { c.MCP = map[string]string{"local": "file:///srv/mcp"} },
		"mcp javascript url": func(c *ResearchConfig) { c.MCP = map[string]string{"js": "javascript:alert(1)"} },
		"mcp bare host":      func(c *ResearchConfig) { c.MCP = map[string]string{"bare": "mcp.internal"} },
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

// TestJSONNativeRoundTrip pins C2-TEST-5: pipeline tooling may decode
// research-config output with encoding/json rather than a YAML decoder,
// which routes durations through UnmarshalJSON — a path decode (yaml.v3
// underneath) never exercises. The string duration form EncodeJSON
// emits must survive json.Unmarshal.
func TestJSONNativeRoundTrip(t *testing.T) {
	in := Default()
	in.Query = "10BASE-T1L PHY vendors"
	in.Timeout = Duration(45 * time.Minute)

	var buf bytes.Buffer
	if err := in.EncodeJSON(&buf); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	var got ResearchConfig
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.Timeout != in.Timeout {
		t.Errorf("timeout = %v, want %v — UnmarshalJSON must parse the string duration form", got.Timeout, in.Timeout)
	}
	if got.Query != in.Query {
		t.Errorf("query = %q, want %q", got.Query, in.Query)
	}
}

// TestDurationUnmarshalJSONForms: both accepted wire forms, plus the
// rejection of anything else.
func TestDurationUnmarshalJSONForms(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"30m"`), &d); err != nil || time.Duration(d) != 30*time.Minute {
		t.Errorf(`"30m" = %v, %v; want 30m`, d, err)
	}
	if err := json.Unmarshal([]byte(`60000000000`), &d); err != nil || time.Duration(d) != time.Minute {
		t.Errorf("nanosecond form = %v, %v; want 1m", d, err)
	}
	if err := json.Unmarshal([]byte(`"not a duration"`), &d); err == nil {
		t.Error("garbage duration was accepted")
	}
	if err := json.Unmarshal([]byte(`true`), &d); err == nil {
		t.Error("a boolean duration was accepted")
	}
}

// TestDurationMarshalYAML pins C2-TEST-12: the YAML form is the
// human-readable string, never a nanosecond integer.
func TestDurationMarshalYAML(t *testing.T) {
	out, err := yaml.Marshal(Duration(30 * time.Minute))
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "30m0s" {
		t.Errorf("YAML form = %q, want %q", got, "30m0s")
	}
}

// TestApplyFlagsOutputAndCostLevers pins C2-TEST-9: each flag is set
// alone and only its field may change — a transposed pflag binding or
// type coercion bug must fail here, not three layers up the CLI stack.
func TestApplyFlagsOutputAndCostLevers(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		assert func(t *testing.T, cfg ResearchConfig)
	}{
		{"budget", []string{"--budget", "2.50"}, func(t *testing.T, cfg ResearchConfig) {
			if cfg.BudgetGBP != 2.50 {
				t.Errorf("BudgetGBP = %v, want 2.50", cfg.BudgetGBP)
			}
		}},
		{"timeout", []string{"--timeout", "45m"}, func(t *testing.T, cfg ResearchConfig) {
			if time.Duration(cfg.Timeout) != 45*time.Minute {
				t.Errorf("Timeout = %v, want 45m", cfg.Timeout)
			}
		}},
		{"output", []string{"--output", "json"}, func(t *testing.T, cfg ResearchConfig) {
			if cfg.Output != OutputJSON {
				t.Errorf("Output = %q, want %q", cfg.Output, OutputJSON)
			}
		}},
		{"out", []string{"--out", "/tmp/x.md"}, func(t *testing.T, cfg ResearchConfig) {
			if cfg.Out != "/tmp/x.md" {
				t.Errorf("Out = %q, want /tmp/x.md", cfg.Out)
			}
		}},
		{"template", []string{"--template", "/tmp/t.md"}, func(t *testing.T, cfg ResearchConfig) {
			if cfg.Template != "/tmp/t.md" {
				t.Errorf("Template = %q, want /tmp/t.md", cfg.Template)
			}
		}},
		{"input", []string{"--input", "a.pdf", "--input", "b.png"}, func(t *testing.T, cfg ResearchConfig) {
			if len(cfg.Inputs) != 2 || cfg.Inputs[0] != "a.pdf" || cfg.Inputs[1] != "b.png" {
				t.Errorf("Inputs = %v, want [a.pdf b.png]", cfg.Inputs)
			}
		}},
		{"file-search", []string{"--file-search", "store-1"}, func(t *testing.T, cfg ResearchConfig) {
			if len(cfg.FileSearch) != 1 || cfg.FileSearch[0] != "store-1" {
				t.Errorf("FileSearch = %v, want [store-1]", cfg.FileSearch)
			}
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			fs := newFlagSet(t)
			if err := fs.Parse(tt.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := ApplyFlags(&cfg, fs); err != nil {
				t.Fatalf("ApplyFlags: %v", err)
			}
			tt.assert(t, cfg)

			// Only the flag under test may diverge from the defaults:
			// blank it back out and the config must equal Default().
			rest := cfg
			rest.BudgetGBP = 0
			rest.Timeout = Default().Timeout
			rest.Output = Default().Output
			rest.Out = ""
			rest.Template = ""
			rest.Inputs = nil
			rest.FileSearch = nil
			if !reflect.DeepEqual(rest, Default()) {
				t.Errorf("an unrelated field changed: %+v", cfg)
			}
		})
	}
}
