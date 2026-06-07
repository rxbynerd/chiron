package secret

import (
	"context"
	"fmt"
	"log/slog"
)

// ScrubHandler is an slog.Handler wrapper that scrubs every record —
// message, attribute keys, and attribute values, including those bound
// with WithAttrs — through Scrub before the inner handler sees anything.
// Wrapping the handler rather than the logger makes leakage structurally
// impossible: there is no path to the backend that bypasses the scrubber.
//
// Values of kind Any (errors included) are stringified and scrubbed
// rather than passed through, deliberately trading type fidelity for the
// guarantee that a credential cannot hide inside an opaque value.
type ScrubHandler struct {
	inner slog.Handler
}

// NewScrubHandler wraps inner so that every record it receives has
// already been scrubbed.
func NewScrubHandler(inner slog.Handler) *ScrubHandler {
	return &ScrubHandler{inner: inner}
}

// Enabled implements slog.Handler.
func (h *ScrubHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle implements slog.Handler, rebuilding the record with scrubbed
// message and attributes before delegating.
func (h *ScrubHandler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, Scrub(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(scrubAttr(a))
		return true
	})
	return h.inner.Handle(ctx, clean)
}

// WithAttrs implements slog.Handler, scrubbing the bound attributes
// before the inner handler stores them.
func (h *ScrubHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = scrubAttr(a)
	}
	return &ScrubHandler{inner: h.inner.WithAttrs(scrubbed)}
}

// WithGroup implements slog.Handler.
func (h *ScrubHandler) WithGroup(name string) slog.Handler {
	return &ScrubHandler{inner: h.inner.WithGroup(Scrub(name))}
}

// scrubAttr scrubs one attribute, recursing into groups. LogValuer
// values are resolved first so the scrubber sees what would be logged.
func scrubAttr(a slog.Attr) slog.Attr {
	key := Scrub(a.Key)
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(key, Scrub(v.String()))
	case slog.KindGroup:
		members := v.Group()
		scrubbed := make([]slog.Attr, len(members))
		for i, m := range members {
			scrubbed[i] = scrubAttr(m)
		}
		return slog.Attr{Key: key, Value: slog.GroupValue(scrubbed...)}
	case slog.KindAny:
		return slog.String(key, Scrub(fmt.Sprint(v.Any())))
	default:
		// Numeric, bool, time, and duration values cannot carry a
		// credential.
		return slog.Attr{Key: key, Value: v}
	}
}

var _ slog.Handler = (*ScrubHandler)(nil)
