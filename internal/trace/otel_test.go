package trace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newRecordedOTel returns an OTel tracer whose spans land in an
// in-memory recorder instead of an OTLP endpoint.
func newRecordedOTel() (*OTel, *tracetest.SpanRecorder) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	return &OTel{tr: tp.Tracer(instrumentationName)}, rec
}

func TestOTelSpansNestAndRecord(t *testing.T) {
	tr, rec := newRecordedOTel()

	rctx, root := tr.StartSpan(context.Background(), SpanResearch)
	actx, await := tr.StartSpan(rctx, SpanAwait)
	await.SetAttr("attempt", 3)
	tr.Metric(actx, MetricPollCount, 3)
	await.End(nil)
	tr.Metric(rctx, MetricInputTokens, 4096)
	root.End(nil)

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	awaitSpan, rootSpan := spans[0], spans[1]
	if awaitSpan.Name() != SpanAwait || rootSpan.Name() != SpanResearch {
		t.Fatalf("span names = %q, %q", awaitSpan.Name(), rootSpan.Name())
	}
	if awaitSpan.Parent().SpanID() != rootSpan.SpanContext().SpanID() {
		t.Fatal("await span does not nest beneath research")
	}

	attrs := attrMap(awaitSpan)
	if attrs["attempt"] != "3" {
		t.Fatalf("attempt attr = %q, want 3", attrs["attempt"])
	}
	if attrs["metric."+MetricPollCount] != "3" {
		t.Fatalf("metric attr = %q, want 3", attrs["metric."+MetricPollCount])
	}
	if attrMap(rootSpan)["metric."+MetricInputTokens] != "4096" {
		t.Fatal("root span missing input_tokens metric attribute")
	}
}

// TestOTelScrubsEverything proves a credential cannot reach the SDK via
// span name, attribute key, attribute value, stringified value, error,
// or metric name.
func TestOTelScrubsEverything(t *testing.T) {
	tr, rec := newRecordedOTel()

	ctx, span := tr.StartSpan(context.Background(), "research for "+fakeKey)
	span.SetAttr("header "+fakeKey, "x-goog-api-key: "+fakeKey)
	span.SetAttr("err_detail", errors.New("denied for "+fakeKey))
	tr.Metric(ctx, "tokens for "+fakeKey, 7)
	span.End(errors.New("auth failed: " + fakeKey))

	for _, s := range rec.Ended() {
		if strings.Contains(s.Name(), fakeKey) {
			t.Fatalf("credential in span name %q", s.Name())
		}
		for _, kv := range s.Attributes() {
			if strings.Contains(string(kv.Key), fakeKey) || strings.Contains(kv.Value.String(), fakeKey) {
				t.Fatalf("credential in attribute %s=%s", kv.Key, kv.Value.String())
			}
		}
		if strings.Contains(s.Status().Description, fakeKey) {
			t.Fatalf("credential in status %q", s.Status().Description)
		}
		for _, ev := range s.Events() {
			if strings.Contains(ev.Name, fakeKey) {
				t.Fatalf("credential in event name %q", ev.Name)
			}
			for _, kv := range ev.Attributes {
				if strings.Contains(kv.Value.String(), fakeKey) {
					t.Fatalf("credential in event attribute %s=%s", kv.Key, kv.Value.String())
				}
			}
		}
	}
}

func TestNewOTelConstructsAndShutsDown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, shutdown, err := NewOTel(ctx, "http://localhost:4318")
	if err != nil {
		t.Fatalf("NewOTel: %v", err)
	}
	if tr == nil || shutdown == nil {
		t.Fatal("NewOTel returned nil tracer or shutdown")
	}
	// No spans were recorded, so shutdown flushes nothing and must not
	// touch the network.
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func attrMap(s sdktrace.ReadOnlySpan) map[string]string {
	m := make(map[string]string)
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = fmt.Sprint(kv.Value.AsInterface())
	}
	return m
}
