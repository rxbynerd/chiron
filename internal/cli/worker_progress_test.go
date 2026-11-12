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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	must := require.New(t)
	var events []progressStreamEvent
	for line := range strings.Lines(stderr) {
		var ev progressStreamEvent
		must.NoError(json.Unmarshal([]byte(line), &ev), "stderr is not NDJSON: %q", line)
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

// TestWorkerProgress_EmitsOneDeltaPerTurnThroughCLI: a streamed worker run emits one
// worker_turn delta per turn, in turn order, strictly between
// interaction_created and run_completed; a --quiet run of the same script
// emits none, and both runs print the same report and exit cleanly.
func TestWorkerProgress_EmitsOneDeltaPerTurnThroughCLI(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

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
	must.NoError(err, "research --agent worker failed; stderr:\n%s", stderr)

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
			must.NoError(json.Unmarshal(ev.Payload, &p), "delta payload %s", ev.Payload)
			if p.Type == "worker_turn" {
				turns = append(turns, turnEvent{index: i, payload: p})
			}
		}
	}
	must.GreaterOrEqual(created, 0, "stderr lacks interaction_created:\n%s", stderr)
	must.GreaterOrEqual(completed, 0, "stderr lacks run_completed:\n%s", stderr)

	want := []struct {
		name   string
		action string
		detail string
		text   string
	}{
		{"search turn", "search", "sky", "turn 1/8: search sky (38 tokens so far)"},
		{"fetch turn", "fetch", page.URL, "turn 2/8: fetch " + page.URL + " (104 tokens so far)"},
		{"final turn", "final", "", "turn 3/8: final (214 tokens so far)"},
	}
	must.Len(turns, len(want), "worker_turn deltas = %+v\n%s", turns, stderr)
	for i, tt := range want {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got := turns[i]
			must.Greater(got.index, created, "turn event must follow interaction_created")
			must.Less(got.index, completed, "turn event must precede run_completed")
			p := got.payload
			must.Equal(i+1, p.Turn)
			must.Equal(8, p.MaxTurns)
			must.Equal(tt.action, p.Action)
			must.Equal(tt.detail, p.Detail)
			must.Equal(tt.text, p.Text)
		})
	}
	check.NotContains(stderr, "utm_source", "progress events carry a URL query")
	check.NotContains(stderr, "Rayleigh", "progress events carry page content")

	quietModel := progressScriptModel(articleURL)
	defer quietModel.Close()
	quietOut, quietErr, err := execute(t, workerArgs(quietModel, searchSrv, "-o", "text", "--quiet")...)
	must.NoError(err, "research --agent worker --quiet failed; stderr:\n%s", quietErr)
	var quietTurns []json.RawMessage
	for _, ev := range decodeProgressStream(t, quietErr) {
		if ev.Kind != transport.KindDelta {
			continue
		}
		var p workerTurnPayload
		if json.Unmarshal(ev.Payload, &p) == nil && p.Type == "worker_turn" {
			quietTurns = append(quietTurns, ev.Payload)
		}
	}
	check.Empty(quietTurns, "--quiet run emitted worker_turn deltas")
	check.Equal(withoutRunStamps(quietOut), withoutRunStamps(stdout), "report differs with progress events on")
	check.Contains(stdout, "Rayleigh scattering", "report lacks the answer")
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

// TestBindWorkerProgress_QuietBindsNothing: without streaming the binder
// returns no hook, so the worker reports nothing.
func TestBindWorkerProgress_QuietBindsNothing(t *testing.T) {
	must := require.New(t)
	hook := bindWorkerProgress(false, &emitRecorder{})
	must.Nil(hook, "bindWorkerProgress(stream=false) returned a hook")
}

// TestBindWorkerProgress_ScrubsSearchPayload: one report becomes one worker_turn
// delta whose text and detail are scrubbed again at the transport boundary.
func TestBindWorkerProgress_ScrubsSearchPayload(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const key = "Zk3xQ9vB2mN7pL4tR8wY1cH6jD5fG0sA"
	rec := &emitRecorder{}
	hook := bindWorkerProgress(true, rec)
	hook(context.Background(), fleet.Progress{
		Turn: 2, MaxTurns: 8, Action: "search", Detail: "docs for " + key,
		InputTokens: 100, OutputTokens: 20, EstimatedCostGBP: 0.0012,
	})

	must.Len(rec.events, 1, "events = %+v", rec.events)
	must.Equal(transport.KindDelta, rec.events[0].Kind)
	raw := string(rec.events[0].Payload)
	check.NotContains(raw, key, "payload carries the credential")
	check.NotContains(raw, "worker_id", "a single worker's payload names a fleet worker")
	var p workerTurnPayload
	must.NoError(json.Unmarshal(rec.events[0].Payload, &p), "payload %s", raw)
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
	check.Equal(want, p)
}

// TestBindWorkerProgress_ScrubsRecallPayload: bindWorkerProgress scrubs Detail the
// same way for a recall action as it does for search.
func TestBindWorkerProgress_ScrubsRecallPayload(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const key = "Zk3xQ9vB2mN7pL4tR8wY1cH6jD5fG0sA"
	rec := &emitRecorder{}
	hook := bindWorkerProgress(true, rec)
	hook(context.Background(), fleet.Progress{
		Turn: 3, MaxTurns: 8, Action: "recall", Detail: "PHY vendor " + key,
		InputTokens: 50, OutputTokens: 10, EstimatedCostGBP: 0.0006,
	})

	must.Len(rec.events, 1, "events = %+v", rec.events)
	must.Equal(transport.KindDelta, rec.events[0].Kind)
	raw := string(rec.events[0].Payload)
	check.NotContains(raw, key, "payload carries the credential")
	var p workerTurnPayload
	must.NoError(json.Unmarshal(rec.events[0].Payload, &p), "payload %s", raw)
	want := workerTurnPayload{
		Type:             "worker_turn",
		Text:             "turn 3/8: recall PHY vendor [REDACTED:high-entropy] (60 tokens so far)",
		Turn:             3,
		MaxTurns:         8,
		Action:           "recall",
		Detail:           "PHY vendor [REDACTED:high-entropy]",
		InputTokens:      50,
		OutputTokens:     10,
		EstimatedCostGBP: 0.0006,
	}
	check.Equal(want, p)
}

// TestBindWorkerProgress_NamesFleetWorkerInPayload: a fleet worker's report
// carries its worker id in the payload and leads the text with it.
func TestBindWorkerProgress_NamesFleetWorkerInPayload(t *testing.T) {
	must := require.New(t)
	rec := &emitRecorder{}
	bindWorkerProgress(true, rec)(context.Background(), fleet.Progress{
		Turn: 1, MaxTurns: 8, Action: "search", Detail: "heat pumps",
		InputTokens: 10, OutputTokens: 5, WorkerID: "worker-2",
	})
	must.Len(rec.events, 1, "events = %+v", rec.events)
	var p workerTurnPayload
	must.NoError(json.Unmarshal(rec.events[0].Payload, &p), "payload %s", rec.events[0].Payload)
	must.Equal("worker-2", p.WorkerID)
	must.Equal("worker-2 turn 1/8: search heat pumps (15 tokens so far)", p.Text)
}

// TestBindWorkerProgress_DropsEmitErrorsAndUnmarshalableReports: an Emit error or an
// unmarshalable report is dropped without a panic, and the hook returns.
func TestBindWorkerProgress_DropsEmitErrorsAndUnmarshalableReports(t *testing.T) {
	t.Run("emit error", func(t *testing.T) {
		must := require.New(t)
		failer := &emitFailer{}
		bindWorkerProgress(true, failer)(context.Background(), fleet.Progress{Turn: 1, MaxTurns: 8, Action: "final"})
		must.Equal(1, failer.calls)
	})

	t.Run("unmarshalable report", func(t *testing.T) {
		must := require.New(t)
		rec := &emitRecorder{}
		bindWorkerProgress(true, rec)(context.Background(), fleet.Progress{Turn: 1, MaxTurns: 8, Action: "final", EstimatedCostGBP: math.NaN()})
		must.Empty(rec.events, "a report that cannot be marshalled emits no event")
	})
}
