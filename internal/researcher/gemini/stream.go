package gemini

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rxbynerd/chiron/internal/interactions"
)

// Streaming reconnect defaults: backoff mirrors the interactions
// client's retry backoff; the failure budget bounds how long a broken
// streaming path is retried before the await degrades to polling.
const (
	defaultReconnectBaseDelay = 500 * time.Millisecond
	defaultReconnectMaxDelay  = 8 * time.Second

	// maxStreamFailures is the per-await failure budget: dial
	// failures, broken streams, and server error events count against
	// it, and it is deliberately never reset by progress — a stream
	// alternating deltas with errors would otherwise hold the await
	// captive forever. When it is spent, Await falls back to polling:
	// the task is still running, and still spending, server-side, and
	// a long task that genuinely drops this many times concludes less
	// prettily but just as correctly by poll.
	maxStreamFailures = 4
)

// streamConfig is the resolved streaming behaviour shared by the
// adapter's entry points.
type streamConfig struct {
	enabled    bool
	onThought  func(string)
	onDegraded func()
	baseDelay  time.Duration
	maxDelay   time.Duration
}

func streamConfigFrom(opts Options) streamConfig {
	cfg := streamConfig{
		enabled:    opts.Stream,
		onThought:  opts.OnThought,
		onDegraded: opts.OnStreamDegraded,
		baseDelay:  opts.ReconnectBaseDelay,
		maxDelay:   opts.ReconnectMaxDelay,
	}
	if cfg.baseDelay <= 0 {
		cfg.baseDelay = defaultReconnectBaseDelay
	}
	if cfg.maxDelay <= 0 {
		cfg.maxDelay = defaultReconnectMaxDelay
	}
	return cfg
}

// awaitStream consumes the interaction's SSE stream until a terminal
// state, reconnecting on drops with the last seen event id
// (?last_event_id= query parameter — docs/INTERACTIONS-API.md §5) and
// capped exponential backoff. It returns nil once the interaction is
// terminal; an error wrapping interactions.ErrRequiresAction for the
// requires_action state; ctx.Err() on cancellation; and the last
// failure once the consecutive-failure budget is spent — the caller
// (Await) then degrades to polling. Each re-dial after the initial
// attach increments the reconnect_count cost signal, whether or not it
// succeeds.
func (r *Researcher) awaitStream(ctx context.Context, id string) error {
	var (
		lastEventID string
		failures    int
		dialled     bool
		lastErr     error
	)

	fail := func(err error) (giveUp bool) {
		failures++
		lastErr = err
		return failures >= maxStreamFailures
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if dialled {
			r.reconnectCount.Add(1)
		}
		stream, err := r.poll.Stream(ctx, id, lastEventID)
		dialled = true
		if err != nil {
			if fail(err) {
				return lastErr
			}
			if err := r.backoff(ctx, failures); err != nil {
				return err
			}
			continue
		}

		done, err := r.consumeStream(stream)
		lastEventID = stream.LastEventID()
		stream.Close()
		if done || err == nil {
			return err
		}
		if errors.Is(err, interactions.ErrRequiresAction) {
			return err
		}
		if fail(err) {
			return lastErr
		}
		if err := r.backoff(ctx, failures); err != nil {
			return err
		}
	}
}

// consumeStream reads one attached stream to its end. It returns
// done=true when the interaction reached a terminal state (err nil) or
// a state that must not be retried (requires_action); done=false with
// the drop's error when the stream ended without a terminal state and
// the caller should reconnect.
func (r *Researcher) consumeStream(stream *interactions.Stream) (done bool, err error) {
	for {
		ev, err := stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// A clean end without a terminal state is still a drop:
				// the server closed an in-flight stream (idle timeout,
				// rolling restart). Reconnect from the last event.
				return false, fmt.Errorf("gemini: stream ended before the interaction concluded: %w", err)
			}
			return false, err
		}

		switch ev.Type {
		case interactions.EventError:
			// A server-reported stream error counts against the
			// failure budget like any other drop.
			return false, fmt.Errorf("gemini: stream error event: %w", ev.Err)
		case interactions.EventStepDelta:
			if r.streamCfg.onThought != nil && ev.Delta != nil &&
				ev.Delta.Type == interactions.DeltaThoughtSummary && ev.Delta.Text != "" {
				r.streamCfg.onThought(ev.Delta.Text)
			}
		case interactions.EventInteractionCreated, interactions.EventInteractionCompleted:
			// interaction.completed may omit content; the run core's
			// Result call re-GETs the full resource afterwards
			// (docs/INTERACTIONS-API.md §5), so only the status matters
			// here. interaction.created carries the current resource —
			// already terminal when re-attaching to a finished run.
			if ev.Type == interactions.EventInteractionCompleted ||
				(ev.Interaction != nil && terminalOrRequiresAction(ev.Interaction.Status)) {
				return true, requiresActionErr(eventStatus(ev))
			}
		case interactions.EventInteractionStatusUpdate:
			if terminalOrRequiresAction(ev.Status) {
				return true, requiresActionErr(ev.Status)
			}
		default:
			// Unknown event types are forward compatibility
			// (docs/INTERACTIONS-API.md §5); tolerate and read on.
		}
	}
}

// eventStatus extracts the interaction status carried by a created or
// completed event, when there is one.
func eventStatus(ev *interactions.Event) interactions.Status {
	if ev.Interaction != nil {
		return ev.Interaction.Status
	}
	if ev.Type == interactions.EventInteractionCompleted {
		return interactions.StatusCompleted
	}
	return ""
}

// terminalOrRequiresAction reports whether the status ends the await:
// any terminal state, or requires_action — not terminal, but a state
// deep research cannot legitimately produce and polling would not
// resolve (docs/INTERACTIONS-API.md §4).
func terminalOrRequiresAction(s interactions.Status) bool {
	return s.Terminal() || s == interactions.StatusRequiresAction
}

// requiresActionErr returns ErrRequiresAction for the requires_action
// state and nil for every other concluded state, mirroring
// PollUntilTerminal's contract.
func requiresActionErr(s interactions.Status) error {
	if s == interactions.StatusRequiresAction {
		return interactions.ErrRequiresAction
	}
	return nil
}

// backoff sleeps before reconnect attempt n (1-based), doubling from
// the base delay to the cap, returning early on cancellation.
func (r *Researcher) backoff(ctx context.Context, n int) error {
	if n > 30 { // avoid shift overflow, matching client.go's backoffDelay
		n = 30
	}
	// Left-shifting time.Duration (int64) wraps on overflow — defined
	// behaviour in Go — so d <= 0 below catches a wrapped negative.
	d := r.streamCfg.baseDelay << (n - 1)
	if d <= 0 || d > r.streamCfg.maxDelay {
		d = r.streamCfg.maxDelay
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
