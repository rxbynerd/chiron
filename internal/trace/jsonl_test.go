package trace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestJSONLSpansNestAndRecord(t *testing.T) {
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
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4 (metric, span, metric, span)", len(lines))
	}

	pollMetric, awaitSpan, costMetric, rootSpan := lines[0], lines[1], lines[2], lines[3]

	if rootSpan["name"] != SpanResearch || rootSpan["type"] != "span" {
		t.Fatalf("root span line = %v", rootSpan)
	}
	if rootSpan["parent_id"] != nil {
		t.Fatalf("root span has parent_id %v; want none", rootSpan["parent_id"])
	}
	if awaitSpan["parent_id"] != rootSpan["span_id"] {
		t.Fatalf("await parent_id = %v, want root span_id %v", awaitSpan["parent_id"], rootSpan["span_id"])
	}
	if awaitSpan["trace_id"] != rootSpan["trace_id"] {
		t.Fatal("child span does not share the root trace_id")
	}
	attrs, ok := awaitSpan["attrs"].(map[string]any)
	if !ok || attrs["attempt"] != float64(3) {
		t.Fatalf("await attrs = %v, want attempt=3", awaitSpan["attrs"])
	}

	if pollMetric["type"] != "metric" || pollMetric["name"] != MetricPollCount || pollMetric["value"] != float64(3) {
		t.Fatalf("poll metric line = %v", pollMetric)
	}
	if pollMetric["span_id"] != awaitSpan["span_id"] {
		t.Fatal("metric not attributed to the span in context")
	}
	if costMetric["span_id"] != rootSpan["span_id"] {
		t.Fatal("cost metric not attributed to the root span")
	}
}

// TestJSONLScrubsEverything proves a credential cannot transit the trace
// file via span name, attribute key, attribute value, non-string
// attribute, error message, or metric name.
func TestJSONLScrubsEverything(t *testing.T) {
	var buf bytes.Buffer
	tr := NewJSONL(&buf)

	ctx, span := tr.StartSpan(context.Background(), "research for "+fakeKey)
	span.SetAttr("header "+fakeKey, "x-goog-api-key: "+fakeKey)
	span.SetAttr("err_detail", errors.New("denied for "+fakeKey))
	tr.Metric(ctx, "tokens for "+fakeKey, 7)
	span.End(errors.New("auth failed: " + fakeKey))

	out := buf.String()
	if strings.Contains(out, fakeKey) {
		t.Fatalf("credential transited the jsonl tracer:\n%s", out)
	}
	if !strings.Contains(out, "[REDACTED") {
		t.Fatalf("no redaction markers in output:\n%s", out)
	}
	decodeLines(t, out) // every line must still be valid JSON
}

// TestJSONLScrubsLangfuseKeys proves a Langfuse key pair set on a span, raw
// or as the OTLP Basic Authorization header, never reaches the trace file.
func TestJSONLScrubsLangfuseKeys(t *testing.T) {
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
	for _, leaked := range []string{langfusePublic[6:], langfuseSecret[6:], basic} {
		if strings.Contains(out, leaked) {
			t.Fatalf("Langfuse credential %q transited the jsonl tracer:\n%s", leaked, out)
		}
	}
	decodeLines(t, out)
}

func TestJSONLDoubleEndWritesOnce(t *testing.T) {
	var buf bytes.Buffer
	tr := NewJSONL(&buf)
	_, span := tr.StartSpan(context.Background(), SpanEmit)
	span.End(nil)
	span.End(errors.New("late"))
	if lines := decodeLines(t, buf.String()); len(lines) != 1 {
		t.Fatalf("double End wrote %d lines, want 1", len(lines))
	}
}

func TestJSONLMetricWithoutSpan(t *testing.T) {
	var buf bytes.Buffer
	tr := NewJSONL(&buf)
	tr.Metric(context.Background(), MetricFailures, 1)
	lines := decodeLines(t, buf.String())
	if len(lines) != 1 || lines[0]["name"] != MetricFailures {
		t.Fatalf("metric without span = %v", lines)
	}
	if _, present := lines[0]["span_id"]; present {
		t.Fatal("unattributed metric carries a span_id")
	}
}

func decodeLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for raw := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("invalid JSON line %q: %v", raw, err)
		}
		lines = append(lines, m)
	}
	return lines
}
