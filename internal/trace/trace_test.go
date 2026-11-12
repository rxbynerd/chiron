package trace

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeKey is shaped exactly like a Google API key (AIza + 35 key
// characters) without being one; every binding's tests prove it cannot
// transit the tracer.
const fakeKey = "AIzaAb1-x9QzAb1-x9QzAb1-x9QzAb1-x9QzAb1"

func TestNoop_ReturnsContextAndIgnoresTelemetry(t *testing.T) {
	must := require.New(t)
	tr := Noop{}
	ctx := context.Background()
	got, span := tr.StartSpan(ctx, SpanResearch)
	must.Equal(ctx, got, "Noop.StartSpan must return the context unchanged")
	span.SetAttr("query", "anything")
	span.End(errors.New("ignored"))
	span.End(nil) // double End must be safe
	tr.Metric(ctx, MetricPollCount, 1)
}
