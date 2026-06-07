package trace

import (
	"context"
	"errors"
	"testing"
)

// fakeKey is shaped exactly like a Google API key (AIza + 35 key
// characters) without being one; every binding's tests prove it cannot
// transit the tracer.
const fakeKey = "AIzaAb1-x9QzAb1-x9QzAb1-x9QzAb1-x9QzAb1"

func TestNoop(t *testing.T) {
	tr := Noop{}
	ctx := context.Background()
	got, span := tr.StartSpan(ctx, SpanResearch)
	if got != ctx {
		t.Fatal("Noop.StartSpan must return the context unchanged")
	}
	span.SetAttr("query", "anything")
	span.End(errors.New("ignored"))
	span.End(nil) // double End must be safe
	tr.Metric(ctx, MetricPollCount, 1)
}
