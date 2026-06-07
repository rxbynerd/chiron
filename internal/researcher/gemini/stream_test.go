package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rxbynerd/chiron/internal/interactions"
	"github.com/rxbynerd/chiron/internal/researcher"
)

// streamOpts makes a fast-reconnect streaming adapter for tests.
func streamOpts(thoughts *[]string, mu *sync.Mutex) func(*Options) {
	return func(o *Options) {
		o.Stream = true
		o.ReconnectBaseDelay = time.Millisecond
		o.ReconnectMaxDelay = time.Millisecond
		if thoughts != nil {
			o.OnThought = func(text string) {
				mu.Lock()
				*thoughts = append(*thoughts, text)
				mu.Unlock()
			}
		}
	}
}

// sseHandler writes one SSE response from the given frames and returns.
func sseWrite(t *testing.T, w http.ResponseWriter, frames ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, f := range frames {
		if _, err := fmt.Fprint(w, f); err != nil {
			t.Errorf("writing SSE frame: %v", err)
		}
	}
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func TestAwaitStreamsToCompletion(t *testing.T) {
	var (
		mu          sync.Mutex
		thoughts    []string
		streamGets  atomic.Int64
		plainGets   atomic.Int64
		gotLastID   atomic.Value
		createCalls atomic.Int64
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			createCalls.Add(1)
			w.Write([]byte(`{"id":"v1_s","status":"in_progress"}`))
		case r.URL.Query().Get("stream") == "true":
			streamGets.Add(1)
			gotLastID.Store(r.URL.Query().Get("last_event_id"))
			sseWrite(t, w,
				"id: e1\nevent: step.delta\ndata: {\"index\":0,\"delta\":{\"type\":\"thought_summary_delta\",\"text\":\"reading sources\"}}\n\n",
				"id: e2\nevent: step.delta\ndata: {\"index\":0,\"delta\":{\"type\":\"thought_summary_delta\",\"text\":\"drafting\"}}\n\n",
				"id: e3\nevent: interaction.status_update\ndata: {\"interaction_id\":\"v1_s\",\"status\":\"completed\"}\n\n",
			)
		default:
			plainGets.Add(1)
			w.Write([]byte(`{"id":"v1_s","status":"completed"}`))
		}
	}))
	defer server.Close()

	r := newResearcher(t, server, streamOpts(&thoughts, &mu))
	ctx := context.Background()
	id, err := r.Start(ctx, researcher.Task{Query: "q"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Await(ctx, id); err != nil {
		t.Fatalf("Await: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(thoughts) != 2 || thoughts[0] != "reading sources" || thoughts[1] != "drafting" {
		t.Errorf("thoughts = %v, want both summaries in order", thoughts)
	}
	if streamGets.Load() != 1 {
		t.Errorf("stream attaches = %d, want 1 — no reconnect on a clean completion", streamGets.Load())
	}
	if got := gotLastID.Load(); got != "" {
		t.Errorf("first attach sent last_event_id=%q, want empty", got)
	}

	in, err := r.Result(ctx, id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Usage.ReconnectCount != 0 {
		t.Errorf("reconnect count = %d, want 0", in.Usage.ReconnectCount)
	}
	if in.Usage.PollCount != 0 {
		t.Errorf("poll count = %d, want 0 — the await streamed", in.Usage.PollCount)
	}
}

func TestAwaitReconnectsWithLastEventID(t *testing.T) {
	var (
		attaches atomic.Int64
		lastIDs  []string
		mu       sync.Mutex
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"id":"v1_r","status":"in_progress"}`))
		case r.URL.Query().Get("stream") == "true":
			mu.Lock()
			lastIDs = append(lastIDs, r.URL.Query().Get("last_event_id"))
			mu.Unlock()
			if attaches.Add(1) == 1 {
				// First attach: one delta, then the connection ends
				// without a terminal state — a drop.
				sseWrite(t, w,
					"id: e7\nevent: step.delta\ndata: {\"index\":0,\"delta\":{\"type\":\"thought_summary_delta\",\"text\":\"first\"}}\n\n",
				)
				return
			}
			sseWrite(t, w,
				"id: e8\nevent: interaction.status_update\ndata: {\"interaction_id\":\"v1_r\",\"status\":\"completed\"}\n\n",
			)
		default:
			w.Write([]byte(`{"id":"v1_r","status":"completed"}`))
		}
	}))
	defer server.Close()

	r := newResearcher(t, server, streamOpts(nil, nil))
	ctx := context.Background()
	id, err := r.Start(ctx, researcher.Task{Query: "q"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Await(ctx, id); err != nil {
		t.Fatalf("Await: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(lastIDs) != 2 {
		t.Fatalf("stream attaches = %v, want initial + one reconnect", lastIDs)
	}
	if lastIDs[0] != "" {
		t.Errorf("initial attach sent last_event_id=%q, want empty", lastIDs[0])
	}
	if lastIDs[1] != "e7" {
		t.Errorf("reconnect sent last_event_id=%q, want e7 — resume is via the query parameter", lastIDs[1])
	}

	in, err := r.Result(ctx, id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Usage.ReconnectCount != 1 {
		t.Errorf("reconnect count = %d, want 1", in.Usage.ReconnectCount)
	}
}

func TestAwaitFallsBackToPollingWhenStreamingRepeatedlyFails(t *testing.T) {
	var (
		streamGets atomic.Int64
		plainGets  atomic.Int64
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"id":"v1_f","status":"in_progress"}`))
		case r.URL.Query().Get("stream") == "true":
			// Not an event stream: every attach fails immediately.
			streamGets.Add(1)
			w.Write([]byte(`{"id":"v1_f","status":"in_progress"}`))
		default:
			plainGets.Add(1)
			w.Write([]byte(`{"id":"v1_f","status":"completed"}`))
		}
	}))
	defer server.Close()

	r := newResearcher(t, server, streamOpts(nil, nil))
	ctx := context.Background()
	id, err := r.Start(ctx, researcher.Task{Query: "q"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Await(ctx, id); err != nil {
		t.Fatalf("Await must fall back to polling, got %v", err)
	}

	if got := streamGets.Load(); got != maxStreamFailures {
		t.Errorf("stream attempts = %d, want the %d-failure budget spent", got, maxStreamFailures)
	}
	if plainGets.Load() == 0 {
		t.Error("the poll fallback never polled")
	}

	in, err := r.Result(ctx, id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Usage.PollCount == 0 {
		t.Error("poll count must record the fallback polling")
	}
	if in.Usage.ReconnectCount != maxStreamFailures-1 {
		t.Errorf("reconnect count = %d, want %d re-dials", in.Usage.ReconnectCount, maxStreamFailures-1)
	}
}

func TestAwaitStreamRequiresActionDoesNotFallBack(t *testing.T) {
	var plainGets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"id":"v1_ra","status":"in_progress"}`))
		case r.URL.Query().Get("stream") == "true":
			sseWrite(t, w,
				"id: e1\nevent: interaction.status_update\ndata: {\"interaction_id\":\"v1_ra\",\"status\":\"requires_action\"}\n\n",
			)
		default:
			plainGets.Add(1)
			w.Write([]byte(`{"id":"v1_ra","status":"requires_action"}`))
		}
	}))
	defer server.Close()

	r := newResearcher(t, server, streamOpts(nil, nil))
	ctx := context.Background()
	id, err := r.Start(ctx, researcher.Task{Query: "q"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	err = r.Await(ctx, id)
	if !errors.Is(err, interactions.ErrRequiresAction) {
		t.Fatalf("error = %v, want ErrRequiresAction — a research outcome, not a stream failure", err)
	}
	if plainGets.Load() != 0 {
		t.Error("requires_action must not trigger the poll fallback")
	}
}

func TestAwaitStreamErrorEventsSpendTheFailureBudget(t *testing.T) {
	var (
		streamGets atomic.Int64
		plainGets  atomic.Int64
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"id":"v1_e","status":"in_progress"}`))
		case r.URL.Query().Get("stream") == "true":
			streamGets.Add(1)
			// A delta (progress) followed by an error event, every
			// time. The budget is deliberately not reset by progress:
			// a stream alternating deltas with errors must exhaust it
			// rather than hold the await captive forever.
			sseWrite(t, w,
				"id: d1\nevent: step.delta\ndata: {\"index\":0,\"delta\":{\"type\":\"thought_summary_delta\",\"text\":\"t\"}}\n\n",
				"id: d2\nevent: error\ndata: {\"error\":{\"code\":\"uri:internal\",\"message\":\"stream broke\"}}\n\n",
			)
		default:
			plainGets.Add(1)
			w.Write([]byte(`{"id":"v1_e","status":"completed"}`))
		}
	}))
	defer server.Close()

	r := newResearcher(t, server, streamOpts(nil, nil))
	ctx := context.Background()
	id, err := r.Start(ctx, researcher.Task{Query: "q"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Await(ctx, id); err != nil {
		t.Fatalf("Await must conclude via the poll fallback, got %v", err)
	}
	if got := streamGets.Load(); got != maxStreamFailures {
		t.Errorf("stream attempts = %d, want %d — repeated error events must not hold the await captive", got, maxStreamFailures)
	}
	if plainGets.Load() == 0 {
		t.Error("the poll fallback never ran")
	}
}

func TestQuietPathNeverStreams(t *testing.T) {
	var streamGets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"id":"v1_q","status":"in_progress"}`))
		case r.URL.Query().Get("stream") == "true":
			streamGets.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		default:
			w.Write([]byte(`{"id":"v1_q","status":"completed"}`))
		}
	}))
	defer server.Close()

	// No streamOpts: Stream stays false — the --quiet path.
	r := newResearcher(t, server)
	ctx := context.Background()
	id, err := r.Start(ctx, researcher.Task{Query: "q"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Await(ctx, id); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if streamGets.Load() != 0 {
		t.Errorf("quiet await attached %d streams, want none", streamGets.Load())
	}
}

func TestThinkingSummariesFollowsStreamOption(t *testing.T) {
	for _, tc := range []struct {
		name      string
		summaries bool
		want      string
	}{
		{"streaming", true, "auto"},
		{"quiet", false, "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body struct {
				AgentConfig struct {
					ThinkingSummaries string `json:"thinking_summaries"`
				} `json:"agent_config"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					decodeJSONBody(t, r, &body)
					w.Write([]byte(`{"id":"v1_t","status":"in_progress"}`))
					return
				}
				w.Write([]byte(`{"id":"v1_t","status":"completed"}`))
			}))
			defer server.Close()

			r := newResearcher(t, server, func(o *Options) { o.ThinkingSummaries = tc.summaries })
			if _, err := r.Start(context.Background(), researcher.Task{Query: "q"}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if body.AgentConfig.ThinkingSummaries != tc.want {
				t.Errorf("thinking_summaries = %q, want %q", body.AgentConfig.ThinkingSummaries, tc.want)
			}
		})
	}
}

func TestStartResetsReconnectCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"id":"v1_rst","status":"in_progress"}`))
			return
		}
		w.Write([]byte(`{"id":"v1_rst","status":"completed"}`))
	}))
	defer server.Close()

	r := newResearcher(t, server)
	r.reconnectCount.Store(7) // residue from a previous run
	ctx := context.Background()
	id, err := r.Start(ctx, researcher.Task{Query: "q"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	in, err := r.Result(ctx, id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Usage.ReconnectCount != 0 {
		t.Errorf("reconnect count = %d, want 0 after a fresh Start", in.Usage.ReconnectCount)
	}
}

// decodeJSONBody decodes a request body, failing the test on error.
func decodeJSONBody(t *testing.T, r *http.Request, into any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		t.Fatalf("decoding request body: %v", err)
	}
}
