package trace

// Span names per PROPOSAL §4.5: one research root, child spans per run
// phase. Await and reconnect spans recur (one per poll, one per
// reconnect attempt). The run core and every Tracer binding share this
// vocabulary so backends can rely on stable names.
const (
	SpanResearch = "research"
	SpanPlan     = "plan"
	SpanStart    = "start"
	SpanAwait    = "await"
	SpanFormat   = "format"
	SpanEmit     = "emit"
)

// Worker span names for Chiron's in-process research agents
// (docs/V2-RESEARCH-AGENT §5). SpanWorker wraps one worker's bounded
// search -> read -> synthesise loop; Wave 4's lead reuses it for its
// per-worker delegate spans. These extend the fixed run-core vocabulary
// above for the fleet path without changing the run core's contract.
const (
	SpanWorker = "worker"
)

// Metric names per PROPOSAL §4.5. Values are float64 throughout the
// Tracer seam; counts are recorded as whole numbers.
const (
	MetricTaskDurationSeconds = "task_duration_seconds"
	MetricPollCount           = "poll_count"
	MetricSearchCount         = "search_count"
	MetricRecallCount         = "recall_count"
	MetricInputTokens         = "input_tokens"
	MetricOutputTokens        = "output_tokens"
	MetricEstimatedCostGBP    = "estimated_cost_gbp"
	MetricReconnectCount      = "reconnect_count"
	MetricFailures            = "failures"
)
