package fleet

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

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

// TestRunWorkerProgressPerTurn: a search, fetch, final run reports exactly one
// Progress per turn, in turn order, carrying the accumulated usage and a
// detail that names each target without its content.
func TestRunWorkerProgressPerTurn(t *testing.T) {
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
	if finding.Status != types.StatusCompleted {
		t.Fatalf("status = %s (%s), want completed", finding.Status, finding.Detail)
	}

	got := log.snapshot()
	want := []struct {
		action string
		detail string
	}{
		{"search", "why is the sky blue"},
		{"fetch", page.URL},
		{"final", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("progress calls = %+v, want %d (one per turn)", got, len(want))
	}
	for i, tt := range want {
		p := got[i]
		if p.Turn != i+1 || p.MaxTurns != c.MaxTurns || p.Action != tt.action || p.Detail != tt.detail {
			t.Errorf("call %d = %+v, want turn %d/%d %s %q", i, p, i+1, c.MaxTurns, tt.action, tt.detail)
		}
		if i == 0 {
			continue
		}
		prev := got[i-1]
		if p.InputTokens < prev.InputTokens || p.OutputTokens < prev.OutputTokens || p.EstimatedCostGBP < prev.EstimatedCostGBP {
			t.Errorf("call %d usage %+v went backwards from %+v", i, p, prev)
		}
	}
	if got[0].InputTokens != 30 || got[0].OutputTokens != 8 || got[0].EstimatedCostGBP == 0 {
		t.Errorf("turn 1 usage = %+v, want its own model call counted and priced", got[0])
	}
	last := got[len(got)-1]
	if last.InputTokens != finding.Usage.InputTokens || last.OutputTokens != finding.Usage.OutputTokens || last.EstimatedCostGBP != finding.Usage.EstimatedCostGBP {
		t.Errorf("final report usage = %+v, want the finding's %+v", last, finding.Usage)
	}
}

// TestRunWorkerProgressRecall: a recall turn reports the recall query.
func TestRunWorkerProgressRecall(t *testing.T) {
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
	if finding.Status != types.StatusCompleted {
		t.Fatalf("status = %s (%s), want completed", finding.Status, finding.Detail)
	}

	got := log.snapshot()
	if len(got) != 2 {
		t.Fatalf("progress calls = %+v, want 2", got)
	}
	if got[0].Action != "recall" || got[0].Detail != "which PHY vendor did we shortlist" {
		t.Errorf("recall report = %+v, want the recall query as detail", got[0])
	}
	if got[1].Action != "final" || got[1].Detail != "" {
		t.Errorf("final report = %+v, want an empty detail", got[1])
	}
}

// TestRunWorkerProgressNothingBeforeParsedAction: a turn that ends before its
// action parses reports nothing; the Finding carries that outcome.
func TestRunWorkerProgressNothingBeforeParsedAction(t *testing.T) {
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
			if finding.Status != tt.want {
				t.Errorf("status = %s (%s), want %s", finding.Status, finding.Detail, tt.want)
			}
			if got := log.snapshot(); len(got) != 0 {
				t.Errorf("progress calls = %+v, want none", got)
			}
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

// TestRunWorkerProgressHookPanicIsRecovered: a hook that panics on every turn
// is recovered, the run ends with the Finding it produces without a hook, and
// the recovered value is logged at Debug with any credential scrubbed.
func TestRunWorkerProgressHookPanicIsRecovered(t *testing.T) {
	want := searchFinalRun(t, nil, 0, nil)
	if want.Status != types.StatusCompleted {
		t.Fatalf("baseline status = %s (%s), want completed", want.Status, want.Detail)
	}

	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var calls atomic.Int32
	got := searchFinalRun(t, func(context.Context, Progress) {
		calls.Add(1)
		panic("progress hook failure: " + highEntropyKey)
	}, 0, logger)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("finding with a panicking hook = %+v, want %+v", got, want)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("hook calls = %d, want 2: a panic on one turn must not stop later reports", n)
	}

	log := logged.String()
	if !strings.Contains(log, "progress hook panicked") {
		t.Errorf("log = %q, want a record of the recovered panic", log)
	}
	if strings.Contains(log, highEntropyKey) {
		t.Errorf("log = %q, leaked the raw credential from the panic value", log)
	}
	if !strings.Contains(log, "REDACTED") {
		t.Errorf("log = %q, want the scrubbed placeholder in place of the credential", log)
	}
}

// TestRunWorkerProgressBlockingHookIsAbandoned: a hook that ignores its
// context is abandoned at the progress deadline on each turn, so the run
// completes with an unchanged Finding in time bounded by that deadline.
func TestRunWorkerProgressBlockingHookIsAbandoned(t *testing.T) {
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

	if !reflect.DeepEqual(got, want) {
		t.Errorf("finding with a blocking hook = %+v, want %+v", got, want)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("hook calls = %d, want 2: an abandoned report must not stop later ones", n)
	}
	if elapsed >= progressTimeout {
		t.Errorf("run took %v, want it bounded by the 50ms test deadline per turn (well under %v)", elapsed, progressTimeout)
	}
}

// TestProgressDetail: a detail names the target without its content, as one
// scrubbed, printable, bounded line; a fetch reduces to scheme://host.
func TestProgressDetail(t *testing.T) {
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
			if got := progressDetail(tt.act); got != tt.want {
				t.Errorf("progressDetail = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestProgressDetailBounded: a long query is cut to maxProgressDetailRunes on
// a rune boundary, and scrubbing precedes the cut so a credential straddling
// the bound leaves no fragment behind.
func TestProgressDetailBounded(t *testing.T) {
	long := strings.Repeat("é", 3*maxProgressDetailRunes)
	got := progressDetail(action{Kind: actionSearch, Query: long})
	if n := utf8.RuneCountInString(got); n != maxProgressDetailRunes {
		t.Errorf("detail runes = %d, want %d", n, maxProgressDetailRunes)
	}
	if !utf8.ValidString(got) || !strings.HasPrefix(long, got) {
		t.Errorf("detail %q is not a clean prefix of the query", got)
	}

	straddle := strings.Repeat("a", maxProgressDetailRunes-10) + " " + highEntropyKey
	got = progressDetail(action{Kind: actionSearch, Query: straddle})
	if strings.Contains(got, highEntropyKey[:9]) {
		t.Errorf("detail %q carries a fragment of the credential", got)
	}
}

// waitFor polls cond until it holds, failing the test after a generous bound.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// newProgressWorker builds a Worker whose search returns one sky result and
// which reports progress to log under the given deadline.
func newProgressWorker(t *testing.T, modelSrv *modeltest.FakeServer, log *progressLog, deadline time.Duration) *Worker {
	t.Helper()
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
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w
}

// TestWorkerProgressWaitsForAwait: the Worker holds a run's progress
// deliveries until Await is entered for it, so none can precede the run
// core's id event; the held loop takes no further turn meanwhile, and once
// Await is entered every report arrives in turn order.
func TestWorkerProgressWaitsForAwait(t *testing.T) {
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`},
		finalReply("# Answer\n\nblue sky", "https://example.org/sky"),
	)
	defer modelSrv.Close()
	var log progressLog
	w := newProgressWorker(t, modelSrv, &log, 30*time.Second)

	ctx := context.Background()
	id, err := w.Start(ctx, researcher.Task{Query: "why is the sky blue"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the first model turn", func() bool { return modelSrv.CallCount() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := log.snapshot(); len(got) != 0 {
		t.Fatalf("progress before Await = %+v, want none", got)
	}
	if n := modelSrv.CallCount(); n != 1 {
		t.Fatalf("model calls before Await = %d, want 1: the loop must be held at turn 1's report", n)
	}

	if err := w.Await(ctx, id); err != nil {
		t.Fatalf("Await: %v", err)
	}
	got := log.snapshot()
	if len(got) != 2 || got[0].Turn != 1 || got[0].Action != "search" || got[1].Turn != 2 || got[1].Action != "final" {
		t.Errorf("progress after Await = %+v, want turn 1 search then turn 2 final", got)
	}
	in, err := w.Result(ctx, id)
	if err != nil || in.Status != types.StatusCompleted {
		t.Errorf("Result = %+v, %v, want completed", in, err)
	}
}

// TestWorkerProgressDroppedWithoutAwait: a delivery whose gate stays shut past
// the progress deadline is dropped rather than delivered late, and the run
// completes unaffected.
func TestWorkerProgressDroppedWithoutAwait(t *testing.T) {
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`},
		finalReply("# Answer\n\nblue sky", "https://example.org/sky"),
	)
	defer modelSrv.Close()
	var log progressLog
	w := newProgressWorker(t, modelSrv, &log, 20*time.Millisecond)

	ctx := context.Background()
	id, err := w.Start(ctx, researcher.Task{Query: "why is the sky blue"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the loop to finish", func() bool {
		in, err := w.Result(ctx, id)
		return err == nil && in.Status != types.StatusInProgress
	})
	if err := w.Await(ctx, id); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if got := log.snapshot(); len(got) != 0 {
		t.Errorf("progress = %+v, want both reports dropped at the deadline", got)
	}
	in, err := w.Result(ctx, id)
	if err != nil || in.Status != types.StatusCompleted {
		t.Errorf("Result = %+v, %v, want completed", in, err)
	}
}
