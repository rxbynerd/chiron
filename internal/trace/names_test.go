package trace

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVocabulary_WireNamesAreStable pins every span, attribute and metric name
// to its wire value: trace backends and dashboards key on these strings, so a
// rename has to be a deliberate change to this table.
func TestVocabulary_WireNamesAreStable(t *testing.T) {
	for _, tt := range []struct {
		name, got, want string
	}{
		{"span research", SpanResearch, "research"},
		{"span plan", SpanPlan, "plan"},
		{"span start", SpanStart, "start"},
		{"span await", SpanAwait, "await"},
		{"span format", SpanFormat, "format"},
		{"span emit", SpanEmit, "emit"},
		{"span worker", SpanWorker, "worker"},
		{"span decompose", SpanDecompose, "decompose"},
		{"span delegate", SpanDelegate, "delegate"},
		{"span synthesise", SpanSynthesise, "synthesise"},
		{"span cite", SpanCite, "cite"},
		{"span control plane", SpanControlPlane, "control_plane"},
		{"attribute worker_id", AttrWorkerID, "worker_id"},
		{"attribute brief_id", AttrBriefID, "brief_id"},
		{"attribute brief_count", AttrBriefCount, "brief_count"},
		{"metric task_duration_seconds", MetricTaskDurationSeconds, "task_duration_seconds"},
		{"metric poll_count", MetricPollCount, "poll_count"},
		{"metric search_count", MetricSearchCount, "search_count"},
		{"metric recall_count", MetricRecallCount, "recall_count"},
		{"metric input_tokens", MetricInputTokens, "input_tokens"},
		{"metric output_tokens", MetricOutputTokens, "output_tokens"},
		{"metric estimated_cost_gbp", MetricEstimatedCostGBP, "estimated_cost_gbp"},
		{"metric reconnect_count", MetricReconnectCount, "reconnect_count"},
		{"metric failures", MetricFailures, "failures"},
		{"metric web_cost_usd", MetricWebCostUSD, "web_cost_usd"},
		{"metric web_cost_unknown_calls", MetricWebCostUnknownCalls, "web_cost_unknown_calls"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.want, tt.got)
		})
	}
}

// TestFleetTrace_SingleWorkerSpanTree drives the reserved fleet vocabulary
// through the JSONL tracer the way a one-worker fleet run does, and asserts
// the span tree a backend rolls up per worker: decompose, delegate, synthesise
// and cite under the research root, and the worker under its delegate.
func TestFleetTrace_SingleWorkerSpanTree(t *testing.T) {
	must := require.New(t)

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
		must.NotContains(spans, name, "span %q written twice", name)
		spans[name] = line
	}
	must.Len(spans, 6, "spans: %v", spans)

	rootLine := spans[SpanResearch]
	must.Nil(rootLine["parent_id"], "research root must have no parent")

	for _, tt := range []struct {
		name, child, parent string
	}{
		{"decompose under research", SpanDecompose, SpanResearch},
		{"delegate under research", SpanDelegate, SpanResearch},
		{"worker under delegate", SpanWorker, SpanDelegate},
		{"synthesise under research", SpanSynthesise, SpanResearch},
		{"cite under research", SpanCite, SpanResearch},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			child, parent := spans[tt.child], spans[tt.parent]
			must.NotNil(child, "missing span %s", tt.child)
			must.NotNil(parent, "missing span %s", tt.parent)
			must.Equal(parent["span_id"], child["parent_id"])
			must.Equal(rootLine["trace_id"], child["trace_id"])
		})
	}

	t.Run("delegate attributes", func(t *testing.T) {
		must := require.New(t)
		delegateAttrs, _ := spans[SpanDelegate]["attrs"].(map[string]any)
		must.Equal("wkr_0123456789abcdef", delegateAttrs["worker_id"])
		must.Equal("brief-1", delegateAttrs["brief_id"])
	})

	t.Run("decompose attributes", func(t *testing.T) {
		must := require.New(t)
		decomposeAttrs, _ := spans[SpanDecompose]["attrs"].(map[string]any)
		must.InDelta(1, decomposeAttrs["brief_count"], 0)
	})
}
