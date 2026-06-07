package interactions

import (
	"context"
	"time"
)

// Polling defaults: deep-research tasks run for minutes (most under 20,
// hard max 60 — docs/INTERACTIONS-API.md §7), so polling starts at 10s
// and backs off towards a 60s ceiling.
const (
	defaultPollInterval    = 10 * time.Second
	defaultPollMaxInterval = 60 * time.Second
	defaultPollMultiplier  = 1.5
)

// PollConfig configures PollUntilTerminal. The zero value polls every
// 10s, backing off ×1.5 to a 60s ceiling.
type PollConfig struct {
	// Interval is the initial wait between polls.
	Interval time.Duration
	// MaxInterval caps the backed-off wait.
	MaxInterval time.Duration
	// Multiplier grows the wait after each poll; values <= 1 keep the
	// interval constant.
	Multiplier float64
	// OnPoll, when set, observes each retrieved snapshot — for poll
	// counting and status display. It is called synchronously and must
	// not block.
	OnPoll func(*Interaction)
}

func (cfg PollConfig) withDefaults() PollConfig {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultPollInterval
	}
	if cfg.MaxInterval <= 0 {
		cfg.MaxInterval = defaultPollMaxInterval
	}
	if cfg.Multiplier <= 0 {
		cfg.Multiplier = defaultPollMultiplier
	}
	return cfg
}

// PollUntilTerminal polls Get until the interaction reaches a terminal
// status — anything other than in_progress, per the full enum in
// docs/INTERACTIONS-API.md §4, so requires_action and budget_exceeded
// stop the poll rather than hanging it. It returns the terminal snapshot
// with a nil error; mapping failure variants to errors is the caller's
// concern (Status.Failed helps). It returns early with ctx.Err() on
// cancellation, or with the Get error if a retrieval fails after the
// client's own transient-error retries.
func (c *Client) PollUntilTerminal(ctx context.Context, id string, cfg PollConfig) (*Interaction, error) {
	cfg = cfg.withDefaults()
	interval := cfg.Interval
	for {
		in, err := c.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if cfg.OnPoll != nil {
			cfg.OnPoll(in)
		}
		if in.Status.Terminal() {
			return in, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		interval = nextInterval(interval, cfg)
	}
}

// nextInterval grows the poll interval by cfg.Multiplier, capped at
// cfg.MaxInterval.
func nextInterval(current time.Duration, cfg PollConfig) time.Duration {
	if cfg.Multiplier <= 1 {
		return min(current, cfg.MaxInterval)
	}
	next := time.Duration(float64(current) * cfg.Multiplier)
	if next <= 0 || next > cfg.MaxInterval {
		next = cfg.MaxInterval
	}
	return next
}
