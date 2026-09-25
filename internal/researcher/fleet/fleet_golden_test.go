package fleet

import (
	"bytes"
	"context"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/rxbynerd/chiron/internal/formatter"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/types"
)

// TestFleetGoldenReport renders a fleet run end to end, from decompose to the
// Markdown formatter, and pins the synthesis user message. Three briefs under
// a two-worker cap put a dropped brief in the golden; the fake model answers
// by request kind and brief, so the run is deterministic at concurrency 2.
func TestFleetGoldenReport(t *testing.T) {
	fm := syntheticFleetModel(t)
	modelSrv := httptest.NewServer(fm)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(syntheticSearchResults)
	defer searchSrv.Close()
	l, p, wd := syntheticFleet(t, modelSrv.URL, searchSrv, trace.Noop{}, 0, 0)

	run := runFleet(context.Background(), t, l, p, testQuery)
	in := &types.Interaction{ID: "fleet-golden", Agent: "fleet", Query: testQuery, Tools: workerTools(wd)}
	run.outcome.applyTo(in)
	report, err := formatter.NewMarkdown().Format(context.Background(), in)
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	checkGolden(t, "fleet-report.md", report.Markdown)
	synthesis := fm.requestsOf(kindSynthesise)
	if len(synthesis) != 1 {
		t.Fatalf("synthesis requests = %d, want 1", len(synthesis))
	}
	checkGolden(t, "fleet-synthesis-prompt.txt", []byte(synthesis[0][1].Content))

	want := []types.Citation{
		{URI: heatPumpURL, Title: "Heat pump running costs"},
		{URI: boilerURL, Title: "Gas boiler running costs"},
	}
	if !reflect.DeepEqual(run.outcome.Citations, want) {
		t.Errorf("citations = %+v, want one source from each worker %+v", run.outcome.Citations, want)
	}
	if bytes.Contains(report.Markdown, []byte("example.org/uncited")) {
		t.Error("a URL no worker cited reached the report")
	}
}
