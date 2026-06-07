package trace

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/chiron/internal/secret"
)

// instrumentationName identifies Chiron's tracer to the backend.
const instrumentationName = "github.com/rxbynerd/chiron/internal/trace"

// OTel is the production Tracer binding: spans over OTLP/HTTP to the
// suite's Langfuse/Grafana backend. As with the JSONL binding, every
// string payload — span names, attribute keys and values, error
// messages, metric names — passes through secret.Scrub before the SDK
// sees it.
//
// Metrics are recorded as attributes (metric.<name>) on the span carried
// by the context rather than through the OTel metrics SDK — a deliberate
// dependency-surface decision recorded in docs/DECISIONS.md.
type OTel struct {
	tr oteltrace.Tracer
}

// NewOTel builds the OTLP/HTTP exporter and returns the tracer plus a
// shutdown function that flushes pending spans; callers must invoke it
// before exit or trailing spans are lost. An empty endpointURL defers to
// the standard OTEL_EXPORTER_OTLP_* environment variables.
func NewOTel(ctx context.Context, endpointURL string) (*OTel, func(context.Context) error, error) {
	var opts []otlptracehttp.Option
	if endpointURL != "" {
		opts = append(opts, otlptracehttp.WithEndpointURL(endpointURL))
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("trace: creating OTLP exporter: %w", err)
	}
	res, err := sdkresource.Merge(
		sdkresource.Default(),
		sdkresource.NewSchemaless(attribute.String("service.name", "chiron")),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("trace: building resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	return &OTel{tr: tp.Tracer(instrumentationName)}, tp.Shutdown, nil
}

// StartSpan implements Tracer. Nesting rides on OTel's own context
// propagation.
func (t *OTel) StartSpan(ctx context.Context, name string) (context.Context, Span) {
	ctx, sp := t.tr.Start(ctx, secret.Scrub(name))
	return ctx, otelSpan{sp: sp}
}

// Metric implements Tracer, recording the measurement as a metric.<name>
// attribute on the current span. With no span in the context the SDK
// hands back a no-op span and the measurement is dropped, matching the
// seam's best-effort contract.
func (t *OTel) Metric(ctx context.Context, name string, value float64) {
	oteltrace.SpanFromContext(ctx).SetAttributes(
		attribute.Float64("metric."+secret.Scrub(name), value),
	)
}

type otelSpan struct {
	sp oteltrace.Span
}

// SetAttr implements Span, mapping native Go scalars onto OTel attribute
// types and stringifying (then scrubbing) anything else.
func (s otelSpan) SetAttr(key string, value any) {
	k := secret.Scrub(key)
	var kv attribute.KeyValue
	switch v := value.(type) {
	case string:
		kv = attribute.String(k, secret.Scrub(v))
	case bool:
		kv = attribute.Bool(k, v)
	case int:
		kv = attribute.Int(k, v)
	case int64:
		kv = attribute.Int64(k, v)
	case float64:
		kv = attribute.Float64(k, v)
	default:
		kv = attribute.String(k, secret.Scrub(fmt.Sprint(v)))
	}
	s.sp.SetAttributes(kv)
}

// End implements Span. The error is rebuilt from its scrubbed message so
// the original — which may carry a credential in an HTTP error body —
// never reaches the SDK.
func (s otelSpan) End(err error) {
	if err != nil {
		msg := secret.Scrub(err.Error())
		s.sp.RecordError(errors.New(msg))
		s.sp.SetStatus(codes.Error, msg)
	}
	s.sp.End()
}

var _ Tracer = (*OTel)(nil)
