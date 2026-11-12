package fleet

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/formatter"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/types"
)

// TestFleet_Report_GoldenEndToEnd renders a fleet run end to end, from decompose to the
// Markdown formatter, and pins the synthesis user message. Three briefs under
// a two-worker cap put a dropped brief in the golden; the fake model answers
// by request kind and brief, so the run is deterministic at concurrency 2.
func TestFleet_Report_GoldenEndToEnd(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

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
	must.NoError(err, "format report")
	checkGolden(t, "fleet-report.md", report.Markdown)

	synthesis := fm.requestsOf(kindSynthesise)
	must.Len(synthesis, 1, "one synthesis request")
	checkGolden(t, "fleet-synthesis-prompt.txt", []byte(synthesis[0][1].Content))

	want := []types.Citation{
		{URI: heatPumpURL, Title: "Heat pump running costs"},
		{URI: boilerURL, Title: "Gas boiler running costs"},
	}
	check.Equal(want, run.outcome.Citations, "want one source from each worker")
	check.NotContains(string(report.Markdown), "example.org/uncited", "a URL no worker cited reached the report")
}
