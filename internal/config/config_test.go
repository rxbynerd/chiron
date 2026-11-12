package config

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestDecode_EmptyInput_YieldsDefaults(t *testing.T) {
	must := require.New(t)
	cfg, err := decode(strings.NewReader(""))
	must.NoError(err)
	must.Equal(Default(), cfg)
}

func TestDecode_OverlaysDefaults(t *testing.T) {
	// Absent keys must keep their defaults; present keys must override.
	must := require.New(t)
	check := assert.New(t)
	cfg, err := decode(strings.NewReader("agent: deep-research-max\nstream: false\n"))
	must.NoError(err)
	check.Equal(AgentDeepResearchMax, cfg.Agent)
	check.False(cfg.Stream, "stream: explicit false was clobbered")
	check.Equal(OutputText, cfg.Output)
	check.Equal(DefaultAPIKeyRef, cfg.APIKeyRef)
}

func TestDecode_RejectsUnknownKeys(t *testing.T) {
	must := require.New(t)
	_, err := decode(strings.NewReader("agnet: deep-research\n"))
	must.Error(err, "typo key was accepted silently")
}

func TestJSONRoundTripThroughDecode(t *testing.T) {
	// research-config emits JSON; the next pipeline stage decodes it.
	must := require.New(t)
	check := assert.New(t)
	in := Default()
	in.Query = "10BASE-T1L PHY vendors"
	in.Agent = AgentDeepResearchMax
	in.MCP = map[string]string{"corp": "https://mcp.internal"}
	in.Timeout = Duration(45 * time.Minute)

	var buf bytes.Buffer
	must.NoError(in.EncodeJSON(&buf))
	out, err := decode(&buf)
	must.NoError(err)
	check.Equal(in.Query, out.Query)
	check.Equal(in.Agent, out.Agent)
	check.Equal(in.Timeout, out.Timeout)
	check.Equal("https://mcp.internal", out.MCP["corp"])
}

func TestApplyFlagsOverlaysOnlySetFlags(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	base := Default()
	base.Agent = AgentDeepResearchMax
	base.Query = "from the pipe"

	fs := newFlagSet(t)
	must.NoError(fs.Parse([]string{"--visualise", "--quiet"}))
	must.NoError(ApplyFlags(&base, fs))

	check.True(base.Visualise, "visualise flag not applied")
	check.False(base.Stream, "--quiet must mean stream=false")
	// Unset flags must not reset piped base values to flag defaults.
	check.Equal(AgentDeepResearchMax, base.Agent, "unset flags clobbered the base")
	check.Equal("from the pipe", base.Query, "unset flags clobbered the base")
}

func TestApplyFlags_PlanningAndModelLevers(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := Default()
	fs := newFlagSet(t)
	must.NoError(fs.Parse([]string{"--plan", "--accept-plan", "--model", "gemini-3.1-pro-preview"}))
	must.NoError(ApplyFlags(&cfg, fs))
	check.True(cfg.Plan, "--plan not applied")
	check.True(cfg.AcceptPlan, "--accept-plan not applied")
	check.Equal("gemini-3.1-pro-preview", cfg.Model)
}

func TestApplyFlags_ParsesMCP(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := Default()
	fs := newFlagSet(t)
	must.NoError(fs.Parse([]string{"--mcp", "corp=https://mcp.internal", "--mcp", "docs=https://docs.internal"}))
	must.NoError(ApplyFlags(&cfg, fs))
	check.Equal("https://mcp.internal", cfg.MCP["corp"])
	check.Equal("https://docs.internal", cfg.MCP["docs"])

	fs = newFlagSet(t)
	must.NoError(fs.Parse([]string{"--mcp", "missing-equals"}))
	must.Error(ApplyFlags(&cfg, fs), "malformed --mcp entry was accepted")
}

func TestValidate(t *testing.T) {
	must := require.New(t)
	good := Default()
	must.NoError(good.Validate(), "defaults must validate")

	cases := map[string]func(*ResearchConfig){
		"unknown agent":     func(c *ResearchConfig) { c.Agent = "deep-thought" },
		"unknown output":    func(c *ResearchConfig) { c.Output = "xml" },
		"timeout above cap": func(c *ResearchConfig) { c.Timeout = Duration(2 * time.Hour) },
		"zero timeout":      func(c *ResearchConfig) { c.Timeout = 0 },
		"negative budget":   func(c *ResearchConfig) { c.BudgetGBP = -1 },
		"literal api key":   func(c *ResearchConfig) { c.APIKeyRef = "AIzaSyLiteralKey" },
		// MCP servers are remote endpoints; non-web schemes must fail locally.
		"mcp file url":       func(c *ResearchConfig) { c.MCP = map[string]string{"local": "file:///srv/mcp"} },
		"mcp javascript url": func(c *ResearchConfig) { c.MCP = map[string]string{"js": "javascript:alert(1)"} },
		"mcp bare host":      func(c *ResearchConfig) { c.MCP = map[string]string{"bare": "mcp.internal"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			cfg := Default()
			mutate(&cfg)
			must.Error(cfg.Validate())
		})
	}
}

func newFlagSet(t *testing.T) *pflag.FlagSet {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs)
	return fs
}

// TestResearchConfig_EncodeJSON_StdlibRoundTrip: pipeline tooling may decode
// research-config output with encoding/json rather than the YAML decoder.
// Durations then go through UnmarshalJSON, which must accept the string
// form EncodeJSON emits.
func TestResearchConfig_EncodeJSON_StdlibRoundTrip(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	in := Default()
	in.Query = "10BASE-T1L PHY vendors"
	in.Timeout = Duration(45 * time.Minute)

	var buf bytes.Buffer
	must.NoError(in.EncodeJSON(&buf))
	var got ResearchConfig
	must.NoError(json.Unmarshal(buf.Bytes(), &got))
	check.Equal(in.Timeout, got.Timeout, "UnmarshalJSON must parse the string duration form")
	check.Equal(in.Query, got.Query)
}

// TestDuration_UnmarshalJSON_Forms: both accepted wire forms, plus the
// rejection of anything else.
func TestDuration_UnmarshalJSON_Forms(t *testing.T) {
	t.Run("string form", func(t *testing.T) {
		must := require.New(t)
		var d Duration
		must.NoError(json.Unmarshal([]byte(`"30m"`), &d))
		must.Equal(30*time.Minute, time.Duration(d))
	})

	t.Run("nanosecond form", func(t *testing.T) {
		must := require.New(t)
		var d Duration
		must.NoError(json.Unmarshal([]byte(`60000000000`), &d))
		must.Equal(time.Minute, time.Duration(d))
	})

	t.Run("rejects garbage string", func(t *testing.T) {
		must := require.New(t)
		var d Duration
		must.Error(json.Unmarshal([]byte(`"not a duration"`), &d))
	})

	t.Run("rejects boolean", func(t *testing.T) {
		must := require.New(t)
		var d Duration
		must.Error(json.Unmarshal([]byte(`true`), &d))
	})
}

// TestDuration_MarshalYAML_UsesStringForm: the YAML form is the
// human-readable string, never a nanosecond integer.
func TestDuration_MarshalYAML_UsesStringForm(t *testing.T) {
	must := require.New(t)
	out, err := yaml.Marshal(Duration(30 * time.Minute))
	must.NoError(err)
	must.Equal("30m0s", strings.TrimSpace(string(out)))
}

// TestApplyFlags_OutputAndCostLevers: each flag is set alone and only its
// field may change. A transposed pflag binding or a type coercion bug must
// fail here, not three layers up the CLI stack.
func TestApplyFlags_OutputAndCostLevers(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		assert func(t *testing.T, cfg ResearchConfig)
	}{
		{"budget", []string{"--budget", "2.50"}, func(t *testing.T, cfg ResearchConfig) {
			must := require.New(t)
			must.InDelta(2.50, cfg.BudgetGBP, 0)
		}},
		{"timeout", []string{"--timeout", "45m"}, func(t *testing.T, cfg ResearchConfig) {
			must := require.New(t)
			must.Equal(45*time.Minute, time.Duration(cfg.Timeout))
		}},
		{"output", []string{"--output", "json"}, func(t *testing.T, cfg ResearchConfig) {
			must := require.New(t)
			must.Equal("json", cfg.Output)
		}},
		{"out", []string{"--out", "/tmp/x.md"}, func(t *testing.T, cfg ResearchConfig) {
			must := require.New(t)
			must.Equal("/tmp/x.md", cfg.Out)
		}},
		{"template", []string{"--template", "/tmp/t.md"}, func(t *testing.T, cfg ResearchConfig) {
			must := require.New(t)
			must.Equal("/tmp/t.md", cfg.Template)
		}},
		{"input", []string{"--input", "a.pdf", "--input", "b.png"}, func(t *testing.T, cfg ResearchConfig) {
			must := require.New(t)
			must.Equal([]string{"a.pdf", "b.png"}, cfg.Inputs)
		}},
		{"file-search", []string{"--file-search", "store-1"}, func(t *testing.T, cfg ResearchConfig) {
			must := require.New(t)
			must.Equal([]string{"store-1"}, cfg.FileSearch)
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			cfg := Default()
			fs := newFlagSet(t)
			must.NoError(fs.Parse(tt.args))
			must.NoError(ApplyFlags(&cfg, fs))
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
			must.Equal(Default(), rest, "an unrelated field changed")
		})
	}
}
