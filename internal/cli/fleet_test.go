package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/transport"
	"github.com/rxbynerd/chiron/internal/types"
)

// The request kinds a cliFleetModel routes on: the structured-output schema
// a request names, or a plain-text request for the synthesis.
const (
	fleetKindDecompose  = "lead_decomposition"
	fleetKindWorker     = "worker_action"
	fleetKindCite       = "lead_citations"
	fleetKindSynthesise = "synthesis"
)

// fleetSources are what every search in a fleet run returns; worker n cites
// fleetSources[n-1].
var fleetSources = []search.Result{
	{Title: "Heat pump running costs", URL: "https://example.org/heat-pumps", Snippet: "coefficient of performance"},
	{Title: "Gas boiler running costs", URL: "https://example.org/boilers", Snippet: "condensing boiler efficiency"},
	{Title: "Installation grants", URL: "https://example.org/grants", Snippet: "boiler upgrade scheme"},
}

// fleetUsage is what the scripted replies of each kind report; a worker
// reports it on every turn.
var fleetUsage = map[string]model.Usage{
	fleetKindDecompose:  {InputTokens: 100, OutputTokens: 40},
	fleetKindWorker:     {InputTokens: 10, OutputTokens: 5},
	fleetKindSynthesise: {InputTokens: 300, OutputTokens: 200},
	fleetKindCite:       {InputTokens: 150, OutputTokens: 50},
}

const fleetReportBody = `# Heat pumps and gas boilers

## Summary

A heat pump costs less to run than a gas boiler.

Grants cover part of a heat pump's installation.`

var fleetBriefObjective = regexp.MustCompile(`Objective (\d+)`)

// fleetRequest is one recorded model request: its system prompt and the
// length of its transcript.
type fleetRequest struct {
	System   string
	Messages int
}

// cliFleetModel is the one model endpoint a fleet run's lead and workers
// share. It answers by request kind and, for a worker, by the brief number
// its system prompt names, so no reply depends on arrival order. Like the
// provider, it rejects a strict schema that fails strict mode.
type cliFleetModel struct {
	t         *testing.T
	decompose string

	mu       sync.Mutex
	requests map[string][]fleetRequest
}

func (m *cliFleetModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		ResponseFormat *struct {
			JSONSchema struct {
				Name   string          `json:"name"`
				Schema json.RawMessage `json:"schema"`
				Strict bool            `json:"strict"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) == 0 {
		m.t.Errorf("decode model request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	kind := fleetKindSynthesise
	if rf := body.ResponseFormat; rf != nil {
		kind = rf.JSONSchema.Name
		if rf.JSONSchema.Strict {
			if err := model.ValidateStrictSchema(rf.JSONSchema.Schema); err != nil {
				m.t.Errorf("schema %s fails strict mode: %v", kind, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
	}
	system := body.Messages[0].Content
	m.mu.Lock()
	if m.requests == nil {
		m.requests = map[string][]fleetRequest{}
	}
	m.requests[kind] = append(m.requests[kind], fleetRequest{System: system, Messages: len(body.Messages)})
	m.mu.Unlock()

	var content string
	switch kind {
	case fleetKindDecompose:
		content = m.decompose
	case fleetKindWorker:
		content = m.workerAction(system, len(body.Messages))
	case fleetKindSynthesise:
		content = fleetReportBody
	case fleetKindCite:
		content = fmt.Sprintf(`{"claims":[{"claim":"A heat pump costs less to run than a gas boiler.","urls":[%q,%q]},{"claim":"Grants cover part of a heat pump's installation.","urls":[%q]}]}`,
			fleetSources[0].URL, fleetSources[1].URL, fleetSources[2].URL)
	default:
		m.t.Errorf("a request names the unknown schema %q", kind)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	usage := fleetUsage[kind]
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens":     usage.InputTokens,
			"completion_tokens": usage.OutputTokens,
			"total_tokens":      usage.InputTokens + usage.OutputTokens,
		},
	})
}

// workerAction searches on a worker's first turn and then answers citing
// the worker's own source.
func (m *cliFleetModel) workerAction(system string, messages int) string {
	match := fleetBriefObjective.FindStringSubmatch(system)
	if match == nil {
		m.t.Error("a worker request names no brief objective")
		return ""
	}
	n, _ := strconv.Atoi(match[1])
	if messages <= 2 {
		return fmt.Sprintf(`{"action":"search","query":"topic %d","url":null,"answer":null,"citations":null}`, n)
	}
	src := fleetSources[n-1]
	return fmt.Sprintf(`{"action":"final","query":null,"url":null,"answer":"Finding %d.","citations":[{"url":%q,"title":%q}]}`, n, src.URL, src.Title)
}

// requestsOf returns the recorded requests of one kind.
func (m *cliFleetModel) requestsOf(kind string) []fleetRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]fleetRequest(nil), m.requests[kind]...)
}

// total counts every recorded request.
func (m *cliFleetModel) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, reqs := range m.requests {
		n += len(reqs)
	}
	return n
}

// fleetDecomposition is a decompose reply of n briefs whose objectives are
// numbered from 1.
func fleetDecomposition(t *testing.T, n int) string {
	t.Helper()
	briefs := make([]map[string]string, n)
	for i := range briefs {
		briefs[i] = map[string]string{
			"objective":       fmt.Sprintf("Objective %d", i+1),
			"output_format":   "A short paragraph.",
			"source_guidance": "Prefer primary sources.",
			"boundaries":      "UK homes only.",
			"target":          "external_web",
		}
	}
	b, err := json.Marshal(map[string]any{"briefs": briefs})
	if err != nil {
		t.Fatalf("marshal decomposition: %v", err)
	}
	return string(b)
}

// newCLIFleetModel serves a fleet model whose decomposition yields briefs
// briefs.
func newCLIFleetModel(t *testing.T, briefs int) (*cliFleetModel, *httptest.Server) {
	t.Helper()
	m := &cliFleetModel{t: t, decompose: fleetDecomposition(t, briefs)}
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return m, srv
}

// fleetArgs returns the flags that point --agent fleet at the given fakes.
func fleetArgs(modelURL string, searchSrv *searchtest.FakeServer, extra ...string) []string {
	args := []string{
		"research", "--query", "How do heat pumps compare with gas boilers?",
		"--agent", "fleet", "--fleet-memory", "inmemory",
		"--fleet-max-workers", "3", "--fleet-concurrency", "2",
		"--fleet-model-endpoint", modelURL,
		"--fleet-model-name", "test-model",
		"--fleet-model-key-ref", "secret://MODEL_KEY",
		"--fleet-search-endpoint", searchSrv.URL(),
	}
	return append(args, extra...)
}

// TestFleetAgentThroughCLI drives --agent fleet through the compiled command
// tree against one loopback model endpoint and a fake search MCP: the lead
// decomposes into three briefs, three workers each search and answer citing
// their own source, and the lead synthesises and cites one report. Progress
// names each worker and flows only after the id is emitted, and the run's
// usage is the lead's and workers' calls rolled up once.
func TestFleetAgentThroughCLI(t *testing.T) {
	fm, modelSrv := newCLIFleetModel(t, 3)
	searchSrv := searchtest.NewFakeServer(fleetSources)
	defer searchSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")

	stdout, stderr, err := execute(t, fleetArgs(modelSrv.URL, searchSrv, "-o", "text")...)
	if err != nil {
		t.Fatalf("research --agent fleet: %v\nstderr: %s", err, stderr)
	}

	for _, want := range []string{"agent: fleet", "interaction: flt_", "status: completed", "tools: [web_search, web_fetch]", fleetReportBody, "## Sources"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report lacks %q:\n%s", want, stdout)
		}
	}
	for _, src := range fleetSources {
		if !strings.Contains(stdout, "["+src.Title+"]("+src.URL+")") {
			t.Errorf("report does not cite %s, which a worker found:\n%s", src.URL, stdout)
		}
	}

	for kind, want := range map[string]int{fleetKindDecompose: 1, fleetKindWorker: 6, fleetKindSynthesise: 1, fleetKindCite: 1} {
		if got := len(fm.requestsOf(kind)); got != want {
			t.Errorf("%s requests = %d, want %d", kind, got, want)
		}
	}
	if got := searchSrv.ToolCallCount(); got != 3 {
		t.Errorf("search tool calls = %d, want one per worker", got)
	}

	want := types.Usage{InputTokens: 100 + 6*10 + 300 + 150, OutputTokens: 40 + 6*5 + 200 + 50}
	if !strings.Contains(stdout, fmt.Sprintf("tokens:\n  input: %d\n  output: %d\n", want.InputTokens, want.OutputTokens)) {
		t.Errorf("report tokens are not the rollup %+v:\n%s", want, stdout)
	}

	created, completed := -1, -1
	workerTurns := map[string]int{}
	var summary *types.Usage
	for i, ev := range decodeProgressStream(t, stderr) {
		switch ev.Kind {
		case transport.KindInteractionCreated:
			created = i
			if !strings.Contains(string(ev.Payload), `"flt_`) {
				t.Errorf("interaction_created payload lacks a fleet id: %s", ev.Payload)
			}
		case transport.KindRunCompleted:
			completed = i
		case transport.KindCostSummary:
			var p struct {
				Usage types.Usage `json:"usage"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatalf("cost_summary payload %s: %v", ev.Payload, err)
			}
			summary = &p.Usage
		case transport.KindDelta:
			var p workerTurnPayload
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatalf("delta payload %s: %v", ev.Payload, err)
			}
			if p.Type != "worker_turn" {
				continue
			}
			if created < 0 || completed >= 0 {
				t.Errorf("worker_turn delta at %d outside interaction_created and run_completed: %s", i, ev.Payload)
			}
			if !strings.HasPrefix(p.WorkerID, "worker-") || !strings.HasPrefix(p.Text, p.WorkerID+" turn ") {
				t.Errorf("worker_turn delta does not name its worker: %+v", p)
			}
			workerTurns[p.WorkerID]++
		}
	}
	if len(workerTurns) != 3 {
		t.Errorf("worker_turn deltas per worker = %v, want three workers", workerTurns)
	}
	for id, n := range workerTurns {
		if n != 2 {
			t.Errorf("%s reported %d turns, want 2", id, n)
		}
	}
	if summary == nil {
		t.Fatalf("stderr lacks cost_summary:\n%s", stderr)
	}
	if summary.InputTokens != want.InputTokens || summary.OutputTokens != want.OutputTokens || summary.SearchCount != 3 {
		t.Errorf("cost_summary usage = %+v, want the rollup %+v with 3 searches", *summary, want)
	}
}

// TestFleetTemplateShapesOnlyTheSynthesis: --template reaches the lead's
// synthesis prompt, query substituted, and no worker's prompt.
func TestFleetTemplateShapesOnlyTheSynthesis(t *testing.T) {
	fm, modelSrv := newCLIFleetModel(t, 3)
	searchSrv := searchtest.NewFakeServer(fleetSources)
	defer searchSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	path := writeWorkerTemplate(t, "Answer \"{{.Query}}\" as a comparison table.\n")

	_, stderr, err := execute(t, fleetArgs(modelSrv.URL, searchSrv, "-o", "none", "--template", path)...)
	if err != nil {
		t.Fatalf("research --agent fleet --template: %v\nstderr: %s", err, stderr)
	}
	synth := fm.requestsOf(fleetKindSynthesise)
	if len(synth) != 1 {
		t.Fatalf("synthesis requests = %d, want 1", len(synth))
	}
	want := "Required output format:\nAnswer \"How do heat pumps compare with gas boilers?\" as a comparison table."
	if !strings.Contains(synth[0].System, want) {
		t.Errorf("synthesis prompt lacks the rendered template %q:\n%s", want, synth[0].System)
	}
	for _, req := range fm.requestsOf(fleetKindWorker) {
		if strings.Contains(req.System, "comparison table") {
			t.Errorf("a worker prompt carries the report template:\n%s", req.System)
		}
	}
}

// TestFleetFailedDecompositionExitCode: a decomposition outside the brief
// bounds fails the run as a research outcome, and no worker runs.
func TestFleetFailedDecompositionExitCode(t *testing.T) {
	fm, modelSrv := newCLIFleetModel(t, 2)
	searchSrv := searchtest.NewFakeServer(fleetSources)
	defer searchSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")

	_, _, err := execute(t, fleetArgs(modelSrv.URL, searchSrv, "-o", "none")...)
	exitErr, ok := errors.AsType[*ExitError](err)
	if !ok || exitErr.Code != ExitResearchFailed {
		t.Fatalf("err = %v, want exit code %d", err, ExitResearchFailed)
	}
	if n := fm.total(); n != 1 {
		t.Errorf("model requests = %d, want only the decomposition", n)
	}
	if n := searchSrv.CallCount(); n != 0 {
		t.Errorf("search calls = %d, want 0", n)
	}
}

// TestFleetRefusedBeforeAnyRequest: a fleet configuration the composition
// root cannot run fails as a usage error before the model or search endpoint
// is dialled.
func TestFleetRefusedBeforeAnyRequest(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func([]string) []string
		want []string
	}{
		{"missing model name", func(args []string) []string { return withoutFlag(args, "--fleet-model-name") }, []string{"research --agent fleet:", "fleet.model_name"}},
		{"noop memory", func(args []string) []string { return append(args, "--fleet-memory", "noop") }, []string{"fleet.memory:", "--fleet-memory inmemory"}},
		{"knowledge remember", func(args []string) []string {
			return append(args, "--fleet-knowledge-provider", "billet", "--fleet-knowledge-endpoint", "http://127.0.0.1:1/", "--fleet-knowledge-remember")
		}, []string{"fleet.knowledge_remember:", `"worker"`}},
		{"concurrency above workers", func(args []string) []string { return append(args, "--fleet-concurrency", "4") }, []string{"fleet.concurrency:"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fm, modelSrv := newCLIFleetModel(t, 3)
			searchSrv := searchtest.NewFakeServer(fleetSources)
			defer searchSrv.Close()
			t.Setenv("MODEL_KEY", "test-model-key")

			_, stderr, err := execute(t, tt.edit(fleetArgs(modelSrv.URL, searchSrv, "-o", "none"))...)
			if err == nil {
				t.Fatal("the run was not refused")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to contain %s", err, want)
				}
			}
			if _, ok := errors.AsType[*ExitError](err); ok {
				t.Errorf("a refused configuration is a usage error, not a research outcome: %v", err)
			}
			if fm.total() != 0 || searchSrv.CallCount() != 0 {
				t.Error("a refused fleet dialled an endpoint")
			}
			if strings.Contains(stderr, "flt_") {
				t.Errorf("a refused fleet minted an id:\n%s", stderr)
			}
		})
	}
}

// withoutFlag drops flag and its value from args.
func withoutFlag(args []string, flag string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// TestGetAndFollowUpRefuseFleetIDs: the commands that re-attach to
// server-side state refuse a fleet-minted id with a message naming the
// fleet, before resolving the Gemini key.
func TestGetAndFollowUpRefuseFleetIDs(t *testing.T) {
	const id = "flt_0123456789abcdef0123456789abcdef"
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"get", []string{"get", id, "-o", "none"}},
		{"follow-up", []string{"follow-up", id, "--query", "more?", "-o", "none"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := execute(t, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.name+": "+id+" is an in-process fleet id") {
				t.Fatalf("err = %v, want the fleet-id refusal", err)
			}
		})
	}
}
