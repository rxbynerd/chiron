package fleet

import (
	"bytes"
	"context"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/rxbynerd/chiron/internal/formatter"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/run"
	"github.com/rxbynerd/chiron/internal/sink"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/transport"
	"github.com/rxbynerd/chiron/internal/types"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

// The golden normalisers replace the fields an in-process run over loopback
// fakes cannot make deterministic — the random local interaction id, the
// wall-clock started/completed timestamps, and the ephemeral loopback port the
// fetched source URL carries — with fixed placeholders, so the golden compares
// the stable structure (front-matter shape, body, citations) rather than
// run-to-run noise.
var (
	volatileID    = regexp.MustCompile(`(?m)^interaction: wkr_[0-9a-f]+$`)
	volatileStamp = regexp.MustCompile(`(?m)^(started|completed): .+$`)
	volatilePort  = regexp.MustCompile(`127\.0\.0\.1:\d+`)
)

func normalise(md []byte) []byte {
	md = volatileID.ReplaceAll(md, []byte("interaction: wkr_XXXX"))
	md = volatileStamp.ReplaceAll(md, []byte("$1: <timestamp>"))
	md = volatilePort.ReplaceAll(md, []byte("127.0.0.1:PORT"))
	return md
}

// captureSink records the RunResult the run core writes, so the test can
// assert on the produced report without a file or stdout.
type captureSink struct{ result *types.RunResult }

func (c *captureSink) Write(_ context.Context, r *types.RunResult) error {
	c.result = r
	return nil
}

// TestWorkerGoldenReportThroughRunCore is the end-to-end golden: a Worker over
// a scripted model (search -> fetch -> final with citations), a fake search
// MCP, and a loopback web_fetch, driven through the UNCHANGED run core, must
// produce the expected Markdown report (body and citations). This proves the
// whole Wave 3 path — worker loop, Finding -> Interaction mapping, and the
// existing formatter/sink — hangs together, per docs/V2-RESEARCH-AGENT §5
// acceptance criteria and §8 golden-report requirement.
func TestWorkerGoldenReportThroughRunCore(t *testing.T) {
	// A loopback page the worker fetches. Its text is what the model
	// synthesises the report from.
	page := httptest.NewServer(plainTextPage("10BASE-T1L is a long-reach single-pair Ethernet PHY standardised in IEEE 802.3cg."))
	defer page.Close()

	searchSrv := search.NewFakeServer([]search.Result{
		{Title: "IEEE 802.3cg overview", URL: page.URL, Snippet: "single-pair Ethernet"},
		// A degraded result with no URL — the worker must tolerate it (it is
		// surfaced but not fetchable).
		{Title: "Unlinked note", URL: "", Snippet: "no url here"},
	})
	defer searchSrv.Close()

	answer := "# 10BASE-T1L\n\n" +
		"10BASE-T1L is a long-reach single-pair Ethernet physical layer " +
		"standardised in IEEE 802.3cg, suited to industrial and building " +
		"automation over a single twisted pair.\n"
	modelSrv := model.NewFakeServer(
		model.FakeReply{Content: `{"action":"search","query":"10BASE-T1L PHY standard"}`, FinishReason: "stop", Usage: model.Usage{InputTokens: 120, OutputTokens: 15, TotalTokens: 135}},
		model.FakeReply{Content: `{"action":"fetch","url":"` + page.URL + `"}`, FinishReason: "stop", Usage: model.Usage{InputTokens: 200, OutputTokens: 10, TotalTokens: 210}},
		model.FakeReply{
			Content:      `{"action":"final","answer":` + jsonString(answer) + `,"citations":[{"url":"` + page.URL + `","title":"IEEE 802.3cg overview"}]}`,
			FinishReason: "stop",
			Usage:        model.Usage{InputTokens: 300, OutputTokens: 90, TotalTokens: 390},
		},
	)
	defer modelSrv.Close()

	w, err := NewWorker(WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	cap := &captureSink{}
	deps := run.Deps{
		Researcher: w,
		Formatter:  formatter.NewMarkdown(),
		Sink:       cap,
		Transport:  transport.NewStdio(bytes.NewBuffer(nil)),
		Tracer:     trace.Noop{},
	}
	result, err := run.Run(context.Background(), deps, run.Params{Query: "what is 10BASE-T1L", Agent: "worker"})
	if err != nil {
		t.Fatalf("run.Run: %v", err)
	}
	if result.Status != types.StatusCompleted {
		t.Fatalf("status = %s, want completed", result.Status)
	}
	if cap.result == nil || cap.result.Report == nil {
		t.Fatal("no report written to the sink")
	}

	got := normalise(cap.result.Report.Markdown)
	goldenPath := filepath.Join("testdata", "worker-report.md")
	if *update {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("report mismatch.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// Sanity beyond the golden: the search-derived source is cited, and the
	// URL-less degraded result never became a citation.
	if want := "single-pair Ethernet"; !bytes.Contains(cap.result.Report.Markdown, []byte(want)) {
		t.Errorf("report missing expected body content %q", want)
	}
}

var _ sink.ReportSink = (*captureSink)(nil)
