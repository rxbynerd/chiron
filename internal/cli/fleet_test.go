package cli

import (
	"bytes"
	"context"
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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	// holdSynthesis, when set, is closed by the first synthesis request,
	// which then gets no reply until its client gives up.
	holdSynthesis chan struct{}
	holdOnce      sync.Once

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
	if kind == fleetKindSynthesise && m.holdSynthesis != nil {
		m.holdOnce.Do(func() { close(m.holdSynthesis) })
		<-r.Context().Done()
		return
	}

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
	must := require.New(t)
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
	must.NoError(err, "marshal decomposition")
	return string(b)
}

// newCLIFleetModel returns a fleet model whose decomposition yields briefs
// briefs.
func newCLIFleetModel(t *testing.T, briefs int) *cliFleetModel {
	t.Helper()
	return &cliFleetModel{t: t, decompose: fleetDecomposition(t, briefs)}
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

// TestFleet_Run_ReportsCitedSynthesisAndUsage: --agent fleet over one loopback
// model endpoint produces a synthesised report citing every worker's source,
// worker-tagged progress between interaction_created and run_completed, and one
// usage rollup of the lead's and workers' calls.
func TestFleet_Run_ReportsCitedSynthesisAndUsage(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fm := newCLIFleetModel(t, 3)
	modelSrv := httptest.NewServer(fm)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(fleetSources)
	defer searchSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")

	stdout, stderr, err := execute(t, fleetArgs(modelSrv.URL, searchSrv, "-o", "text")...)
	must.NoError(err, "stderr:\n%s", stderr)

	for _, want := range []string{"agent: fleet", "interaction: flt_", "status: completed", "tools: [web_search, web_fetch]", fleetReportBody, "## Sources"} {
		check.Contains(stdout, want)
	}
	for _, src := range fleetSources {
		check.Contains(stdout, "["+src.Title+"]("+src.URL+")", "report does not cite %s, which a worker found", src.URL)
	}

	for kind, want := range map[string]int{fleetKindDecompose: 1, fleetKindWorker: 6, fleetKindSynthesise: 1, fleetKindCite: 1} {
		t.Run(kind, func(t *testing.T) {
			must := require.New(t)
			must.Len(fm.requestsOf(kind), want)
		})
	}
	check.Equal(3, searchSrv.ToolCallCount(), "one search tool call per worker")

	want := types.Usage{InputTokens: 100 + 6*10 + 300 + 150, OutputTokens: 40 + 6*5 + 200 + 50}
	check.Contains(stdout, fmt.Sprintf("tokens:\n  input: %d\n  output: %d\n", want.InputTokens, want.OutputTokens),
		"report tokens are not the rollup %+v", want)

	created, completed := -1, -1
	workerTurns := map[string]int{}
	var summary *types.Usage
	for i, ev := range decodeProgressStream(t, stderr) {
		switch ev.Kind {
		case transport.KindInteractionCreated:
			created = i
			check.Contains(string(ev.Payload), `"flt_`, "interaction_created payload lacks a fleet id")
		case transport.KindRunCompleted:
			completed = i
		case transport.KindCostSummary:
			var p struct {
				Usage types.Usage `json:"usage"`
			}
			must.NoError(json.Unmarshal(ev.Payload, &p), "cost_summary payload %s", ev.Payload)
			summary = &p.Usage
		case transport.KindDelta:
			var p workerTurnPayload
			must.NoError(json.Unmarshal(ev.Payload, &p), "delta payload %s", ev.Payload)
			if p.Type != "worker_turn" {
				continue
			}
			check.True(created >= 0 && completed < 0,
				"worker_turn delta at %d outside interaction_created and run_completed: %s", i, ev.Payload)
			check.True(strings.HasPrefix(p.WorkerID, "worker-") && strings.HasPrefix(p.Text, p.WorkerID+" turn "),
				"worker_turn delta does not name its worker: %+v", p)
			workerTurns[p.WorkerID]++
		}
	}
	check.Len(workerTurns, 3, "worker_turn deltas per worker: %v", workerTurns)
	for id, n := range workerTurns {
		check.Equal(2, n, "turn deltas for %s", id)
	}

	must.NotNil(summary, "stderr lacks cost_summary:\n%s", stderr)
	check.Equal(want.InputTokens, summary.InputTokens)
	check.Equal(want.OutputTokens, summary.OutputTokens)
	check.Equal(3, summary.SearchCount)
}

// TestFleet_Template_ShapesOnlySynthesis: --template reaches the lead's
// synthesis prompt, query substituted, and no worker's prompt.
func TestFleet_Template_ShapesOnlySynthesis(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fm := newCLIFleetModel(t, 3)
	modelSrv := httptest.NewServer(fm)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(fleetSources)
	defer searchSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	path := writeWorkerTemplate(t, "Answer \"{{.Query}}\" as a comparison table.\n")

	_, stderr, err := execute(t, fleetArgs(modelSrv.URL, searchSrv, "-o", "none", "--template", path)...)
	must.NoError(err, "stderr:\n%s", stderr)

	synth := fm.requestsOf(fleetKindSynthesise)
	must.Len(synth, 1)
	want := "Required output format:\nAnswer \"How do heat pumps compare with gas boilers?\" as a comparison table."
	check.Contains(synth[0].System, want)
	for _, req := range fm.requestsOf(fleetKindWorker) {
		check.NotContains(req.System, "comparison table", "a worker prompt carries the report template")
	}
}

// TestFleet_Decompose_OutOfBoundsExitsResearchFailed: a decomposition outside
// the brief bounds fails the run as a research outcome, and no worker runs.
func TestFleet_Decompose_OutOfBoundsExitsResearchFailed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fm := newCLIFleetModel(t, 2)
	modelSrv := httptest.NewServer(fm)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(fleetSources)
	defer searchSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")

	_, _, err := execute(t, fleetArgs(modelSrv.URL, searchSrv, "-o", "none")...)
	var exitErr *ExitError
	must.ErrorAs(err, &exitErr)
	must.Equal(ExitResearchFailed, exitErr.Code)

	check.Equal(1, fm.total(), "the decomposition should be the only model request")
	check.Zero(searchSrv.CallCount(), "no worker should search")
}

// executeContext is execute with ctx as the command's context. Ending ctx
// ends the run's context, as its --timeout deadline does.
func executeContext(ctx context.Context, t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCommand()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(args)
	err = root.ExecuteContext(ctx)
	return out.String(), errBuf.String(), err
}

var loggedCost = regexp.MustCompile(`estimated_cost_gbp=(\S+)`)

// TestFleet_Cancel_DuringSynthesisLogsSpend: when the run's context ends
// while synthesis is in flight, after every worker finished, no report
// reaches stdout and the run fails as a usage error; stderr carries the
// run's rolled-up spend, workers included, with no finding text, and no
// citation call is made.
func TestFleet_Cancel_DuringSynthesisLogsSpend(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fm := newCLIFleetModel(t, 3)
	fm.holdSynthesis = make(chan struct{})
	modelSrv := httptest.NewServer(fm)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(fleetSources)
	defer searchSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-fm.holdSynthesis:
			cancel()
		case <-ctx.Done():
		}
	}()
	stdout, stderr, err := executeContext(ctx, t, fleetArgs(modelSrv.URL, searchSrv,
		"--fleet-price-input", "2", "--fleet-price-output", "10")...)
	must.ErrorIs(err, context.Canceled, "stderr:\n%s", stderr)
	_, isExitErr := errors.AsType[*ExitError](err)
	check.False(isExitErr, "a run whose context ended is a usage error, not a research outcome")
	check.Empty(stdout, "no report should reach stdout")

	var spend string
	for line := range strings.Lines(stderr) {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "recorded spend") {
			spend = line
		}
	}
	id := regexp.MustCompile(`interaction_id=(flt_[0-9a-f]{32})`).FindStringSubmatch(spend)
	must.NotNil(id, "stderr lacks a spend line:\n%s", stderr)
	check.Contains(stderr, `{"interaction_id":"`+id[1]+`"}`, "the spend line should name the emitted interaction id")

	wantIn, wantOut := 100+6*10, 40+6*5
	wantSpend := []string{
		"status=incomplete",
		fmt.Sprintf("input_tokens=%d", wantIn),
		fmt.Sprintf("output_tokens=%d", wantOut),
		"search_count=3",
	}
	for _, want := range wantSpend {
		check.Contains(spend, want)
	}

	cost := loggedCost.FindStringSubmatch(spend)
	must.NotNil(cost, "spend line lacks the estimated cost: %s", spend)
	wantCost := float64(wantIn*2+wantOut*10) / 1e6
	got, err := strconv.ParseFloat(cost[1], 64)
	must.NoError(err)
	check.InDelta(wantCost, got, 1e-12, "logged cost %s", cost[1])

	check.NotContains(stderr, "Finding 1.", "stderr should carry no finding text")
	for kind, want := range map[string]int{fleetKindWorker: 6, fleetKindSynthesise: 1, fleetKindCite: 0} {
		t.Run(kind, func(t *testing.T) {
			must := require.New(t)
			must.Len(fm.requestsOf(kind), want)
		})
	}
}

// TestFleet_Config_RefusedBeforeAnyRequest: a fleet configuration the
// composition root cannot run fails as a usage error before the model or
// search endpoint is dialled.
func TestFleet_Config_RefusedBeforeAnyRequest(t *testing.T) {
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
			must := require.New(t)

			fm := newCLIFleetModel(t, 3)
			modelSrv := httptest.NewServer(fm)
			defer modelSrv.Close()
			searchSrv := searchtest.NewFakeServer(fleetSources)
			defer searchSrv.Close()
			t.Setenv("MODEL_KEY", "test-model-key")

			_, stderr, err := execute(t, tt.edit(fleetArgs(modelSrv.URL, searchSrv, "-o", "none"))...)
			must.Error(err, "the run was not refused")
			for _, want := range tt.want {
				must.ErrorContains(err, want)
			}
			_, isExitErr := errors.AsType[*ExitError](err)
			must.False(isExitErr, "a refused configuration is a usage error, not a research outcome: %v", err)
			must.Zero(fm.total(), "a refused fleet dialled the model endpoint")
			must.Zero(searchSrv.CallCount(), "a refused fleet dialled the search endpoint")
			must.NotContains(stderr, "flt_", "a refused fleet minted an id")
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

// TestFleetID_GetAndFollowUp_Refused: the commands that re-attach to
// server-side state refuse a fleet-minted id with a message naming the
// fleet, before resolving the Gemini key.
func TestFleetID_GetAndFollowUp_Refused(t *testing.T) {
	const id = "flt_0123456789abcdef0123456789abcdef"
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"get", []string{"get", id, "-o", "none"}},
		{"follow-up", []string{"follow-up", id, "--query", "more?", "-o", "none"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			_, _, err := execute(t, tt.args...)
			must.ErrorContains(err, tt.name+": "+id+" is an in-process fleet id")
		})
	}
}
