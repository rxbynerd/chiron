package cli

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/researcher/fleet"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/transport"
)

// progressStreamEvent is one decoded NDJSON event from stderr.
type progressStreamEvent struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// decodeProgressStream decodes every stderr line as an NDJSON event.
func decodeProgressStream(t *testing.T, stderr string) []progressStreamEvent {
	t.Helper()
	var events []progressStreamEvent
	for line := range strings.Lines(stderr) {
		var ev progressStreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stderr is not NDJSON: %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

// progressScriptModel scripts a search, a fetch of articleURL, and a final
// answer citing it, each turn with usage so token counts accumulate.
func progressScriptModel(articleURL string) *modeltest.FakeServer {
	return modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, Usage: model.Usage{InputTokens: 30, OutputTokens: 8}},
		modeltest.FakeReply{Content: `{"action":"fetch","url":"` + articleURL + `"}`, Usage: model.Usage{InputTokens: 60, OutputTokens: 6}},
		modeltest.FakeReply{
			Content: `{"action":"final","answer":"# Answer\n\nRayleigh scattering.","citations":[{"url":"` + articleURL + `","title":"Sky article"}]}`,
			Usage:   model.Usage{InputTokens: 90, OutputTokens: 20},
		},
	)
}

// withoutRunStamps drops the report lines that differ between two runs of
// the same script: the minted interaction id and the timestamps.
func withoutRunStamps(report string) string {
	var b strings.Builder
	for line := range strings.Lines(report) {
		if strings.HasPrefix(line, "interaction: ") || strings.HasPrefix(line, "started: ") || strings.HasPrefix(line, "completed: ") {
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}

// TestWorkerProgressEventsThroughCLI: a streamed worker run emits one
// worker_turn delta per turn, in turn order, strictly between
// interaction_created and run_completed; a --quiet run of the same script
// emits none, and both runs print the same report and exit cleanly.
func TestWorkerProgressEventsThroughCLI(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Sky</h1><p>Rayleigh scattering.</p></body></html>"))
	}))
	defer page.Close()
	articleURL := page.URL + "/sky?utm_source=search"
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "Sky article", URL: articleURL, Snippet: "scattering"}})
	defer searchSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("CHIRON_FETCH_ALLOW_LOOPBACK", "1")

	streamedModel := progressScriptModel(articleURL)
	defer streamedModel.Close()
	stdout, stderr, err := execute(t, workerArgs(streamedModel, searchSrv, "-o", "text")...)
	if err != nil {
		t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr)
	}

	type turnEvent struct {
		index   int
		payload workerTurnPayload
	}
	created, completed := -1, -1
	var turns []turnEvent
	for i, ev := range decodeProgressStream(t, stderr) {
		switch ev.Kind {
		case transport.KindInteractionCreated:
			created = i
		case transport.KindRunCompleted:
			completed = i
		case transport.KindDelta:
			var p workerTurnPayload
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatalf("delta payload %s: %v", ev.Payload, err)
			}
			if p.Type == "worker_turn" {
				turns = append(turns, turnEvent{index: i, payload: p})
			}
		}
	}
	if created < 0 || completed < 0 {
		t.Fatalf("stderr lacks interaction_created or run_completed:\n%s", stderr)
	}

	want := []struct {
		action string
		detail string
		text   string
	}{
		{"search", "sky", "turn 1/8: search sky (38 tokens so far)"},
		{"fetch", page.URL, "turn 2/8: fetch " + page.URL + " (104 tokens so far)"},
		{"final", "", "turn 3/8: final (214 tokens so far)"},
	}
	if len(turns) != len(want) {
		t.Fatalf("worker_turn deltas = %+v, want %d:\n%s", turns, len(want), stderr)
	}
	for i, tt := range want {
		got := turns[i]
		if got.index <= created || got.index >= completed {
			t.Errorf("turn %d event at %d, want strictly between interaction_created (%d) and run_completed (%d)", i+1, got.index, created, completed)
		}
		p := got.payload
		if p.Turn != i+1 || p.MaxTurns != 8 || p.Action != tt.action || p.Detail != tt.detail || p.Text != tt.text {
			t.Errorf("turn %d payload = %+v, want action %q detail %q text %q", i+1, p, tt.action, tt.detail, tt.text)
		}
	}
	if strings.Contains(stderr, "utm_source") || strings.Contains(stderr, "Rayleigh") {
		t.Errorf("progress events carry a URL path/query or page content:\n%s", stderr)
	}

	quietModel := progressScriptModel(articleURL)
	defer quietModel.Close()
	quietOut, quietErr, err := execute(t, workerArgs(quietModel, searchSrv, "-o", "text", "--quiet")...)
	if err != nil {
		t.Fatalf("research --agent worker --quiet: %v\nstderr: %s", err, quietErr)
	}
	for _, ev := range decodeProgressStream(t, quietErr) {
		if ev.Kind == transport.KindDelta {
			t.Errorf("--quiet run emitted a delta: %s", ev.Payload)
		}
	}
	if withoutRunStamps(stdout) != withoutRunStamps(quietOut) {
		t.Errorf("report differs with progress events on:\n--- streamed\n%s\n--- quiet\n%s", stdout, quietOut)
	}
	if !strings.Contains(stdout, "Rayleigh scattering") {
		t.Errorf("report lacks the answer:\n%s", stdout)
	}
}

// emitRecorder records every event it is given.
type emitRecorder struct {
	events []transport.Event
}

func (r *emitRecorder) Emit(_ context.Context, ev transport.Event) error {
	r.events = append(r.events, ev)
	return nil
}

func (*emitRecorder) Close() error { return nil }

// emitFailer fails every Emit, standing in for a broken event stream.
type emitFailer struct {
	calls int
}

func (f *emitFailer) Emit(context.Context, transport.Event) error {
	f.calls++
	return errors.New("transport: write \"delta\" event: broken pipe")
}

func (*emitFailer) Close() error { return nil }

// TestBindWorkerProgressQuietBindsNothing: without streaming the binder
// returns no hook, so the worker reports nothing.
func TestBindWorkerProgressQuietBindsNothing(t *testing.T) {
	if hook := bindWorkerProgress(false, &emitRecorder{}); hook != nil {
		t.Error("bindWorkerProgress(stream=false) returned a hook, want nil")
	}
}

// TestBindWorkerProgressPayload: one report becomes one worker_turn delta
// whose text and detail are scrubbed again at the transport boundary.
func TestBindWorkerProgressPayload(t *testing.T) {
	const key = "Zk3xQ9vB2mN7pL4tR8wY1cH6jD5fG0sA"
	rec := &emitRecorder{}
	hook := bindWorkerProgress(true, rec)
	hook(context.Background(), fleet.Progress{
		Turn: 2, MaxTurns: 8, Action: "search", Detail: "docs for " + key,
		InputTokens: 100, OutputTokens: 20, EstimatedCostGBP: 0.0012,
	})

	if len(rec.events) != 1 || rec.events[0].Kind != transport.KindDelta {
		t.Fatalf("events = %+v, want one delta", rec.events)
	}
	raw := string(rec.events[0].Payload)
	if strings.Contains(raw, key) {
		t.Errorf("payload carries the credential: %s", raw)
	}
	var p workerTurnPayload
	if err := json.Unmarshal(rec.events[0].Payload, &p); err != nil {
		t.Fatalf("payload %s: %v", raw, err)
	}
	want := workerTurnPayload{
		Type:             "worker_turn",
		Text:             "turn 2/8: search docs for [REDACTED:high-entropy] (120 tokens so far)",
		Turn:             2,
		MaxTurns:         8,
		Action:           "search",
		Detail:           "docs for [REDACTED:high-entropy]",
		InputTokens:      100,
		OutputTokens:     20,
		EstimatedCostGBP: 0.0012,
	}
	if p != want {
		t.Errorf("payload = %+v, want %+v", p, want)
	}
}

// TestBindWorkerProgressFailuresAreDropped: an Emit error or an
// unmarshalable report is dropped without a panic, and the hook returns.
func TestBindWorkerProgressFailuresAreDropped(t *testing.T) {
	failer := &emitFailer{}
	bindWorkerProgress(true, failer)(context.Background(), fleet.Progress{Turn: 1, MaxTurns: 8, Action: "final"})
	if failer.calls != 1 {
		t.Errorf("emit calls = %d, want 1", failer.calls)
	}

	rec := &emitRecorder{}
	bindWorkerProgress(true, rec)(context.Background(), fleet.Progress{Turn: 1, MaxTurns: 8, Action: "final", EstimatedCostGBP: math.NaN()})
	if len(rec.events) != 0 {
		t.Errorf("events = %+v, want none for a report that cannot be marshalled", rec.events)
	}
}
