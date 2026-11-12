package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/formatter"
	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/types"
)

// The synthetic fleet run's sources: each worker cites its own.
const (
	heatPumpURL = "https://example.org/heat-pumps"
	boilerURL   = "https://example.org/boilers"
)

// syntheticSearchResults are what every search in the synthetic fleet run
// returns, so each worker has seen both sources.
var syntheticSearchResults = []search.Result{
	{Title: "Heat pump running costs", URL: heatPumpURL, Snippet: "coefficient of performance"},
	{Title: "Gas boiler running costs", URL: boilerURL, Snippet: "condensing boiler efficiency"},
}

// syntheticReport is the synthetic fleet run's synthesised body.
const syntheticReport = `# Heat pumps and gas boilers for UK homes

## Summary

An air-source heat pump costs less to run than a gas boiler in a typical UK home, though it costs more to install.

## Running costs

A heat pump delivers around three units of heat for each unit of electricity it uses, which offsets the higher unit price of electricity.

A gas boiler converts around nine-tenths of its fuel into heat, so its running cost follows the gas price closely.

## Gaps in coverage

The research did not cover installation grants, so this report does not compare upfront costs after support.`

// workerFinal is a worker's final action answering with answer and citing
// url under title.
func workerFinal(t *testing.T, answer, url, title string) string {
	t.Helper()
	must := require.New(t)
	b, err := json.Marshal(map[string]any{
		"action":    "final",
		"query":     nil,
		"url":       nil,
		"answer":    answer,
		"citations": []map[string]string{{"url": url, "title": title}},
	})
	must.NoError(err, "marshal final action")
	return string(b)
}

// syntheticFleetModel scripts a fleet run of three briefs: each worker
// searches once, then answers citing its own source; the synthesis writes
// syntheticReport; the citation pass attributes claims to both workers'
// sources and one to a URL no worker cited.
func syntheticFleetModel(t *testing.T) *fleetModel {
	finals := map[int]string{
		1: workerFinal(t, "A heat pump delivers around three units of heat per unit of electricity.", heatPumpURL, "Heat pump running costs"),
		2: workerFinal(t, "A condensing gas boiler converts around nine-tenths of its fuel into heat.", boilerURL, "Gas boiler running costs"),
		3: workerFinal(t, "Grants cover part of a heat pump's installation.", heatPumpURL, "Heat pump running costs"),
	}
	return &fleetModel{
		t:         t,
		decompose: decomposeReply(decompositionJSON(t, testBriefs(3))),
		worker: func(brief, messages int) string {
			if messages <= 2 {
				return fmt.Sprintf(`{"action":"search","query":"topic %d","url":null,"answer":null,"citations":null}`, brief)
			}
			return finals[brief]
		},
		synthesis: synthesisReply(syntheticReport),
		cite: citeReplyOf(citedClaims{
			{Claim: "An air-source heat pump costs less to run than a gas boiler.", URLs: []string{heatPumpURL, boilerURL}},
			{Claim: "A heat pump delivers around three units of heat for each unit of electricity.", URLs: []string{heatPumpURL}},
			{Claim: "A gas boiler converts around nine-tenths of its fuel into heat.", URLs: []string{boilerURL, "https://example.org/uncited"}},
		}.json(t)),
	}
}

// fleetRun is one fleet run's decomposition, pool result and outcome.
type fleetRun struct {
	plan      leadPlan
	planUsage types.Usage
	pooled    poolResult
	outcome   fleetOutcome
}

// runFleet is the lead flow a fleet run follows: decompose, run the plan
// through the pool, then conclude.
func runFleet(ctx context.Context, t *testing.T, l *lead, p *pool, query string) fleetRun {
	t.Helper()
	must := require.New(t)
	plan, planUsage, err := l.decompose(ctx, query)
	must.NoError(err, "decompose")
	pooled := p.run(ctx, plan.Briefs, nil)
	return fleetRun{plan: plan, planUsage: planUsage, pooled: pooled, outcome: l.conclude(ctx, query, plan, planUsage, pooled)}
}

// syntheticFleet builds the lead and a two-worker pool for the synthetic
// run over one model and search endpoint, both tracing to tracer and
// priced at the given rates.
func syntheticFleet(t *testing.T, modelURL string, searchSrv *searchtest.FakeServer, tracer trace.Tracer, inPrice, outPrice float64) (*lead, *pool, WorkerDeps) {
	t.Helper()
	store, ns := newRecordingStore(t, memory.InMemoryOptions{})
	mc := newModelClientAt(t, modelURL)
	wd := poolWorkerDeps(t, mc, searchSrv)
	wd.Tracer = tracer
	wd.Caps.InputGBPPerMTok, wd.Caps.OutputGBPPerMTok = inPrice, outPrice
	l := mustLead(t, leadDeps{
		Model:            mc,
		Store:            store,
		Namespace:        ns,
		Tracer:           tracer,
		InputGBPPerMTok:  wd.Caps.InputGBPPerMTok,
		OutputGBPPerMTok: wd.Caps.OutputGBPPerMTok,
	})
	p := mustPool(t, poolDeps{Worker: wd, Store: store, Namespace: ns, MaxWorkers: 2, Concurrency: 2})
	return l, p, wd
}

// TestConclude_Status_CompletedOnlyWhenEveryBriefYields: a run is Completed only when every brief yields a
// completed finding and both lead passes complete; a gap, a partial finding
// or a degraded pass makes it Incomplete; no finding with text makes it
// Failed without a call. The detail names each reason, and the body and
// citations follow the passes that ran.
func TestConclude_Status_CompletedOnlyWhenEveryBriefYields(t *testing.T) {
	failedCall := modeltest.FakeReply{Status: http.StatusInternalServerError, StatusBody: `{"error":{"message":"down"}}`}
	cutOff := modeltest.FakeReply{Content: "# Report\n\nCut", FinishReason: "length", Usage: synthesisUsage}
	attributeBoth := citeReplyOf(citedClaims{{Claim: "Claim.", URLs: []string{citeURLA, citeURLB}}}.json(t))
	partial := Finding{
		Text:      "Partial text.",
		Citations: []types.Citation{{URI: citeURLB, Title: "Partial source"}},
		Status:    types.StatusIncomplete,
		Detail:    "reached the 8-turn cap before a final answer",
		Turns:     8,
	}
	failed := Finding{Status: types.StatusFailed, Detail: "model turn failed", Turns: 1}
	both := []types.Citation{{URI: citeURLA, Title: "Source 1"}, {URI: citeURLB, Title: "Source 2"}}

	for _, tt := range []struct {
		name       string
		findings   []Finding
		dropLast   bool
		replies    []modeltest.FakeReply
		wantStatus types.Status
		wantCalls  int
		wantDetail []string
		wantBody   string
		wantCites  []types.Citation
	}{
		{
			name:       "completed",
			findings:   []Finding{completedFinding(1, citeURLA), completedFinding(2, citeURLB)},
			replies:    []modeltest.FakeReply{synthesisReply("# Report"), attributeBoth},
			wantStatus: types.StatusCompleted,
			wantCalls:  2,
			wantBody:   "# Report",
			wantCites:  both,
		},
		{
			name:       "a dropped brief",
			findings:   []Finding{completedFinding(1, citeURLA), completedFinding(2, citeURLB)},
			dropLast:   true,
			replies:    []modeltest.FakeReply{synthesisReply("# Report"), attributeBoth},
			wantStatus: types.StatusIncomplete,
			wantCalls:  2,
			wantDetail: []string{"brief-3 produced no finding: dropped: brief 3 of 3 is past the 2-worker cap"},
			wantBody:   "# Report",
			wantCites:  both,
		},
		{
			name:       "a partial finding",
			findings:   []Finding{completedFinding(1, citeURLA), partial},
			replies:    []modeltest.FakeReply{synthesisReply("# Report"), attributeBoth},
			wantStatus: types.StatusIncomplete,
			wantCalls:  2,
			wantDetail: []string{"brief-2 is partial: the worker ended incomplete: reached the 8-turn cap"},
			wantBody:   "# Report",
			wantCites:  []types.Citation{{URI: citeURLA, Title: "Source 1"}, {URI: citeURLB, Title: "Partial source"}},
		},
		{
			name:       "no completed worker",
			findings:   []Finding{partial, partial},
			replies:    []modeltest.FakeReply{synthesisReply("# Report"), attributeBoth},
			wantStatus: types.StatusIncomplete,
			wantCalls:  2,
			wantDetail: []string{"brief-1 is partial", "brief-2 is partial"},
			wantBody:   "# Report",
			wantCites:  []types.Citation{{URI: citeURLB, Title: "Partial source"}},
		},
		{
			name:       "synthesis failed",
			findings:   []Finding{completedFinding(1, citeURLA), completedFinding(2, citeURLB)},
			replies:    []modeltest.FakeReply{failedCall},
			wantStatus: types.StatusIncomplete,
			wantCalls:  1,
			wantDetail: []string{"the synthesis call failed: model: request failed: HTTP 500", "stitched unedited"},
			wantBody:   stitchedNote + "\n\n## Objective 1\n\nFinding 1 text.\n\n## Objective 2\n\nFinding 2 text.",
			wantCites:  both,
		},
		{
			name:       "synthesis cut off",
			findings:   []Finding{completedFinding(1, citeURLA), completedFinding(2, citeURLB)},
			replies:    []modeltest.FakeReply{cutOff, attributeBoth},
			wantStatus: types.StatusIncomplete,
			wantCalls:  2,
			wantDetail: []string{"the synthesis was cut off at the 16384-token completion cap"},
			wantBody:   "# Report\n\nCut",
			wantCites:  both,
		},
		{
			name:       "citation pass failed",
			findings:   []Finding{completedFinding(1, citeURLA), completedFinding(2, citeURLB)},
			replies:    []modeltest.FakeReply{synthesisReply("# Report"), failedCall},
			wantStatus: types.StatusIncomplete,
			wantCalls:  2,
			wantDetail: []string{"the citation call failed", "every worker citation"},
			wantBody:   "# Report",
			wantCites:  both,
		},
		{
			name:       "no worker text",
			findings:   []Finding{failed, failed},
			wantStatus: types.StatusFailed,
			wantCalls:  0,
			wantDetail: []string{"no worker produced a finding with text; brief-1 produced no finding: the worker failed: model turn failed"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			modelSrv := modeltest.NewFakeServer(tt.replies...)
			defer modelSrv.Close()
			l, store, ns := synthesisLead(t, modelSrv, nil)
			plan, pooled := storedRun(t, store, ns, tt.findings...)
			if tt.dropLast {
				n := len(tt.findings) + 1
				plan.Briefs = poolBriefs(n)
				pooled.Briefs = append(pooled.Briefs, briefResult{
					BriefID:     fmt.Sprintf("brief-%d", n),
					Disposition: dispositionDropped,
					Detail:      fmt.Sprintf("dropped: brief %d of %d is past the %d-worker cap", n, n, n-1),
				})
			}

			out := l.conclude(context.Background(), testQuery, plan, types.Usage{InputTokens: 400, OutputTokens: 250}, pooled)
			must.Equal(tt.wantStatus, out.Status, "detail %q", out.Detail)
			must.Equal(tt.wantCalls, modelSrv.CallCount(), "model calls")
			if tt.wantStatus == types.StatusCompleted {
				must.Empty(out.Detail, "a completed run has no detail")
			}
			for _, want := range tt.wantDetail {
				must.Contains(out.Detail, want)
			}
			must.Equal(tt.wantBody, out.Body)
			must.Equal(tt.wantCites, out.Citations)

			in := &types.Interaction{ID: "run-1", Agent: "fleet", Query: testQuery}
			out.applyTo(in)
			must.Equal("run-1", in.ID)
			must.Equal(out.Status, in.Status)
			must.Equal(out.Detail, in.StatusDetail)
			must.Equal(out.Citations, in.Citations)
			must.Equal(out.Usage, in.Usage)
			if out.Body == "" {
				must.Empty(in.Outputs, "outputs without a body")
			} else {
				must.Equal([]types.Output{{Type: types.OutputText, Text: out.Body}}, in.Outputs)
			}
		})
	}
}

// TestConclude_Body_CannotHideSources: a synthesised or stitched body that
// ends in an unclosed HTML comment is escaped, so once formatted the Sources
// list still follows as a list rather than inside an HTML block that would
// run to the end of the document.
func TestConclude_Body_CannotHideSources(t *testing.T) {
	const unclosed = "<!-- reviewer note"
	failedCall := modeltest.FakeReply{Status: http.StatusInternalServerError, StatusBody: `{"error":{"message":"down"}}`}
	attributeA := citeReplyOf(citedClaims{{Claim: "Claim.", URLs: []string{citeURLA}}}.json(t))
	for _, tt := range []struct {
		name    string
		replies []modeltest.FakeReply
		text    string
	}{
		{"synthesised", []modeltest.FakeReply{synthesisReply("# Report\n\nThe answer is X.\n\n" + unclosed), attributeA}, "Finding 1 text."},
		{"stitched", []modeltest.FakeReply{failedCall}, "Finding 1 text.\n\n" + unclosed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			modelSrv := modeltest.NewFakeServer(tt.replies...)
			defer modelSrv.Close()
			l, store, ns := synthesisLead(t, modelSrv, nil)
			f := completedFinding(1, citeURLA)
			f.Text = tt.text
			plan, pooled := storedRun(t, store, ns, f)

			out := l.conclude(context.Background(), testQuery, plan, types.Usage{}, pooled)
			in := &types.Interaction{ID: "run-1", Agent: "fleet", Query: testQuery}
			out.applyTo(in)
			report, err := formatter.NewMarkdown().Format(context.Background(), in)
			must.NoError(err, "format the report")
			md := string(report.Markdown)

			must.True(strings.HasSuffix(md, "\n\\"+unclosed+"\n\n## Sources\n\n1. [Source 1]("+citeURLA+")\n"),
				"the report does not end in the escaped comment then the Sources list:\n%s", md)
			// The formatter's provenance comment opens every report; only the
			// body below it must keep no raw-HTML opener.
			body := md
			if strings.HasPrefix(md, "<!-- chiron-version: ") {
				_, body, _ = strings.Cut(md, "\n")
			}
			must.NotRegexp(unescapedHTMLOpener, body,
				"the report keeps an unescaped raw-HTML opener, which could hide the Sources list:\n%s", md)
		})
	}
}

// panicValueKey is the credential a fault-injecting fake panics with, so a
// test can check the recovered value never reaches a detail or trace.
const panicValueKey = "sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0"

// panickingTracer panics when a span named panicOn starts, standing in for
// a faulty tracer inside a lead pass; every other span is discarded.
type panickingTracer struct {
	trace.Noop
	panicOn string
}

func (p panickingTracer) StartSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	if name == p.panicOn {
		panic("tracer failure for key " + panicValueKey)
	}
	return p.Noop.StartSpan(ctx, name)
}

// panickingStore panics on every Get of a reference in panicOn.
type panickingStore struct {
	memory.ContextStore
	panicOn map[memory.Reference]bool
}

func (s panickingStore) Get(ctx context.Context, ref memory.Reference) (io.ReadCloser, memory.ArtifactMeta, error) {
	if s.panicOn[ref] {
		panic("store failure for key " + panicValueKey)
	}
	return s.ContextStore.Get(ctx, ref)
}

// TestConclude_RecoversLeadPassPanic: a panic in a lead pass does not reach
// the caller. The outcome keeps every finding read back, stitched, with
// every worker citation, Incomplete, under a fixed detail that never echoes
// the recovered value, and counts the tokens of each lead call that
// returned; with no finding read back it is Failed with no body.
func TestConclude_RecoversLeadPassPanic(t *testing.T) {
	planUsage := types.Usage{InputTokens: 400, OutputTokens: 250}
	failed := Finding{Status: types.StatusFailed, Detail: "model turn failed", Turns: 1}
	for _, tt := range []struct {
		name       string
		panicOn    string
		findings   []Finding
		replies    []modeltest.FakeReply
		wantStatus types.Status
		wantCalls  int
		wantBody   bool
		wantDetail string
		leadTokens int
	}{
		{
			name:       "the synthesise span",
			panicOn:    trace.SpanSynthesise,
			findings:   []Finding{completedFinding(1, citeURLA), completedFinding(2, citeURLB)},
			wantStatus: types.StatusIncomplete,
			wantBody:   true,
			wantDetail: leadPanicDetail + leadPanicFallback,
		},
		{
			name:       "the cite span, after a paid synthesis",
			panicOn:    trace.SpanCite,
			findings:   []Finding{completedFinding(1, citeURLA), completedFinding(2, citeURLB)},
			replies:    []modeltest.FakeReply{synthesisReply("# Synthesised report")},
			wantStatus: types.StatusIncomplete,
			wantCalls:  1,
			wantBody:   true,
			wantDetail: leadPanicDetail + leadPanicFallback,
			leadTokens: synthesisUsage.InputTokens,
		},
		{
			name:       "the synthesise span with no finding",
			panicOn:    trace.SpanSynthesise,
			findings:   []Finding{failed, failed},
			wantStatus: types.StatusFailed,
			wantDetail: leadPanicDetail,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			modelSrv := modeltest.NewFakeServer(tt.replies...)
			defer modelSrv.Close()
			store, ns := newRecordingStore(t, memory.InMemoryOptions{})
			l := mustLead(t, leadDeps{Model: newModelClient(t, modelSrv), Store: store, Namespace: ns, Tracer: panickingTracer{panicOn: tt.panicOn}})
			plan, pooled := storedRun(t, store, ns, tt.findings...)

			out := l.conclude(context.Background(), testQuery, plan, planUsage, pooled)
			must.Equal(tt.wantStatus, out.Status, "detail %q", out.Detail)
			must.Equal(tt.wantDetail, out.Detail)
			must.Equal(tt.wantCalls, modelSrv.CallCount(), "model calls")
			collected := []collectedFinding{}
			for i, f := range tt.findings {
				if f.Status != types.StatusFailed {
					collected = append(collected, collectedFinding{BriefID: plan.Briefs[i].ID, Objective: plan.Briefs[i].Brief.Objective, Finding: f})
				}
			}
			wantBody := ""
			if tt.wantBody {
				wantBody = stitchFindings(collected)
			}
			must.Equal(wantBody, out.Body)
			must.Equal(workerCitations(collected), out.Citations, "every worker citation")
			wantInput := pooled.Usage.InputTokens + planUsage.InputTokens + tt.leadTokens
			must.Equal(wantInput, out.Usage.InputTokens, "input tokens from the workers, the plan and each lead call that returned")
			must.NotContains(out.Detail, panicValueKey, "the recovered value reached the detail")
			must.NotContains(out.Body, panicValueKey, "the recovered value reached the body")
		})
	}
}

// TestConclude_RecoversStoreReadPanic: a store that panics reading one
// brief's finding back makes that brief a gap with a fixed reason; the other
// findings are still read back and synthesised, and a store that panics on
// every read leaves a Failed run with no model call.
func TestConclude_RecoversStoreReadPanic(t *testing.T) {
	t.Run("one read", func(t *testing.T) {
		must := require.New(t)
		modelSrv := modeltest.NewFakeServer(synthesisReply("# Report"), citeReplyOf(citedClaims{{Claim: "Claim.", URLs: []string{citeURLA}}}.json(t)))
		defer modelSrv.Close()
		store, ns := newRecordingStore(t, memory.InMemoryOptions{})
		plan, pooled := storedRun(t, store, ns, completedFinding(1, citeURLA), completedFinding(2, citeURLB))
		faulty := panickingStore{ContextStore: store, panicOn: map[memory.Reference]bool{pooled.Briefs[1].Ref: true}}
		l := mustLead(t, leadDeps{Model: newModelClient(t, modelSrv), Store: faulty, Namespace: ns})

		out := l.conclude(context.Background(), testQuery, plan, types.Usage{}, pooled)
		must.Equal(types.StatusIncomplete, out.Status, "detail %q", out.Detail)
		must.Equal("# Report", out.Body, "the synthesised body")
		want := "brief-2 produced no finding: the finding could not be read back: the store panicked"
		must.Contains(out.Detail, want)
		must.NotContains(out.Detail, panicValueKey, "the recovered value reached the detail")
		must.Equal(2, modelSrv.CallCount(), "synthesis and the citation pass")
	})

	t.Run("every read", func(t *testing.T) {
		must := require.New(t)
		modelSrv := modeltest.NewFakeServer()
		defer modelSrv.Close()
		store, ns := newRecordingStore(t, memory.InMemoryOptions{})
		plan, pooled := storedRun(t, store, ns, completedFinding(1, citeURLA), completedFinding(2, citeURLB))
		faulty := panickingStore{ContextStore: store, panicOn: map[memory.Reference]bool{}}
		for _, r := range pooled.Briefs {
			faulty.panicOn[r.Ref] = true
		}
		l := mustLead(t, leadDeps{Model: newModelClient(t, modelSrv), Store: faulty, Namespace: ns})

		out := l.conclude(context.Background(), testQuery, plan, types.Usage{}, pooled)
		must.Equal(types.StatusFailed, out.Status, "outcome %+v", out)
		must.Empty(out.Body, "a failed run has no body")
		must.Nil(out.Citations, "a failed run has no citations")
		must.Zero(modelSrv.CallCount(), "the lead makes no model call")
	})
}

// TestConclude_Detail_IsScrubbedAndBounded: the run's detail joins every
// reason, scrubbed, and never exceeds maxDetailBytes.
func TestConclude_Detail_IsScrubbedAndBounded(t *testing.T) {
	must := require.New(t)
	const key = "sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0"
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	l, _, _ := synthesisLead(t, modelSrv, nil)
	var pooled poolResult
	for n := 1; n <= 5; n++ {
		pooled.Briefs = append(pooled.Briefs, briefResult{
			BriefID:     fmt.Sprintf("brief-%d", n),
			Disposition: dispositionDropped,
			Detail:      "dropped for key " + key + " " + strings.Repeat("x", 600),
		})
	}

	out := l.conclude(context.Background(), testQuery, leadPlan{Briefs: poolBriefs(5)}, types.Usage{}, pooled)
	must.Equal(types.StatusFailed, out.Status)
	must.LessOrEqual(len(out.Detail), maxDetailBytes, "detail is cut to at most maxDetailBytes")
	must.True(strings.HasSuffix(out.Detail, truncatedMarker), "detail ends with the truncation marker")
	must.NotContains(out.Detail, key, "detail carries the credential")
}

// TestRunUsage_PricesLeadOnce: the run's Usage is the workers' usage plus
// the lead calls' tokens priced at the lead's rates. An estimate already on
// a lead call is replaced, not added, and zero prices add no cost.
func TestRunUsage_PricesLeadOnce(t *testing.T) {
	workers := types.Usage{InputTokens: 40, OutputTokens: 20, SearchCount: 2, RecallCount: 1, EstimatedCostGBP: 0.5}
	calls := []types.Usage{
		{InputTokens: 400, OutputTokens: 250},
		{InputTokens: 300, OutputTokens: 200, EstimatedCostGBP: 99},
		{InputTokens: 150, OutputTokens: 50},
	}
	for _, tt := range []struct {
		name              string
		inPrice, outPrice float64
		wantCost          float64
	}{
		{"priced", 2, 8, 0.5 + (850*2+500*8)/1e6},
		{"unpriced", 0, 0, 0.5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			l := &lead{deps: leadDeps{InputGBPPerMTok: tt.inPrice, OutputGBPPerMTok: tt.outPrice}}
			got := l.runUsage(workers, calls...)
			want := types.Usage{InputTokens: 890, OutputTokens: 520, SearchCount: 2, RecallCount: 1}
			cost := got.EstimatedCostGBP
			got.EstimatedCostGBP = 0
			must.Equal(want, got, "usage without the cost")
			must.InDelta(tt.wantCost, cost, 1e-12, "estimated cost")
		})
	}
}

// spanTotals sums the spend attributes across spans.
type spanTotals struct {
	input, output, search, recall float64
	cost                          float64
}

func sumSpans(spans []traceSpan) spanTotals {
	var s spanTotals
	for _, sp := range spans {
		n := func(key string) float64 { v, _ := sp.Attrs[key].(float64); return v }
		s.input += n("input_tokens")
		s.output += n("output_tokens")
		s.search += n("search_count")
		s.recall += n("recall_count")
		s.cost += n("estimated_cost_gbp")
	}
	return s
}

// TestFleet_RunUsage_RollsUpSpansOnce: over a synthetic two-worker run under
// the JSONL tracer, the run's Usage equals the delegate spans' spend plus
// the decompose, synthesise and cite spans' tokens, priced at the workers'
// rates, counting no worker span a second time; and nothing in the fleet
// records a run-level metric, because the run core records them once from
// this Usage.
func TestFleet_RunUsage_RollsUpSpansOnce(t *testing.T) {
	must := require.New(t)
	const inPrice, outPrice = 1.5, 6
	modelSrv := httptest.NewServer(syntheticFleetModel(t))
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(syntheticSearchResults)
	defer searchSrv.Close()
	var out bytes.Buffer
	tracer := trace.NewJSONL(&out)
	l, p, _ := syntheticFleet(t, modelSrv.URL, searchSrv, tracer, inPrice, outPrice)

	ctx, root := tracer.StartSpan(context.Background(), trace.SpanResearch)
	run := runFleet(ctx, t, l, p, testQuery)
	root.End(nil)
	lines := out.String()
	usage := run.outcome.Usage

	delegates := sumSpans(spansNamed(t, lines, trace.SpanDelegate))
	workers := sumSpans(spansNamed(t, lines, trace.SpanWorker))
	var leadSpans []traceSpan
	for _, name := range []string{trace.SpanDecompose, trace.SpanSynthesise, trace.SpanCite} {
		spans := spansNamed(t, lines, name)
		must.Len(spans, 1, "%s spans", name)
		leadSpans = append(leadSpans, spans...)
	}
	leadTotals := sumSpans(leadSpans)

	want := types.Usage{
		InputTokens:      int(delegates.input + leadTotals.input),
		OutputTokens:     int(delegates.output + leadTotals.output),
		SearchCount:      int(delegates.search),
		RecallCount:      int(delegates.recall),
		EstimatedCostGBP: delegates.cost + costGBP(int(leadTotals.input), int(leadTotals.output), inPrice, outPrice),
	}
	must.Equal(want.InputTokens, usage.InputTokens, "input tokens from the spans")
	must.Equal(want.OutputTokens, usage.OutputTokens, "output tokens from the spans")
	must.Equal(want.SearchCount, usage.SearchCount, "searches from the spans")
	must.Equal(want.RecallCount, usage.RecallCount, "recalls from the spans")
	must.InDelta(want.EstimatedCostGBP, usage.EstimatedCostGBP, 1e-12, "cost from the spans")
	// Two workers of two turns at 10/5 tokens, decompose 400/250, synthesis
	// 300/200 and citation 150/50: the spans are the run, not a subset.
	must.Equal(890, usage.InputTokens)
	must.Equal(520, usage.OutputTokens)
	must.Equal(2, usage.SearchCount)
	// Each worker's spend is on both its delegate and its worker span; the
	// rollup above counts the delegates alone.
	must.Equal(delegates, workers, "worker spans should carry the same spend as delegate spans")
	must.NotZero(workers.input, "worker spans should carry input tokens")
	wantInput := run.pooled.Usage.InputTokens + run.planUsage.InputTokens + synthesisUsage.InputTokens + citeUsage.InputTokens
	must.Equal(wantInput, usage.InputTokens, "input tokens should be the pool's plus the lead's calls")

	var metrics []string
	for _, line := range strings.Split(strings.TrimSpace(lines), "\n") {
		var rec struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		must.NoError(json.Unmarshal([]byte(line), &rec), "decode trace line")
		if rec.Type == "metric" {
			metrics = append(metrics, rec.Name)
		}
	}
	must.Empty(metrics, "the run core records run-level metrics once, so the fleet should record none")
}

// TestFleet_Run_SpanTopology: over a synthetic two-worker run, the decompose,
// delegate, synthesise and cite spans are all children of the caller's run
// span; each delegate whose worker ran has exactly one worker span beneath
// it, and the brief past the two-worker cap has a delegate with none.
func TestFleet_Run_SpanTopology(t *testing.T) {
	must := require.New(t)
	modelSrv := httptest.NewServer(syntheticFleetModel(t))
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(syntheticSearchResults)
	defer searchSrv.Close()
	var out bytes.Buffer
	tracer := trace.NewJSONL(&out)
	l, p, _ := syntheticFleet(t, modelSrv.URL, searchSrv, tracer, 0, 0)

	ctx, root := tracer.StartSpan(context.Background(), trace.SpanResearch)
	runFleet(ctx, t, l, p, testQuery)
	root.End(nil)
	lines := out.String()

	research := spansNamed(t, lines, trace.SpanResearch)
	must.Len(research, 1)
	rootID := research[0].SpanID

	for _, tt := range []struct {
		name string
		want int
	}{
		{trace.SpanDecompose, 1},
		{trace.SpanDelegate, 3},
		{trace.SpanSynthesise, 1},
		{trace.SpanCite, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			spans := spansNamed(t, lines, tt.name)
			must.Len(spans, tt.want, "trace:\n%s", lines)
			for _, s := range spans {
				must.Equal(rootID, s.ParentID)
			}
		})
	}

	workers := spansNamed(t, lines, trace.SpanWorker)
	must.Len(workers, 2)
	ran := 0
	for _, d := range spansNamed(t, lines, trace.SpanDelegate) {
		t.Run(fmt.Sprint(d.Attrs[trace.AttrBriefID]), func(t *testing.T) {
			must := require.New(t)
			children := 0
			for _, w := range workers {
				if w.ParentID == d.SpanID {
					children++
				}
			}
			switch d.Attrs["disposition"] {
			case string(dispositionRan):
				ran++
				must.Equal(1, children, "worker spans beneath a delegate whose worker ran")
			default:
				must.Equal("brief-3", d.Attrs[trace.AttrBriefID], "only brief-3 is dropped past the two-worker cap")
				must.Zero(children, "worker spans beneath a dropped delegate")
			}
		})
	}
	must.Equal(2, ran, "delegates whose worker ran")
}
