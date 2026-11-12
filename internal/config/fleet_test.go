package config

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// validWorker returns a Default config selecting the worker agent with a
// fully populated, valid Fleet block — the baseline every negative case
// perturbs one field of.
func validWorker() ResearchConfig {
	cfg := Default()
	cfg.Agent = AgentWorker
	cfg.Fleet.ModelEndpoint = "https://model.example"
	cfg.Fleet.ModelName = "gpt-5.5"
	cfg.Fleet.ModelKeyRef = "secret://MODEL_API_KEY"
	cfg.Fleet.SearchEndpoint = "https://search.example"
	cfg.Fleet.SearchKeyRef = "secret://SEARCH_API_KEY"
	cfg.Fleet.MaxTokens = 200_000
	cfg.Fleet.CeilingGBP = 1.50
	cfg.Fleet.PriceInputGBPPerMTok = 1.2
	cfg.Fleet.PriceOutputGBPPerMTok = 9.6
	return cfg
}

// validFleet is validWorker plus the fan-out agent selection and the
// inmemory binding it requires; the caps it enforces
// (MaxWorkers/Concurrency) already carry defaults.
func validFleet() ResearchConfig {
	return withAgent(validWorker(), AgentFleet)
}

// withAgent selects agent on cfg, adding the inmemory binding the fleet
// requires, so one baseline serves both in-process agents.
func withAgent(cfg ResearchConfig, agent string) ResearchConfig {
	cfg.Agent = agent
	if agent == AgentFleet {
		cfg.Fleet.Memory = MemoryInMemory
	}
	return cfg
}

func TestValidateAcceptsWorkerAndFleet(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  ResearchConfig
	}{
		{"worker fully populated", validWorker()},
		{"fleet fully populated", validFleet()},
		{"worker with default fleet", func() ResearchConfig {
			cfg := Default()
			cfg.Agent = AgentWorker
			return cfg
		}()},
		{"fleet with default fleet and inmemory", func() ResearchConfig {
			cfg := Default()
			cfg.Agent = AgentFleet
			cfg.Fleet.Memory = MemoryInMemory
			return cfg
		}()},
		{"search tool at the 64-byte bound", func() ResearchConfig {
			cfg := validWorker()
			cfg.Fleet.SearchTool = strings.Repeat("a", 64)
			return cfg
		}()},
		{"search query arg at the 64-byte bound", func() ResearchConfig {
			cfg := validWorker()
			cfg.Fleet.SearchQueryArg = strings.Repeat("q", 64)
			return cfg
		}()},
		{"search tool combined charset", func() ResearchConfig {
			cfg := validWorker()
			cfg.Fleet.SearchTool = "a.b-c_d"
			return cfg
		}()},
		{"search query arg combined charset", func() ResearchConfig {
			cfg := validWorker()
			cfg.Fleet.SearchQueryArg = "a.b-c_d"
			return cfg
		}()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.NoError(tt.cfg.Validate())
		})
	}
}

// TestConfig_Validate_DeepResearchIgnoresFleet: a deep-research run must stay
// valid with a zero Fleet block — the fleet caps are never enforced for it, so
// a pipeline that never sets --agent worker/fleet is unaffected.
func TestConfig_Validate_DeepResearchIgnoresFleet(t *testing.T) {
	for _, agent := range []string{AgentDeepResearch, AgentDeepResearchMax} {
		t.Run(agent, func(t *testing.T) {
			must := require.New(t)
			cfg := Default()
			cfg.Agent = agent
			cfg.Fleet = FleetConfig{} // deliberately zero: no memory, no caps.
			must.NoError(cfg.Validate())
		})
	}
}

func TestValidateRejectsBadFleet(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*ResearchConfig)
	}{
		{"model endpoint cleartext non-loopback", func(c *ResearchConfig) {
			c.Fleet.ModelEndpoint = "http://model.example"
		}},
		{"model endpoint ssrf metadata", func(c *ResearchConfig) {
			c.Fleet.ModelEndpoint = "http://169.254.169.254"
		}},
		{"model endpoint non-http scheme", func(c *ResearchConfig) {
			c.Fleet.ModelEndpoint = "ftp://model.example"
		}},
		{"model endpoint no host", func(c *ResearchConfig) {
			c.Fleet.ModelEndpoint = "https://"
		}},
		{"search endpoint cleartext", func(c *ResearchConfig) {
			c.Fleet.SearchEndpoint = "http://search.example"
		}},
		{"model key literal", func(c *ResearchConfig) {
			c.Fleet.ModelKeyRef = "sk-literal-key"
		}},
		{"search key literal", func(c *ResearchConfig) {
			c.Fleet.SearchKeyRef = "literal"
		}},
		{"max turns zero", func(c *ResearchConfig) { c.Fleet.MaxTurns = 0 }},
		{"max turns negative", func(c *ResearchConfig) { c.Fleet.MaxTurns = -1 }},
		{"max tokens negative", func(c *ResearchConfig) { c.Fleet.MaxTokens = -1 }},
		{"ceiling negative", func(c *ResearchConfig) { c.Fleet.CeilingGBP = -0.5 }},
		{"ceiling without a price", func(c *ResearchConfig) {
			c.Fleet.PriceInputGBPPerMTok, c.Fleet.PriceOutputGBPPerMTok = 0, 0
		}},
		{"price negative", func(c *ResearchConfig) { c.Fleet.PriceOutputGBPPerMTok = -1 }},
		{"page bytes zero", func(c *ResearchConfig) { c.Fleet.MaxPageBytes = 0 }},
		{"worker timeout zero", func(c *ResearchConfig) { c.Fleet.WorkerTimeout = 0 }},
		{"worker timeout negative", func(c *ResearchConfig) { c.Fleet.WorkerTimeout = Duration(-time.Second) }},
		{"bad memory enum", func(c *ResearchConfig) { c.Fleet.Memory = "redis" }},
		{"model endpoint userinfo", func(c *ResearchConfig) {
			c.Fleet.ModelEndpoint = "https://user:hunter2@model.example/v1"
		}},
		{"model endpoint query", func(c *ResearchConfig) {
			c.Fleet.ModelEndpoint = "https://model.example/v1?api_key=x"
		}},
		{"search endpoint fragment", func(c *ResearchConfig) {
			c.Fleet.SearchEndpoint = "https://search.example/mcp#frag"
		}},
		{"model endpoint empty query", func(c *ResearchConfig) {
			c.Fleet.ModelEndpoint = "https://model.example/v1?"
		}},
		{"search endpoint empty fragment", func(c *ResearchConfig) {
			c.Fleet.SearchEndpoint = "https://search.example/mcp#"
		}},
		{"model endpoint port without hostname", func(c *ResearchConfig) {
			c.Fleet.ModelEndpoint = "https://:443/v1"
		}},
		{"search tool empty", func(c *ResearchConfig) { c.Fleet.SearchTool = "" }},
		{"search tool too long", func(c *ResearchConfig) { c.Fleet.SearchTool = strings.Repeat("a", 65) }},
		{"search tool bad characters", func(c *ResearchConfig) { c.Fleet.SearchTool = "tool name" }},
		{"search tool unicode", func(c *ResearchConfig) { c.Fleet.SearchTool = "café" }},
		{"search tool slash", func(c *ResearchConfig) { c.Fleet.SearchTool = "a/b" }},
		{"search query arg empty", func(c *ResearchConfig) { c.Fleet.SearchQueryArg = "" }},
		{"search query arg too long", func(c *ResearchConfig) { c.Fleet.SearchQueryArg = strings.Repeat("q", 65) }},
		{"search query arg whitespace", func(c *ResearchConfig) { c.Fleet.SearchQueryArg = "query arg" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			cfg := validWorker()
			tt.mutate(&cfg)
			must.Error(cfg.Validate())
		})
	}
}

// TestConfig_Validate_MemoryBinding: the fleet passes findings by reference,
// so it requires inmemory and rejects noop with an error naming the fix; the
// worker has no session plane and accepts either; unknown values are
// rejected for both.
func TestConfig_Validate_MemoryBinding(t *testing.T) {
	for _, tt := range []struct {
		name    string
		agent   string
		memory  string
		wantErr []string // substrings; empty means valid
	}{
		{"worker noop", AgentWorker, MemoryNoop, nil},
		{"worker inmemory", AgentWorker, MemoryInMemory, nil},
		{"fleet inmemory", AgentFleet, MemoryInMemory, nil},
		{"fleet noop", AgentFleet, MemoryNoop, []string{"fleet.memory:", `"inmemory"`, "--fleet-memory inmemory"}},
		{"worker unknown", AgentWorker, "redis", []string{"fleet.memory:", `"redis"`}},
		{"fleet unknown", AgentFleet, "redis", []string{"fleet.memory:", `"redis"`}},
		{"worker empty", AgentWorker, "", []string{"fleet.memory:"}},
		{"fleet empty", AgentFleet, "", []string{"fleet.memory:"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			cfg := validWorker()
			cfg.Agent = tt.agent
			cfg.Fleet.Memory = tt.memory
			err := cfg.Validate()
			if len(tt.wantErr) == 0 {
				must.NoError(err)
				return
			}
			must.Error(err)
			for _, want := range tt.wantErr {
				must.ErrorContains(err, want)
			}
		})
	}
}

// TestConfig_Validate_FleetRejectsDefaultMemory: noop stays the default, so a bare
// fleet config is rejected until inmemory is chosen.
func TestConfig_Validate_FleetRejectsDefaultMemory(t *testing.T) {
	must := require.New(t)
	must.Equal(MemoryNoop, Default().Fleet.Memory)

	cfg := Default()
	cfg.Agent = AgentFleet
	err := cfg.Validate()
	must.Error(err)
	must.True(strings.HasPrefix(err.Error(), "fleet.memory:"), "err = %v, want a fleet.memory error", err)
}

// MaxWorkers/Concurrency bound fan-out, which a single worker has none of — so
// they are enforced for the fleet agent and ignored for the lone worker.
func TestConfig_Validate_FleetOnlyCapsForFleetAgent(t *testing.T) {
	for _, tt := range []struct {
		name          string
		mutate        func(*ResearchConfig)
		workerValid   bool // valid when Agent is worker
		fleetRejected bool // rejected when Agent is fleet
	}{
		{"max workers zero", func(c *ResearchConfig) { c.Fleet.MaxWorkers = 0 }, true, true},
		{"concurrency zero", func(c *ResearchConfig) { c.Fleet.Concurrency = 0 }, true, true},
		{"concurrency exceeds max workers", func(c *ResearchConfig) {
			c.Fleet.MaxWorkers = 2
			c.Fleet.Concurrency = 3
		}, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			worker := validWorker()
			tt.mutate(&worker)
			workerErr := worker.Validate()
			must.Equal(tt.workerValid, workerErr == nil, "worker: err = %v", workerErr)

			fleet := validFleet()
			tt.mutate(&fleet)
			fleetErr := fleet.Validate()
			must.Equal(tt.fleetRejected, fleetErr != nil, "fleet: err = %v", fleetErr)
		})
	}
}

// TestValidateErrorsNeverEchoSecrets: a literal pasted into a key field
// must not surface in the validation error — the same scrub guarantee the
// api_key_ref rule carries.
func TestValidateErrorsNeverEchoSecrets(t *testing.T) {
	const literal = "AIzaSyA1234567890abcdefghijklmnopqrstuv"
	for _, tt := range []struct {
		name   string
		mutate func(*ResearchConfig)
	}{
		{"model key", func(c *ResearchConfig) { c.Fleet.ModelKeyRef = literal }},
		{"search key", func(c *ResearchConfig) { c.Fleet.SearchKeyRef = literal }},
		{"model endpoint query", func(c *ResearchConfig) { c.Fleet.ModelEndpoint = "https://model.example/v1?key=" + literal }},
		{"search endpoint path over cleartext", func(c *ResearchConfig) { c.Fleet.SearchEndpoint = "http://search.example/" + literal }},
		{"model endpoint host hidden by an at sign", func(c *ResearchConfig) { c.Fleet.ModelEndpoint = "https://" + literal + "#@model.example" }},
		{"search tool", func(c *ResearchConfig) { c.Fleet.SearchTool = literal + " " + literal }},
		{"search query arg", func(c *ResearchConfig) { c.Fleet.SearchQueryArg = literal + "/" + literal }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			cfg := validWorker()
			tt.mutate(&cfg)
			err := cfg.Validate()
			must.Error(err, "literal key was accepted")
			must.NotContains(err.Error(), literal, "validation error leaked the literal")
		})
	}
}

// TestConfig_EncodeJSON_FleetRoundTrip pins the fleet fields through
// EncodeJSON and decode: the research-config command emits JSON that the next
// pipeline stage decodes, so a dropped or misnamed fleet field would silently
// reset a paid run.
func TestConfig_EncodeJSON_FleetRoundTrip(t *testing.T) {
	must := require.New(t)
	in := validFleet()
	in.Query = "10BASE-T1L PHY vendors"
	in.Fleet.MaxTurns = 12
	in.Fleet.MaxTokens = 123_456
	in.Fleet.CeilingGBP = 2.25
	in.Fleet.WorkerTimeout = Duration(90 * time.Second)
	in.Fleet.MaxWorkers = 7
	in.Fleet.Concurrency = 4
	in.Fleet.Memory = MemoryInMemory
	in.Fleet.SearchTool = "web_search"
	in.Fleet.SearchQueryArg = "q"

	var buf bytes.Buffer
	must.NoError(in.EncodeJSON(&buf))
	out, err := decode(&buf)
	must.NoError(err)
	must.Equal(in.Fleet, out.Fleet, "fleet round trip lost data")
}

// TestConfig_Decode_RejectsUnknownFleetKeys: the strict-decode contract still
// holds under the nested block — a typo inside fleet must be an error, not
// a silently ignored knob on a paid run.
func TestConfig_Decode_RejectsUnknownFleetKeys(t *testing.T) {
	must := require.New(t)
	const in = "agent: worker\nfleet:\n  max_trns: 4\n"
	_, err := decode(strings.NewReader(in))
	must.Error(err, "a typo inside the fleet block was accepted silently")
}

// TestConfig_Validate_RejectsDeepResearchLeversForInProcessAgents: a Gemini-only
// spend or planning lever set alongside the worker or fleet agent is an
// error naming the lever, never a silent no-op.
func TestConfig_Validate_RejectsDeepResearchLeversForInProcessAgents(t *testing.T) {
	must := require.New(t)
	for _, tt := range []struct {
		name   string
		mutate func(*ResearchConfig)
		want   string
	}{
		{"budget", func(c *ResearchConfig) { c.BudgetGBP = 2 }, "budget:"},
		{"plan", func(c *ResearchConfig) { c.Plan = true }, "plan:"},
		{"accept_plan", func(c *ResearchConfig) { c.AcceptPlan = true }, "accept_plan:"},
		{"model", func(c *ResearchConfig) { c.Model = "gemini-2.5-pro" }, "model:"},
		{"visualise", func(c *ResearchConfig) { c.Visualise = true }, "visualise:"},
		{"tools", func(c *ResearchConfig) { c.Tools = []string{"google_search"} }, "tools:"},
		{"mcp", func(c *ResearchConfig) { c.MCP = map[string]string{"x": "https://x"} }, "mcp:"},
		{"file_search", func(c *ResearchConfig) { c.FileSearch = []string{"store"} }, "file_search:"},
		{"inputs", func(c *ResearchConfig) { c.Inputs = []string{"doc.pdf"} }, "inputs:"},
	} {
		for _, agent := range []string{AgentWorker, AgentFleet} {
			t.Run(tt.name+"/"+agent, func(t *testing.T) {
				must := require.New(t)
				cfg := withAgent(validWorker(), agent)
				tt.mutate(&cfg)
				err := cfg.Validate()
				must.Error(err)
				must.True(strings.HasPrefix(err.Error(), tt.want), "err = %v, want an error starting %q", err, tt.want)
			})
		}
	}

	// The same levers remain valid for the deep-research tiers.
	cfg := Default()
	cfg.BudgetGBP, cfg.Plan, cfg.Template = 2, true, "t.md"
	must.NoError(cfg.Validate())
}

// TestConfig_Validate_AcceptsTemplateForInProcessAgents: template shapes the worker's
// output-format block, so it is valid for worker and fleet.
func TestConfig_Validate_AcceptsTemplateForInProcessAgents(t *testing.T) {
	for _, agent := range []string{AgentWorker, AgentFleet} {
		t.Run(agent, func(t *testing.T) {
			must := require.New(t)
			cfg := withAgent(validWorker(), agent)
			cfg.Template = "t.md"
			must.NoError(cfg.Validate())
		})
	}
}

// TestConfig_Default_FleetIsBounded: the defaults alone cap turns, tokens, page
// size and wall clock, so a bare --agent worker run is bounded on every axis
// that needs no price table.
func TestConfig_Default_FleetIsBounded(t *testing.T) {
	must := require.New(t)
	f := Default().Fleet
	must.Positive(f.MaxTurns, "MaxTurns")
	must.Positive(f.MaxTokens, "MaxTokens")
	must.Positive(f.MaxPageBytes, "MaxPageBytes")
	must.Positive(time.Duration(f.WorkerTimeout), "WorkerTimeout")
}

// TestConfig_Default_FleetSearchIdentifiers: a bare --agent worker run already
// names the reference search MCP's tool and argument, so it needs no
// override to reach the documented default backend (docs/DECISIONS.md,
// 2026-09-25 "SP-A").
func TestConfig_Default_FleetSearchIdentifiers(t *testing.T) {
	must := require.New(t)
	f := Default().Fleet
	must.Equal(DefaultSearchTool, f.SearchTool, "search tool")
	must.Equal(DefaultSearchQueryArg, f.SearchQueryArg, "search query arg")
}

// TestConfig_Decode_FleetSearchIdentifiersDefaultOnPartialBlock: a fleet block
// that sets other fields and omits search_tool/search_query_arg must still
// resolve to their documented defaults, not a zero value that would fail
// validation.
func TestConfig_Decode_FleetSearchIdentifiersDefaultOnPartialBlock(t *testing.T) {
	must := require.New(t)
	const in = "agent: worker\nfleet:\n  search_endpoint: https://search.example\n"
	cfg, err := decode(strings.NewReader(in))
	must.NoError(err)
	f := cfg.Fleet
	must.Equal(DefaultSearchTool, f.SearchTool, "search tool")
	must.Equal(DefaultSearchQueryArg, f.SearchQueryArg, "search query arg")
}
