package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// sseWrite writes one SSE response from the given frames. It runs on HTTP
// handler goroutines, so it reports through check rather than stopping the test.
func sseWrite(t *testing.T, w http.ResponseWriter, frames ...string) {
	t.Helper()
	check := assert.New(t)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, f := range frames {
		if _, err := fmt.Fprint(w, f); err != nil {
			check.Failf("writing SSE frame", "%v", err)
			return
		}
	}
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func TestAwait_Stream_CompletesWithoutReconnect(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	must.NoError(r.Await(ctx, id))

	mu.Lock()
	defer mu.Unlock()
	check.Equal([]string{"reading sources", "drafting"}, thoughts, "thought summaries arrive in order")
	check.EqualValues(1, streamGets.Load(), "stream attaches: no reconnect on a clean completion")
	check.Empty(gotLastID.Load(), "first attach must not send last_event_id")

	in, err := r.Result(ctx, id)
	must.NoError(err)
	check.Zero(in.Usage.ReconnectCount, "reconnect count")
	check.Zero(in.Usage.PollCount, "poll count: the await streamed")
}

func TestAwaitReconnectsWithLastEventID(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	must.NoError(r.Await(ctx, id))

	mu.Lock()
	defer mu.Unlock()
	must.Len(lastIDs, 2, "want the initial attach plus one reconnect")
	check.Empty(lastIDs[0], "initial attach must not send last_event_id")
	check.Equal("e7", lastIDs[1], "reconnect resumes via the last_event_id query parameter")

	in, err := r.Result(ctx, id)
	must.NoError(err)
	check.Equal(1, in.Usage.ReconnectCount, "reconnect count")
}

func TestAwaitFallsBackToPollingWhenStreamingRepeatedlyFails(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	must.NoError(r.Await(ctx, id), "Await must fall back to polling")

	check.EqualValues(maxStreamFailures, streamGets.Load(), "stream attempts spend the whole failure budget")
	check.Positive(plainGets.Load(), "the poll fallback must poll")

	in, err := r.Result(ctx, id)
	must.NoError(err)
	check.Positive(in.Usage.PollCount, "poll count must record the fallback polling")
	check.Equal(maxStreamFailures-1, in.Usage.ReconnectCount, "reconnect count: re-dials after the first attach")
}

func TestAwaitStreamRequiresActionDoesNotFallBack(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.ErrorIs(r.Await(ctx, id), interactions.ErrRequiresAction, "a research outcome, not a stream failure")
	must.Zero(plainGets.Load(), "requires_action must not trigger the poll fallback")
}

func TestAwaitStreamErrorEventsSpendTheFailureBudget(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	must.NoError(r.Await(ctx, id), "Await must conclude via the poll fallback")
	check.EqualValues(maxStreamFailures, streamGets.Load(), "repeated error events must not hold the await captive")
	check.Positive(plainGets.Load(), "the poll fallback must run")
}

func TestAwait_QuietPath_NeverStreams(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.NoError(r.Await(ctx, id))
	must.Zero(streamGets.Load(), "quiet await must not attach a stream")
}

func TestThinkingSummariesFollowsStreamOption(t *testing.T) {
	for _, tt := range []struct {
		name      string
		summaries bool
		want      string
	}{
		{"streaming", true, "auto"},
		{"quiet", false, "none"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
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

			r := newResearcher(t, server, func(o *Options) { o.ThinkingSummaries = tt.summaries })
			_, err := r.Start(context.Background(), researcher.Task{Query: "q"})
			must.NoError(err)
			must.Equal(tt.want, body.AgentConfig.ThinkingSummaries, "thinking_summaries")
		})
	}
}

func TestStartResetsReconnectCount(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	in, err := r.Result(ctx, id)
	must.NoError(err)
	must.Zero(in.Usage.ReconnectCount, "reconnect count after a fresh Start")
}

// decodeJSONBody decodes a request body into into. It runs on HTTP handler
// goroutines, so a decode error is reported through check without stopping the test.
func decodeJSONBody(t *testing.T, r *http.Request, into any) {
	t.Helper()
	check := assert.New(t)
	check.NoError(json.NewDecoder(r.Body).Decode(into), "decoding request body")
}

// An interaction.completed event concludes the streaming await, whether the
// resource arrives nested under "interaction" or the frame omits it
// (docs/INTERACTIONS-API.md). The poll fallback is never reached.
func TestAwaitStreamInteractionCompletedEvent(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"embedded interaction", `{"interaction":{"id":"v1_done","status":"completed"}}`},
		{"no embedded interaction", `{"event_id":"e9"}`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			var plainGets atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost:
					w.Write([]byte(`{"id":"v1_done","status":"in_progress"}`))
				case r.URL.Query().Get("stream") == "true":
					sseWrite(t, w, "id: e9\nevent: interaction.completed\ndata: "+tt.data+"\n\n")
				default:
					plainGets.Add(1)
					w.Write([]byte(`{"id":"v1_done","status":"completed"}`))
				}
			}))
			defer server.Close()

			r := newResearcher(t, server, streamOpts(nil, nil))
			ctx := context.Background()
			id, err := r.Start(ctx, researcher.Task{Query: "q"})
			must.NoError(err)
			must.NoError(r.Await(ctx, id), "a completed event concludes the await")
			must.Zero(plainGets.Load(), "the poll fallback must not run for a cleanly completed stream")

			in, err := r.Result(ctx, id)
			must.NoError(err)
			must.Zero(in.Usage.ReconnectCount, "reconnect count")
			must.Zero(in.Usage.PollCount, "poll count")
		})
	}
}

// A requires_action status carried on an interaction.completed event surfaces
// as interactions.ErrRequiresAction rather than as a completed interaction.
func TestAwaitStreamRequiresActionViaCompletedEvent(t *testing.T) {
	must := require.New(t)
	var plainGets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"id":"v1_rac","status":"in_progress"}`))
		case r.URL.Query().Get("stream") == "true":
			sseWrite(t, w, "id: e1\nevent: interaction.completed\ndata: {\"interaction\":{\"id\":\"v1_rac\",\"status\":\"requires_action\"}}\n\n")
		default:
			plainGets.Add(1)
			w.Write([]byte(`{"id":"v1_rac","status":"requires_action"}`))
		}
	}))
	defer server.Close()

	r := newResearcher(t, server, streamOpts(nil, nil))
	ctx := context.Background()
	id, err := r.Start(ctx, researcher.Task{Query: "q"})
	must.NoError(err)
	must.ErrorIs(r.Await(ctx, id), interactions.ErrRequiresAction, "requires_action via the completed-event path")
	must.Zero(plainGets.Load(), "requires_action via a completed event must not trigger the poll fallback")
}

// Re-attaching to a finished interaction delivers interaction.created with a
// terminal status as the first event. The await concludes immediately.
func TestAwaitStreamInteractionCreatedAlreadyTerminal(t *testing.T) {
	must := require.New(t)
	var plainGets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"id":"v1_done","status":"in_progress"}`))
		case r.URL.Query().Get("stream") == "true":
			sseWrite(t, w, "id: e1\nevent: interaction.created\ndata: {\"id\":\"v1_done\",\"status\":\"completed\"}\n\n")
		default:
			plainGets.Add(1)
			w.Write([]byte(`{"id":"v1_done","status":"completed"}`))
		}
	}))
	defer server.Close()

	r := newResearcher(t, server, streamOpts(nil, nil))
	ctx := context.Background()
	id, err := r.Start(ctx, researcher.Task{Query: "q"})
	must.NoError(err)
	must.NoError(r.Await(ctx, id), "an already-terminal created event concludes the await")
	must.Zero(plainGets.Load(), "the poll fallback must not run for an already-terminal re-attach")
}

// Cancellation while the await sleeps between reconnect attempts returns
// context.Canceled. Context cancellation never falls back to polling
// (docs/DECISIONS.md).
func TestAwaitStreamCancelledDuringBackoff(t *testing.T) {
	must := require.New(t)
	dialled := make(chan struct{})
	var (
		once      sync.Once
		plainGets atomic.Int64
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"id":"v1_cb","status":"in_progress"}`))
		case r.URL.Query().Get("stream") == "true":
			// Not an event stream: the dial fails and the await enters
			// its backoff sleep.
			once.Do(func() { close(dialled) })
			w.Write([]byte(`{"id":"v1_cb","status":"in_progress"}`))
		default:
			plainGets.Add(1)
			w.Write([]byte(`{"id":"v1_cb","status":"in_progress"}`))
		}
	}))
	defer server.Close()

	r := newResearcher(t, server, func(o *Options) {
		o.Stream = true
		// A backoff long enough that the cancellation below lands
		// mid-sleep, never expiring within the test.
		o.ReconnectBaseDelay = time.Hour
		o.ReconnectMaxDelay = time.Hour
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, err := r.Start(ctx, researcher.Task{Query: "q"})
	must.NoError(err)

	done := make(chan error, 1)
	go func() { done <- r.Await(ctx, id) }()
	<-dialled
	time.Sleep(20 * time.Millisecond) // let the failed dial reach the backoff sleep
	cancel()

	must.ErrorIs(<-done, context.Canceled)
	must.Zero(plainGets.Load(), "cancellation must not fall back to polling")
}

// Exhausting the streaming failure budget calls OnStreamDegraded exactly once,
// before the poll fallback. An operator watching the event stream relies on
// this in-flight signal.
func TestAwait_Stream_SignalsDegradationOnce(t *testing.T) {
	must := require.New(t)
	var degraded atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"id":"v1_dg","status":"in_progress"}`))
		case r.URL.Query().Get("stream") == "true":
			w.Write([]byte(`{"id":"v1_dg","status":"in_progress"}`)) // not SSE: every dial fails
		default:
			w.Write([]byte(`{"id":"v1_dg","status":"completed"}`))
		}
	}))
	defer server.Close()

	r := newResearcher(t, server, streamOpts(nil, nil), func(o *Options) {
		o.OnStreamDegraded = func() { degraded.Add(1) }
	})
	ctx := context.Background()
	id, err := r.Start(ctx, researcher.Task{Query: "q"})
	must.NoError(err)
	must.NoError(r.Await(ctx, id), "Await must conclude via the poll fallback")
	must.EqualValues(1, degraded.Load(), "OnStreamDegraded calls")
}
