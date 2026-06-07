// Package trace defines the Tracer seam: spans and run metrics. v1 binds an
// OpenTelemetry emitter (plus newline-delimited JSON for local debugging);
// the shape is unchanged in v2.
package trace

import "context"

// Tracer records spans and metrics for a run. The run core opens a root
// `research` span with child spans for plan, start, await, format, and
// emit; implementations route them to the suite's observability backend.
type Tracer interface {
	// StartSpan begins a span and returns a context carrying it; child
	// spans started from that context nest beneath it.
	StartSpan(ctx context.Context, name string) (context.Context, Span)

	// Metric records a named measurement for the current run (for example
	// poll_count, input_tokens, estimated_cost_gbp).
	Metric(ctx context.Context, name string, value float64)
}

// Span is one unit of traced work.
type Span interface {
	// SetAttr attaches an attribute to the span.
	SetAttr(key string, value any)

	// End closes the span, recording err if the work failed.
	End(err error)
}
