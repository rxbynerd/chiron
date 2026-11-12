package trace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONL_Spans_NestAndRecord(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	tr := NewJSONL(&buf)
	ctx := context.Background()

	rctx, root := tr.StartSpan(ctx, SpanResearch)
	actx, await := tr.StartSpan(rctx, SpanAwait)
	await.SetAttr("attempt", 3)
	tr.Metric(actx, MetricPollCount, 3)
	await.End(nil)
	tr.Metric(rctx, MetricEstimatedCostGBP, 1.25)
	root.End(nil)

	lines := decodeLines(t, buf.String())
	must.Len(lines, 4, "want metric, span, metric, span")

	pollMetric, awaitSpan, costMetric, rootSpan := lines[0], lines[1], lines[2], lines[3]

	must.Equal(SpanResearch, rootSpan["name"])
	must.Equal("span", rootSpan["type"])
	must.Nil(rootSpan["parent_id"], "root span should have no parent_id")
	must.Equal(rootSpan["span_id"], awaitSpan["parent_id"], "await span should be a child of the root span")
	must.Equal(rootSpan["trace_id"], awaitSpan["trace_id"], "child span should share the root trace_id")

	attrs, ok := awaitSpan["attrs"].(map[string]any)
	must.True(ok, "await span should carry an attrs object")
	must.InDelta(3, attrs["attempt"], 0)

	must.Equal("metric", pollMetric["type"])
	must.Equal(MetricPollCount, pollMetric["name"])
	must.InDelta(3, pollMetric["value"], 0)
	must.Equal(awaitSpan["span_id"], pollMetric["span_id"], "poll metric should be attributed to the span in context")
	must.Equal(rootSpan["span_id"], costMetric["span_id"], "cost metric should be attributed to the root span")
}

// TestJSONL_Scrub_EveryField proves a credential cannot transit the trace
// file via span name, attribute key, attribute value, non-string
// attribute, error message, or metric name.
func TestJSONL_Scrub_EveryField(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	tr := NewJSONL(&buf)

	ctx, span := tr.StartSpan(context.Background(), "research for "+fakeKey)
	span.SetAttr("header "+fakeKey, "x-goog-api-key: "+fakeKey)
	span.SetAttr("err_detail", errors.New("denied for "+fakeKey))
	tr.Metric(ctx, "tokens for "+fakeKey, 7)
	span.End(errors.New("auth failed: " + fakeKey))

	out := buf.String()
	must.NotContains(out, fakeKey, "credential transited the jsonl tracer:\n%s", out)
	must.Contains(out, "[REDACTED", "no redaction markers in output:\n%s", out)
	decodeLines(t, out) // every line must still be valid JSON
}

// TestJSONL_Scrub_LangfuseKeyPair proves a Langfuse key pair set on a span, raw
// or as the OTLP Basic Authorization header, never reaches the trace file.
func TestJSONL_Scrub_LangfuseKeyPair(t *testing.T) {
	const (
		langfusePublic = "pk-lf-1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
		langfuseSecret = "sk-lf-9f8e7d6c-5b4a-4321-8fed-cba987654321"
	)
	basic := base64.StdEncoding.EncodeToString([]byte(langfusePublic + ":" + langfuseSecret))

	var buf bytes.Buffer
	tr := NewJSONL(&buf)
	_, span := tr.StartSpan(context.Background(), SpanControlPlane)
	span.SetAttr("langfuse_public_key", langfusePublic)
	span.SetAttr("langfuse_secret_key", langfuseSecret)
	span.SetAttr("otlp_headers", "Authorization=Basic%20"+basic)
	span.End(errors.New("export rejected for " + langfuseSecret))

	out := buf.String()
	leaks := []struct {
		name, value string
	}{
		{"public key", langfusePublic[6:]},
		{"secret key", langfuseSecret[6:]},
		{"basic pair", basic},
	}
	for _, tt := range leaks {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(out, tt.value, "Langfuse credential transited the jsonl tracer:\n%s", out)
		})
	}
	decodeLines(t, out)
}

// TestJSONL_Scrub_BasicPairInValue proves a Basic credential pair is scrubbed
// from an attribute value even though the value never names the header.
func TestJSONL_Scrub_BasicPairInValue(t *testing.T) {
	must := require.New(t)
	const (
		langfusePublic = "pk-lf-1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
		langfuseSecret = "sk-lf-9f8e7d6c-5b4a-4321-8fed-cba987654321"
	)
	pair := base64.StdEncoding.EncodeToString([]byte(langfusePublic + ":" + langfuseSecret))

	var buf bytes.Buffer
	tr := NewJSONL(&buf)
	_, span := tr.StartSpan(context.Background(), SpanControlPlane)
	span.SetAttr("http.request.header.authorization", "Basic "+pair)
	span.End(nil)

	out := buf.String()
	must.NotContains(out, pair, "Basic credential pair transited the jsonl tracer:\n%s", out)
	must.Contains(out, "[REDACTED", "no redaction marker in output:\n%s", out)
	decodeLines(t, out)
}

func TestJSONL_Span_DoubleEndWritesOnce(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	tr := NewJSONL(&buf)
	_, span := tr.StartSpan(context.Background(), SpanEmit)
	span.End(nil)
	span.End(errors.New("late"))

	lines := decodeLines(t, buf.String())
	must.Len(lines, 1, "double End should write one line")
}

func TestJSONL_Metric_WithoutSpanOmitsSpanID(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	tr := NewJSONL(&buf)
	tr.Metric(context.Background(), MetricFailures, 1)

	lines := decodeLines(t, buf.String())
	must.Len(lines, 1)
	must.Equal(MetricFailures, lines[0]["name"])
	must.NotContains(lines[0], "span_id", "unattributed metric should not carry a span_id")
}

func decodeLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	must := require.New(t)
	var lines []map[string]any
	for raw := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var m map[string]any
		err := json.Unmarshal([]byte(raw), &m)
		must.NoError(err, "invalid JSON line %q", raw)
		lines = append(lines, m)
	}
	return lines
}
