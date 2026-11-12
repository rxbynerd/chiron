package fleet

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/types"
)

// progressLog records the Progress deliveries a hook receives.
type progressLog struct {
	mu    sync.Mutex
	calls []Progress
}

func (l *progressLog) hook(_ context.Context, p Progress) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, p)
}

func (l *progressLog) snapshot() []Progress {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Progress(nil), l.calls...)
}

// highEntropyKey is credential-shaped enough for secret.Scrub's backstop.
const highEntropyKey = "Zk3xQ9vB2mN7pL4tR8wY1cH6jD5fG0sA"

// TestRunWorker_Progress_OnePerTurn: a search, fetch, final run reports exactly
// one Progress per turn, in turn order, carrying the accumulated usage and a
// detail that names each target without its content.
func TestRunWorker_Progress_OnePerTurn(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	page := httptest.NewServer(plainTextPage("Rayleigh scattering makes the sky appear blue."))
	defer page.Close()
	articleURL := page.URL + "/articles/sky?session=abc123&ref=search#section-2"

	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "Why the sky is blue", URL: articleURL}})
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"why is the sky blue"}`, Usage: model.Usage{InputTokens: 30, OutputTokens: 8}},
		modeltest.FakeReply{Content: `{"action":"fetch","url":` + jsonString(articleURL) + `}`, Usage: model.Usage{InputTokens: 60, OutputTokens: 6}},
		finalReply("# Answer\n\nRayleigh scattering.", articleURL),
	)
	defer modelSrv.Close()

	var log progressLog
	c := caps()
	c.InputGBPPerMTok, c.OutputGBPPerMTok = 1, 4
	deps := WorkerDeps{
		Model:    newModelClient(t, modelSrv),
		Search:   newSearchClient(t, searchSrv),
		Fetch:    newFetchClient(t),
		Caps:     c,
		Progress: log.hook,
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "why is the sky blue"})
	must.Equal(types.StatusCompleted, finding.Status, "finding detail: %s", finding.Detail)

	got := log.snapshot()
	want := []struct {
		action string
		detail string
	}{
		{"search", "why is the sky blue"},
		{"fetch", page.URL},
		{"final", ""},
	}
	must.Len(got, len(want), "one report per turn")

	for i, tt := range want {
		t.Run(tt.action, func(t *testing.T) {
			must := require.New(t)
			p := got[i]
			must.Equal(i+1, p.Turn)
			must.Equal(c.MaxTurns, p.MaxTurns)
			must.Equal(tt.action, p.Action)
			must.Equal(tt.detail, p.Detail)
			if i > 0 {
				prev := got[i-1]
				must.GreaterOrEqual(p.InputTokens, prev.InputTokens, "input tokens went backwards")
				must.GreaterOrEqual(p.OutputTokens, prev.OutputTokens, "output tokens went backwards")
				must.GreaterOrEqual(p.EstimatedCostGBP, prev.EstimatedCostGBP, "estimated cost went backwards")
			}
		})
	}

	check.Equal(30, got[0].InputTokens, "turn 1 counts its own model call")
	check.Equal(8, got[0].OutputTokens, "turn 1 counts its own model call")
	check.NotZero(got[0].EstimatedCostGBP, "turn 1 is priced")

	last := got[len(got)-1]
	check.Equal(finding.Usage.InputTokens, last.InputTokens, "final report input tokens")
	check.Equal(finding.Usage.OutputTokens, last.OutputTokens, "final report output tokens")
	check.InDelta(finding.Usage.EstimatedCostGBP, last.EstimatedCostGBP, 0, "final report estimated cost")
}

// TestRunWorker_Progress_RecallReportsQuery: a recall turn reports the recall
// query as its detail.
func TestRunWorker_Progress_RecallReportsQuery(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	recall := &staticRecall{hits: []memory.Recalled{billetHit("m1", "PHY shortlist", "Vendor A was shortlisted.")}}
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		recallReply("which PHY vendor did we shortlist"),
		finalReply("Vendor A.", "billet://memory/m1"),
	)
	defer modelSrv.Close()

	var log progressLog
	finding := RunWorker(context.Background(), WorkerDeps{
		Model:     newModelClient(t, modelSrv),
		Search:    newSearchClient(t, searchSrv),
		Fetch:     newFetchClient(t),
		Knowledge: recall,
		Caps:      caps(),
		Progress:  log.hook,
	}, Brief{Objective: "PHY vendor"})
	must.Equal(types.StatusCompleted, finding.Status, "finding detail: %s", finding.Detail)

	got := log.snapshot()
	must.Len(got, 2, "one recall report and one final report")
	check.Equal("recall", got[0].Action)
	check.Equal("which PHY vendor did we shortlist", got[0].Detail)
	check.Equal("final", got[1].Action)
	check.Empty(got[1].Detail, "the final report carries no detail")
}

// TestRunWorker_Progress_SilentBeforeParsedAction: a turn that ends before its
// action parses reports nothing; the Finding carries that outcome.
func TestRunWorker_Progress_SilentBeforeParsedAction(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reply modeltest.FakeReply
		want  types.Status
	}{
		{"model error", modeltest.FakeReply{Status: http.StatusInternalServerError, StatusBody: `{"error":"boom"}`}, types.StatusFailed},
		{"length cut-off", modeltest.FakeReply{Content: `{"action":"sea`, FinishReason: "length"}, types.StatusIncomplete},
		{"invalid action", modeltest.FakeReply{Content: `{"action":"shell","query":"rm -rf /"}`}, types.StatusFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			modelSrv := modeltest.NewFakeServer(tt.reply)
			defer modelSrv.Close()
			searchSrv := searchtest.NewFakeServer(nil)
			defer searchSrv.Close()

			var log progressLog
			finding := RunWorker(context.Background(), WorkerDeps{
				Model:    newModelClient(t, modelSrv),
				Search:   newSearchClient(t, searchSrv),
				Fetch:    newFetchClient(t),
				Caps:     caps(),
				Progress: log.hook,
			}, Brief{Objective: "q"})
			must.Equal(tt.want, finding.Status, "finding detail: %s", finding.Detail)
			must.Empty(log.snapshot(), "no progress before a parsed action")
		})
	}
}

// searchFinalRun runs a two-turn search-then-final worker with the given
// progress hook, deadline and logger, over fakes whose output is identical on
// every call, so two runs' Findings are comparable. A nil logger discards
// output, as WorkerDeps.Logger does.
func searchFinalRun(t *testing.T, hook func(context.Context, Progress), deadline time.Duration, logger *slog.Logger) Finding {
	t.Helper()
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "Sky", URL: "https://example.org/sky"}})
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, Usage: model.Usage{InputTokens: 20, OutputTokens: 4}},
		finalReply("# Answer\n\nblue sky", "https://example.org/sky"),
	)
	defer modelSrv.Close()

	return RunWorker(context.Background(), WorkerDeps{
		Model:                   newModelClient(t, modelSrv),
		Search:                  newSearchClient(t, searchSrv),
		Fetch:                   newFetchClient(t),
		Caps:                    caps(),
		Progress:                hook,
		Logger:                  logger,
		progressTimeoutOverride: deadline,
	}, Brief{Objective: "why is the sky blue"})
}

// TestRunWorker_Progress_HookPanicIsRecovered: a hook that panics on every turn
// is recovered, the run ends with the Finding it produces without a hook, and
// the recovered value is logged at Debug with any credential scrubbed.
func TestRunWorker_Progress_HookPanicIsRecovered(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	want := searchFinalRun(t, nil, 0, nil)
	must.Equal(types.StatusCompleted, want.Status, "baseline finding detail: %s", want.Detail)

	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var calls atomic.Int32
	got := searchFinalRun(t, func(context.Context, Progress) {
		calls.Add(1)
		panic("progress hook failure: " + highEntropyKey)
	}, 0, logger)
	check.Equal(want, got, "finding with a panicking hook")
	check.EqualValues(2, calls.Load(), "a panic on one turn must not stop later reports")

	log := logged.String()
	check.Contains(log, "progress hook panicked", "the recovered panic is logged")
	check.NotContains(log, highEntropyKey, "the raw credential from the panic value is not logged")
	check.Contains(log, "REDACTED", "the scrubbed placeholder replaces the credential")
}

// TestRunWorker_Progress_BlockingHookIsAbandoned: a hook that ignores its
// context is abandoned at the progress deadline on each turn, so the run
// completes with an unchanged Finding in time bounded by that deadline.
func TestRunWorker_Progress_BlockingHookIsAbandoned(t *testing.T) {
	check := assert.New(t)

	want := searchFinalRun(t, nil, 0, nil)

	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	start := time.Now()
	got := searchFinalRun(t, func(context.Context, Progress) {
		calls.Add(1)
		<-release
	}, 50*time.Millisecond, nil)
	elapsed := time.Since(start)

	check.Equal(want, got, "finding with a blocking hook")
	check.EqualValues(2, calls.Load(), "an abandoned report must not stop later ones")
	check.Less(elapsed, progressTimeout, "the run is bounded by the 50ms test deadline per turn")
}

// TestProgressDetail_NamesTargetWithoutContent: a detail names the target
// without its content, as one scrubbed, printable, bounded line; a fetch
// reduces to scheme://host.
func TestProgressDetail_NamesTargetWithoutContent(t *testing.T) {
	for _, tt := range []struct {
		name string
		act  action
		want string
	}{
		{"search query", action{Kind: actionSearch, Query: "why is the sky blue"}, "why is the sky blue"},
		{"recall query", action{Kind: actionRecall, Query: "PHY shortlist"}, "PHY shortlist"},
		{"flattened to printable text", action{Kind: actionSearch, Query: "  sky\n\tblue\x1b[31m red\u202e "}, "sky blue[31m red"},
		{"credential scrubbed", action{Kind: actionSearch, Query: "docs for " + highEntropyKey}, "docs for [REDACTED:high-entropy]"},
		{"credential split by a control character", action{Kind: actionSearch, Query: highEntropyKey[:16] + "\x00" + highEntropyKey[16:]}, "[REDACTED:high-entropy]"},
		{"bearer scrubbed", action{Kind: actionRecall, Query: "Bearer abc.def-ghi"}, "Bearer [REDACTED:bearer-token]"},
		{"fetch drops userinfo, path, query and fragment", action{Kind: actionFetch, URL: "https://user:pass@example.org:8443/a/b?token=x#frag"}, "https://example.org:8443"},
		{"fetch without port", action{Kind: actionFetch, URL: " https://example.org/path?q=1 "}, "https://example.org"},
		{"fetch lowercases scheme and host", action{Kind: actionFetch, URL: "HTTPS://EXAMPLE.COM/Path"}, "https://example.com"},
		{"fetch unparsable", action{Kind: actionFetch, URL: "http://[::1/path"}, ""},
		{"fetch relative", action{Kind: actionFetch, URL: "/just/a/path?q=1"}, ""},
		{"fetch opaque", action{Kind: actionFetch, URL: "mailto:someone@example.org"}, ""},
		{"final carries no answer", action{Kind: actionFinal, Answer: "the whole answer", Citations: []citationInput{{URL: "https://example.org/a"}}}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.want, progressDetail(tt.act))
		})
	}
}

// TestProgressDetail_BoundsLongQueries: a long query is cut to
// maxProgressDetailRunes on a rune boundary, and scrubbing precedes the cut so
// a credential straddling the bound leaves no fragment behind.
func TestProgressDetail_BoundsLongQueries(t *testing.T) {
	check := assert.New(t)

	long := strings.Repeat("é", 3*maxProgressDetailRunes)
	got := progressDetail(action{Kind: actionSearch, Query: long})
	check.Equal(maxProgressDetailRunes, utf8.RuneCountInString(got), "detail runes")
	check.True(utf8.ValidString(got), "detail is valid UTF-8")
	check.True(strings.HasPrefix(long, got), "detail is a prefix of the query")

	straddle := strings.Repeat("a", maxProgressDetailRunes-10) + " " + highEntropyKey
	got = progressDetail(action{Kind: actionSearch, Query: straddle})
	check.NotContains(got, highEntropyKey[:9], "the detail carries no fragment of the credential")
}

// waitFor polls cond until it holds, failing the test after a generous bound.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	must := require.New(t)
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			must.FailNow("timed out waiting for " + what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// newProgressWorker builds a Worker whose search returns one sky result and
// which reports progress to log under the given deadline.
func newProgressWorker(t *testing.T, modelSrv *modeltest.FakeServer, log *progressLog, deadline time.Duration) *Worker {
	t.Helper()
	must := require.New(t)
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "Sky", URL: "https://example.org/sky"}})
	t.Cleanup(searchSrv.Close)
	w, err := NewWorker(WorkerDeps{
		Model:                   newModelClient(t, modelSrv),
		Search:                  newSearchClient(t, searchSrv),
		Fetch:                   newFetchClient(t),
		Caps:                    caps(),
		Progress:                log.hook,
		progressTimeoutOverride: deadline,
	})
	must.NoError(err, "NewWorker")
	return w
}

// TestWorker_Progress_WaitsForAwait: the Worker holds a run's progress
// deliveries until Await is entered for it, so none can precede the run
// core's id event; the held loop takes no further turn meanwhile, and once
// Await is entered every report arrives in turn order.
func TestWorker_Progress_WaitsForAwait(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`},
		finalReply("# Answer\n\nblue sky", "https://example.org/sky"),
	)
	defer modelSrv.Close()
	var log progressLog
	w := newProgressWorker(t, modelSrv, &log, 30*time.Second)

	ctx := context.Background()
	id, err := w.Start(ctx, researcher.Task{Query: "why is the sky blue"})
	must.NoError(err, "Start")
	waitFor(t, "the first model turn", func() bool { return modelSrv.CallCount() >= 1 })
	time.Sleep(50 * time.Millisecond)
	must.Empty(log.snapshot(), "progress before Await")
	must.Equal(1, modelSrv.CallCount(), "the loop must be held at turn 1's report")

	must.NoError(w.Await(ctx, id), "Await")
	got := log.snapshot()
	must.Len(got, 2, "one report per turn")
	check.Equal(1, got[0].Turn)
	check.Equal("search", got[0].Action)
	check.Equal(2, got[1].Turn)
	check.Equal("final", got[1].Action)

	in, err := w.Result(ctx, id)
	must.NoError(err, "Result")
	must.Equal(types.StatusCompleted, in.Status)
}

// TestWorker_Progress_DroppedWithoutAwait: a delivery whose gate stays shut past
// the progress deadline is dropped rather than delivered late, and the run
// completes unaffected.
func TestWorker_Progress_DroppedWithoutAwait(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`},
		finalReply("# Answer\n\nblue sky", "https://example.org/sky"),
	)
	defer modelSrv.Close()
	var log progressLog
	w := newProgressWorker(t, modelSrv, &log, 20*time.Millisecond)

	ctx := context.Background()
	id, err := w.Start(ctx, researcher.Task{Query: "why is the sky blue"})
	must.NoError(err, "Start")
	waitFor(t, "the loop to finish", func() bool {
		in, err := w.Result(ctx, id)
		return err == nil && in.Status != types.StatusInProgress
	})
	must.Empty(log.snapshot(), "progress before Await: Await was never called yet")
	must.NoError(w.Await(ctx, id), "Await")
	check.Empty(log.snapshot(), "both reports dropped at the deadline")

	in, err := w.Result(ctx, id)
	must.NoError(err, "Result")
	check.Equal(types.StatusCompleted, in.Status)
}
