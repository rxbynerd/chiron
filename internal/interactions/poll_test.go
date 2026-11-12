package interactions

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPoll_StopsAtEachTerminalStatus(t *testing.T) {
	terminals := []Status{
		StatusCompleted,
		StatusFailed,
		StatusCancelled,
		StatusIncomplete,
		StatusBudgetExceeded,
	}
	for _, terminal := range terminals {
		t.Run(string(terminal), func(t *testing.T) {
			must := require.New(t)
			var calls atomic.Int32
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status := StatusInProgress
				if calls.Add(1) >= 3 {
					status = terminal
				}
				writeJSON(t, w, fmt.Sprintf(`{"id":"v1_abc","status":%q}`, status))
			}))

			var observed []Status
			in, err := c.PollUntilTerminal(context.Background(), "v1_abc", PollConfig{
				Interval:    time.Millisecond,
				MaxInterval: 2 * time.Millisecond,
				Multiplier:  1.5,
				OnPoll:      func(in *Interaction) { observed = append(observed, in.Status) },
			})
			must.NoError(err)
			must.Equal(terminal, in.Status)
			must.EqualValues(3, calls.Load())
			must.Len(observed, 3, "OnPoll observed %v", observed)
			must.Equal(StatusInProgress, observed[0])
			must.Equal(terminal, observed[2])
		})
	}
}

func TestPollRequiresActionReturnsTypedError(t *testing.T) {
	must := require.New(t)
	// requires_action cannot legitimately occur for deep research; the
	// poller must surface it immediately as ErrRequiresAction with the
	// snapshot attached, not hang until the server's 60-minute cap.
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"id":"v1_abc","status":"requires_action"}`)
	}))
	in, err := c.PollUntilTerminal(context.Background(), "v1_abc", PollConfig{Interval: time.Millisecond})
	must.ErrorIs(err, ErrRequiresAction)
	must.NotNil(in)
	must.Equal(StatusRequiresAction, in.Status)
}

func TestPollContextCancellation(t *testing.T) {
	must := require.New(t)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"id":"v1_abc","status":"in_progress"}`)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	_, err := c.PollUntilTerminal(ctx, "v1_abc", PollConfig{
		Interval: time.Hour, // ensure we are waiting, not polling, when cancelled
		OnPoll:   func(*Interaction) { cancel() },
	})
	must.ErrorIs(err, context.Canceled)
}

func TestPoll_PropagatesGetErrors(t *testing.T) {
	must := require.New(t)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}), WithMaxRetries(0))
	_, err := c.PollUntilTerminal(context.Background(), "v1_gone", PollConfig{Interval: time.Millisecond})
	var apiErr *APIError
	must.ErrorAs(err, &apiErr)
	must.Equal(http.StatusNotFound, apiErr.HTTPStatus)
}

func TestNextInterval_AppliesMultiplierAndCeiling(t *testing.T) {
	tests := []struct {
		name    string
		current time.Duration
		cfg     PollConfig
		want    time.Duration
	}{
		{"grows by multiplier", 10 * time.Second,
			PollConfig{MaxInterval: 60 * time.Second, Multiplier: 1.5}, 15 * time.Second},
		{"caps at ceiling", 50 * time.Second,
			PollConfig{MaxInterval: 60 * time.Second, Multiplier: 1.5}, 60 * time.Second},
		{"multiplier 1 keeps constant", 10 * time.Second,
			PollConfig{MaxInterval: 60 * time.Second, Multiplier: 1}, 10 * time.Second},
		{"constant interval still capped", 90 * time.Second,
			PollConfig{MaxInterval: 60 * time.Second, Multiplier: 1}, 60 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.want, nextInterval(tt.current, tt.cfg))
		})
	}
}

func TestPollConfig_WithDefaults_FillsUnsetFields(t *testing.T) {
	must := require.New(t)
	cfg := PollConfig{}.withDefaults()
	must.Equal(defaultPollInterval, cfg.Interval)
	must.Equal(defaultPollMaxInterval, cfg.MaxInterval)
	must.InDelta(defaultPollMultiplier, cfg.Multiplier, 0)
}
