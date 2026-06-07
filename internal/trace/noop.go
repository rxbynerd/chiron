package trace

import "context"

// Noop discards all spans and metrics. It is the binding for tests and
// for runs with tracing disabled, so the core can call the Tracer seam
// unconditionally.
type Noop struct{}

// StartSpan implements Tracer.
func (Noop) StartSpan(ctx context.Context, _ string) (context.Context, Span) {
	return ctx, noopSpan{}
}

// Metric implements Tracer.
func (Noop) Metric(context.Context, string, float64) {}

type noopSpan struct{}

func (noopSpan) SetAttr(string, any) {}

func (noopSpan) End(error) {}

var _ Tracer = Noop{}
