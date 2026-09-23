package config

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
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

// validFleet is validWorker plus the fan-out agent selection; the caps it
// enforces (MaxWorkers/Concurrency) already carry defaults.
func validFleet() ResearchConfig {
	cfg := validWorker()
	cfg.Agent = AgentFleet
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
		{"fleet with default fleet", func() ResearchConfig {
			cfg := Default()
			cfg.Agent = AgentFleet
			return cfg
		}()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

// TestValidateDeepResearchIgnoresFleet: a deep-research run must stay valid
// with a zero Fleet block — the fleet caps are never enforced for it, so a
// pipeline that never sets --agent worker/fleet is unaffected.
func TestValidateDeepResearchIgnoresFleet(t *testing.T) {
	cfg := Default()
	cfg.Agent = AgentDeepResearch
	cfg.Fleet = FleetConfig{} // deliberately zero: no memory, no caps.
	if err := cfg.Validate(); err != nil {
		t.Errorf("deep-research with a zero Fleet must validate: %v", err)
	}

	cfg.Agent = AgentDeepResearchMax
	if err := cfg.Validate(); err != nil {
		t.Errorf("deep-research-max with a zero Fleet must validate: %v", err)
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
		{"inmemory not implemented", func(c *ResearchConfig) { c.Fleet.Memory = MemoryInMemory }},
		{"model endpoint userinfo", func(c *ResearchConfig) {
			c.Fleet.ModelEndpoint = "https://user:hunter2@model.example/v1"
		}},
		{"model endpoint query", func(c *ResearchConfig) {
			c.Fleet.ModelEndpoint = "https://model.example/v1?api_key=x"
		}},
		{"search endpoint fragment", func(c *ResearchConfig) {
			c.Fleet.SearchEndpoint = "https://search.example/mcp#frag"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validWorker()
			tt.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("%s: validated", tt.name)
			}
		})
	}
}

// TestValidateFleetOnlyCaps: MaxWorkers/Concurrency bound fan-out, which a
// single worker has none of — so they are enforced for the fleet agent and
// ignored for the lone worker.
func TestValidateFleetOnlyCaps(t *testing.T) {
	for _, tt := range []struct {
		name       string
		mutate     func(*ResearchConfig)
		workerOK   bool // valid when Agent is worker
		fleetError bool // rejected when Agent is fleet
	}{
		{"max workers zero", func(c *ResearchConfig) { c.Fleet.MaxWorkers = 0 }, true, true},
		{"concurrency zero", func(c *ResearchConfig) { c.Fleet.Concurrency = 0 }, true, true},
		{"concurrency exceeds max workers", func(c *ResearchConfig) {
			c.Fleet.MaxWorkers = 2
			c.Fleet.Concurrency = 3
		}, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			worker := validWorker()
			tt.mutate(&worker)
			if err := worker.Validate(); (err == nil) != tt.workerOK {
				t.Errorf("worker: err = %v, want ok=%v", err, tt.workerOK)
			}

			fleet := validFleet()
			tt.mutate(&fleet)
			if err := fleet.Validate(); (err != nil) != tt.fleetError {
				t.Errorf("fleet: err = %v, want error=%v", err, tt.fleetError)
			}
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
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validWorker()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("literal key was accepted")
			}
			if strings.Contains(err.Error(), literal) {
				t.Errorf("validation error leaked the literal: %v", err)
			}
		})
	}
}

// TestFleetRoundTrip pins the new fields through EncodeJSON -> Decode: the
// research-config command emits JSON that the next pipeline stage decodes,
// so a dropped or misnamed fleet field would silently reset a paid run.
func TestFleetRoundTrip(t *testing.T) {
	in := validFleet()
	in.Query = "10BASE-T1L PHY vendors"
	in.Fleet.MaxTurns = 12
	in.Fleet.MaxTokens = 123_456
	in.Fleet.CeilingGBP = 2.25
	in.Fleet.WorkerTimeout = Duration(90 * time.Second)
	in.Fleet.MaxWorkers = 7
	in.Fleet.Concurrency = 4
	in.Fleet.Memory = MemoryInMemory

	var buf bytes.Buffer
	if err := in.EncodeJSON(&buf); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	out, err := Decode(&buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(out.Fleet, in.Fleet) {
		t.Errorf("fleet round trip lost data:\n got %+v\nwant %+v", out.Fleet, in.Fleet)
	}
}

// TestFleetDecodeRejectsUnknownKeys: the strict-decode contract still
// holds under the nested block — a typo inside fleet must be an error, not
// a silently ignored knob on a paid run.
func TestFleetDecodeRejectsUnknownKeys(t *testing.T) {
	const in = "agent: worker\nfleet:\n  max_trns: 4\n"
	if _, err := Decode(strings.NewReader(in)); err == nil {
		t.Error("a typo inside the fleet block was accepted silently")
	}
}

// TestValidateRejectsDeepResearchLeversForInProcessAgents: a Gemini-only
// spend or planning lever set alongside the worker or fleet agent is an
// error naming the lever, never a silent no-op.
func TestValidateRejectsDeepResearchLeversForInProcessAgents(t *testing.T) {
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
		{"template", func(c *ResearchConfig) { c.Template = "t.md" }, "template:"},
	} {
		for _, agent := range []string{AgentWorker, AgentFleet} {
			t.Run(tt.name+"/"+agent, func(t *testing.T) {
				cfg := validWorker()
				cfg.Agent = agent
				tt.mutate(&cfg)
				err := cfg.Validate()
				if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
					t.Errorf("Validate = %v, want an error starting %q", err, tt.want)
				}
			})
		}
	}
	// The same levers remain valid for the deep-research tiers.
	cfg := Default()
	cfg.BudgetGBP, cfg.Plan, cfg.Template = 2, true, "t.md"
	if err := cfg.Validate(); err != nil {
		t.Errorf("deep-research with levers: %v", err)
	}
}

// TestDefaultFleetIsBounded: the defaults alone cap turns, tokens, page size
// and wall clock, so a bare --agent worker run is bounded on every axis
// that needs no price table.
func TestDefaultFleetIsBounded(t *testing.T) {
	f := Default().Fleet
	if f.MaxTurns <= 0 || f.MaxTokens <= 0 || f.MaxPageBytes <= 0 || time.Duration(f.WorkerTimeout) <= 0 {
		t.Errorf("default fleet caps are not all positive: %+v", f)
	}
}
