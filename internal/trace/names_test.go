package trace

import (
	"bytes"
	"context"
	"testing"
)

// TestVocabularyIsStable pins every span, attribute and metric name to its
// wire value: trace backends and dashboards key on these strings, so a
// rename has to be a deliberate change to this table.
func TestVocabularyIsStable(t *testing.T) {
	for _, tt := range []struct {
		got, want string
	}{
		{SpanResearch, "research"},
		{SpanPlan, "plan"},
		{SpanStart, "start"},
		{SpanAwait, "await"},
		{SpanFormat, "format"},
		{SpanEmit, "emit"},
		{SpanWorker, "worker"},
		{SpanDecompose, "decompose"},
		{SpanDelegate, "delegate"},
		{SpanSynthesise, "synthesise"},
		{SpanCite, "cite"},
		{SpanControlPlane, "control_plane"},
		{AttrWorkerID, "worker_id"},
		{AttrBriefID, "brief_id"},
		{AttrBriefCount, "brief_count"},
		{MetricTaskDurationSeconds, "task_duration_seconds"},
		{MetricPollCount, "poll_count"},
		{MetricSearchCount, "search_count"},
		{MetricRecallCount, "recall_count"},
		{MetricInputTokens, "input_tokens"},
		{MetricOutputTokens, "output_tokens"},
		{MetricEstimatedCostGBP, "estimated_cost_gbp"},
		{MetricReconnectCount, "reconnect_count"},
		{MetricFailures, "failures"},
	} {
		if tt.got != tt.want {
			t.Errorf("vocabulary entry = %q, want %q", tt.got, tt.want)
		}
	}
}

// TestSyntheticSingleWorkerFleetTopology drives the reserved fleet
// vocabulary through the JSONL tracer the way a one-worker fleet run does,
// and asserts the span tree a backend rolls up per worker: decompose,
// delegate, synthesise and cite under the research root, and the worker
// under its delegate.
func TestSyntheticSingleWorkerFleetTopology(t *testing.T) {
	var buf bytes.Buffer
	tr := NewJSONL(&buf)

	rctx, root := tr.StartSpan(context.Background(), SpanResearch)

	_, decompose := tr.StartSpan(rctx, SpanDecompose)
	decompose.SetAttr(AttrBriefCount, 1)
	decompose.End(nil)

	dctx, delegate := tr.StartSpan(rctx, SpanDelegate)
	delegate.SetAttr(AttrWorkerID, "wkr_0123456789abcdef")
	delegate.SetAttr(AttrBriefID, "brief-1")
	_, worker := tr.StartSpan(dctx, SpanWorker)
	worker.SetAttr("status", "completed")
	worker.End(nil)
	delegate.End(nil)

	_, synthesise := tr.StartSpan(rctx, SpanSynthesise)
	synthesise.End(nil)
	_, cite := tr.StartSpan(rctx, SpanCite)
	cite.End(nil)
	root.End(nil)

	spans := make(map[string]map[string]any)
	for _, line := range decodeLines(t, buf.String()) {
		name, _ := line["name"].(string)
		if _, dup := spans[name]; dup {
			t.Fatalf("span %q written twice", name)
		}
		spans[name] = line
	}
	if len(spans) != 6 {
		t.Fatalf("got %d spans, want 6: %v", len(spans), spans)
	}

	rootLine := spans[SpanResearch]
	if rootLine["parent_id"] != nil {
		t.Fatalf("research root has parent_id %v; want none", rootLine["parent_id"])
	}
	for _, tt := range []struct {
		child, parent string
	}{
		{SpanDecompose, SpanResearch},
		{SpanDelegate, SpanResearch},
		{SpanWorker, SpanDelegate},
		{SpanSynthesise, SpanResearch},
		{SpanCite, SpanResearch},
	} {
		child, parent := spans[tt.child], spans[tt.parent]
		if child == nil || parent == nil {
			t.Fatalf("missing span: %s=%v, %s=%v", tt.child, child, tt.parent, parent)
		}
		if child["parent_id"] != parent["span_id"] {
			t.Errorf("%s parent_id = %v, want %s span_id %v", tt.child, child["parent_id"], tt.parent, parent["span_id"])
		}
		if child["trace_id"] != rootLine["trace_id"] {
			t.Errorf("%s trace_id = %v, want the root's %v", tt.child, child["trace_id"], rootLine["trace_id"])
		}
	}

	delegateAttrs, _ := spans[SpanDelegate]["attrs"].(map[string]any)
	if delegateAttrs["worker_id"] != "wkr_0123456789abcdef" || delegateAttrs["brief_id"] != "brief-1" {
		t.Errorf("delegate attrs = %v, want worker_id and brief_id", delegateAttrs)
	}
	decomposeAttrs, _ := spans[SpanDecompose]["attrs"].(map[string]any)
	if decomposeAttrs["brief_count"] != float64(1) {
		t.Errorf("decompose attrs = %v, want brief_count=1", decomposeAttrs)
	}
}
