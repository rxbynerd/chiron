package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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
	b, err := json.Marshal(map[string]any{
		"action":    "final",
		"query":     nil,
		"url":       nil,
		"answer":    answer,
		"citations": []map[string]string{{"url": url, "title": title}},
	})
	if err != nil {
		t.Fatalf("marshal final action: %v", err)
	}
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
	plan, planUsage, err := l.decompose(ctx, query)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
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

// TestConcludeStatusRules: a run is Completed only when every brief yields a
// completed finding and both lead passes complete; a gap, a partial finding
// or a degraded pass makes it Incomplete; no finding with text makes it
// Failed without a call. The detail names each reason, and the body and
// citations follow the passes that ran.
func TestConcludeStatusRules(t *testing.T) {
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
			if out.Status != tt.wantStatus {
				t.Errorf("status = %s, want %s (detail %q)", out.Status, tt.wantStatus, out.Detail)
			}
			if n := modelSrv.CallCount(); n != tt.wantCalls {
				t.Errorf("model calls = %d, want %d", n, tt.wantCalls)
			}
			if tt.wantStatus == types.StatusCompleted && out.Detail != "" {
				t.Errorf("detail = %q, want none for a completed run", out.Detail)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(out.Detail, want) {
					t.Errorf("detail = %q, want it to contain %q", out.Detail, want)
				}
			}
			if out.Body != tt.wantBody {
				t.Errorf("body = %q, want %q", out.Body, tt.wantBody)
			}
			if !reflect.DeepEqual(out.Citations, tt.wantCites) {
				t.Errorf("citations = %+v, want %+v", out.Citations, tt.wantCites)
			}

			in := &types.Interaction{ID: "run-1", Agent: "fleet", Query: testQuery}
			out.applyTo(in)
			if in.ID != "run-1" || in.Status != out.Status || in.StatusDetail != out.Detail ||
				!reflect.DeepEqual(in.Citations, out.Citations) || in.Usage != out.Usage {
				t.Errorf("interaction = %+v, want the outcome applied to its fields", in)
			}
			switch {
			case out.Body == "" && len(in.Outputs) != 0:
				t.Errorf("outputs = %+v, want none without a body", in.Outputs)
			case out.Body != "" && !reflect.DeepEqual(in.Outputs, []types.Output{{Type: types.OutputText, Text: out.Body}}):
				t.Errorf("outputs = %+v, want the body as the one text output", in.Outputs)
			}
		})
	}
}

// TestConcludeBodyCannotHideTheSources: a synthesised or stitched body that
// ends in an unclosed HTML comment is escaped, so once formatted the Sources
// list still follows as a list rather than inside an HTML block that would
// run to the end of the document.
func TestConcludeBodyCannotHideTheSources(t *testing.T) {
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
			if err != nil {
				t.Fatalf("Format: %v", err)
			}
			md := string(report.Markdown)

			if !strings.HasSuffix(md, "\n\\"+unclosed+"\n\n## Sources\n\n1. [Source 1]("+citeURLA+")\n") {
				t.Errorf("the report does not end in the escaped comment then the Sources list:\n%s", md)
			}
			if unescapedHTMLOpener.MatchString(md) {
				t.Errorf("the report keeps an unescaped raw-HTML opener, which could hide the Sources list:\n%s", md)
			}
		})
	}
}

// TestConcludeDetailIsScrubbedAndBounded: the run's detail joins every
// reason, scrubbed, and never exceeds maxDetailBytes.
func TestConcludeDetailIsScrubbedAndBounded(t *testing.T) {
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
	if out.Status != types.StatusFailed {
		t.Errorf("status = %s, want failed", out.Status)
	}
	if len(out.Detail) > maxDetailBytes || !strings.HasSuffix(out.Detail, truncatedMarker) {
		t.Errorf("detail is %d bytes, want it cut to %d", len(out.Detail), maxDetailBytes)
	}
	if strings.Contains(out.Detail, key) {
		t.Errorf("detail carries the credential: %q", out.Detail)
	}
}

// TestRunUsagePricesTheLeadOnce: the run's Usage is the workers' usage plus
// the lead calls' tokens priced at the lead's rates. An estimate already on
// a lead call is replaced, not added, and zero prices add no cost.
func TestRunUsagePricesTheLeadOnce(t *testing.T) {
	workers := types.Usage{InputTokens: 40, OutputTokens: 20, SearchCount: 2, RecallCount: 1, EstimatedCostGBP: 0.5}
	calls := []types.Usage{
		{InputTokens: 400, OutputTokens: 250},
		{InputTokens: 300, OutputTokens: 200, EstimatedCostGBP: 99},
		{InputTokens: 150, OutputTokens: 50},
	}
	for _, tt := range []struct {
		name            string
		inPrice, outPrc float64
		wantCost        float64
	}{
		{"priced", 2, 8, 0.5 + (850*2+500*8)/1e6},
		{"unpriced", 0, 0, 0.5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := &lead{deps: leadDeps{InputGBPPerMTok: tt.inPrice, OutputGBPPerMTok: tt.outPrc}}
			got := l.runUsage(workers, calls...)
			want := types.Usage{InputTokens: 890, OutputTokens: 520, SearchCount: 2, RecallCount: 1}
			cost := got.EstimatedCostGBP
			got.EstimatedCostGBP = 0
			if got != want || math.Abs(cost-tt.wantCost) > 1e-12 {
				t.Errorf("runUsage = %+v (cost %v), want %+v (cost %v)", got, cost, want, tt.wantCost)
			}
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

// TestFleetRunUsageRollsUpSpansOnce: over a synthetic two-worker run under
// the JSONL tracer, the run's Usage equals the delegate spans' spend plus
// the decompose, synthesise and cite spans' tokens, priced at the workers'
// rates, counting no worker span a second time; and nothing in the fleet
// records a run-level metric, because the run core records them once from
// this Usage.
func TestFleetRunUsageRollsUpSpansOnce(t *testing.T) {
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
		if len(spans) != 1 {
			t.Fatalf("%s spans = %d, want 1", name, len(spans))
		}
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
	if usage.InputTokens != want.InputTokens || usage.OutputTokens != want.OutputTokens ||
		usage.SearchCount != want.SearchCount || usage.RecallCount != want.RecallCount ||
		math.Abs(usage.EstimatedCostGBP-want.EstimatedCostGBP) > 1e-12 {
		t.Errorf("usage = %+v, want the spans' rollup %+v", usage, want)
	}
	// Two workers of two turns at 10/5 tokens, decompose 400/250, synthesis
	// 300/200 and citation 150/50: the spans are the run, not a subset.
	if usage.InputTokens != 890 || usage.OutputTokens != 520 || usage.SearchCount != 2 {
		t.Errorf("usage = %+v, want 890/520 tokens and 2 searches", usage)
	}
	// Each worker's spend is on both its delegate and its worker span; the
	// rollup above counts the delegates alone.
	if workers != delegates || workers.input == 0 {
		t.Errorf("worker spans %+v and delegate spans %+v, want the same spend on each", workers, delegates)
	}
	if usage.InputTokens != run.pooled.Usage.InputTokens+run.planUsage.InputTokens+synthesisUsage.InputTokens+citeUsage.InputTokens {
		t.Errorf("usage %+v is not the pool's %+v plus the lead's calls", usage, run.pooled.Usage)
	}

	for _, line := range strings.Split(strings.TrimSpace(lines), "\n") {
		var rec struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode trace line: %v", err)
		}
		if rec.Type == "metric" {
			t.Errorf("the fleet recorded the metric %q; the run core records run-level metrics once", rec.Name)
		}
	}
}

// TestFleetRunSpanTopology: over a synthetic two-worker run, the decompose,
// delegate, synthesise and cite spans are all children of the caller's run
// span; each delegate whose worker ran has exactly one worker span beneath
// it, and the brief past the two-worker cap has a delegate with none.
func TestFleetRunSpanTopology(t *testing.T) {
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
	if len(research) != 1 {
		t.Fatalf("research spans = %d, want 1", len(research))
	}
	rootID := research[0].SpanID
	for name, want := range map[string]int{trace.SpanDecompose: 1, trace.SpanDelegate: 3, trace.SpanSynthesise: 1, trace.SpanCite: 1} {
		spans := spansNamed(t, lines, name)
		if len(spans) != want {
			t.Errorf("%s spans = %d, want %d:\n%s", name, len(spans), want, lines)
		}
		for _, s := range spans {
			if s.ParentID != rootID {
				t.Errorf("a %s span's parent is %q, want the run span %q", name, s.ParentID, rootID)
			}
		}
	}

	workers := spansNamed(t, lines, trace.SpanWorker)
	if len(workers) != 2 {
		t.Fatalf("worker spans = %d, want 2", len(workers))
	}
	ran := 0
	for _, d := range spansNamed(t, lines, trace.SpanDelegate) {
		children := 0
		for _, w := range workers {
			if w.ParentID == d.SpanID {
				children++
			}
		}
		switch d.Attrs["disposition"] {
		case string(dispositionRan):
			ran++
			if children != 1 {
				t.Errorf("delegate %v has %d worker spans, want 1", d.Attrs[trace.AttrBriefID], children)
			}
		default:
			if children != 0 || d.Attrs[trace.AttrBriefID] != "brief-3" {
				t.Errorf("delegate %v (%v) has %d worker spans, want the dropped brief-3 with none", d.Attrs[trace.AttrBriefID], d.Attrs["disposition"], children)
			}
		}
	}
	if ran != 2 {
		t.Errorf("delegates whose worker ran = %d, want 2", ran)
	}
}
