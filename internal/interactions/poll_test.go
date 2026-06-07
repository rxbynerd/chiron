package interactions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollUntilTerminal(t *testing.T) {
	terminals := []Status{
		StatusCompleted,
		StatusFailed,
		StatusCancelled,
		StatusIncomplete,
		StatusBudgetExceeded,
	}
	for _, terminal := range terminals {
		t.Run(string(terminal), func(t *testing.T) {
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
			if err != nil {
				t.Fatalf("PollUntilTerminal: %v", err)
			}
			if in.Status != terminal {
				t.Errorf("status = %q, want %q", in.Status, terminal)
			}
			if got := calls.Load(); got != 3 {
				t.Errorf("polls = %d, want 3", got)
			}
			if len(observed) != 3 || observed[0] != StatusInProgress || observed[2] != terminal {
				t.Errorf("OnPoll observed %v", observed)
			}
		})
	}
}

func TestPollRequiresActionReturnsTypedError(t *testing.T) {
	// requires_action cannot legitimately occur for deep research; the
	// poller must surface it immediately as ErrRequiresAction with the
	// snapshot attached, not hang until the server's 60-minute cap.
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"id":"v1_abc","status":"requires_action"}`)
	}))
	in, err := c.PollUntilTerminal(context.Background(), "v1_abc", PollConfig{Interval: time.Millisecond})
	if !errors.Is(err, ErrRequiresAction) {
		t.Fatalf("error = %v, want ErrRequiresAction", err)
	}
	if in == nil || in.Status != StatusRequiresAction {
		t.Errorf("snapshot = %+v, want requires_action interaction", in)
	}
}

func TestPollContextCancellation(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"id":"v1_abc","status":"in_progress"}`)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	_, err := c.PollUntilTerminal(ctx, "v1_abc", PollConfig{
		Interval: time.Hour, // ensure we are waiting, not polling, when cancelled
		OnPoll:   func(*Interaction) { cancel() },
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestPollPropagatesGetErrors(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}), WithMaxRetries(0))
	_, err := c.PollUntilTerminal(context.Background(), "v1_gone", PollConfig{Interval: time.Millisecond})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusNotFound {
		t.Errorf("error = %v, want APIError 404", err)
	}
}

func TestNextInterval(t *testing.T) {
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
			if got := nextInterval(tt.current, tt.cfg); got != tt.want {
				t.Errorf("nextInterval(%v) = %v, want %v", tt.current, got, tt.want)
			}
		})
	}
}

func TestPollConfigDefaults(t *testing.T) {
	cfg := PollConfig{}.withDefaults()
	if cfg.Interval != defaultPollInterval || cfg.MaxInterval != defaultPollMaxInterval || cfg.Multiplier != defaultPollMultiplier {
		t.Errorf("defaults = %+v", cfg)
	}
}
