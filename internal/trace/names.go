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

// Metric names per PROPOSAL §4.5. Values are float64 throughout the
// Tracer seam; counts are recorded as whole numbers.
const (
	MetricTaskDurationSeconds = "task_duration_seconds"
	MetricPollCount           = "poll_count"
	MetricSearchCount         = "search_count"
	MetricInputTokens         = "input_tokens"
	MetricOutputTokens        = "output_tokens"
	MetricEstimatedCostGBP    = "estimated_cost_gbp"
	MetricReconnectCount      = "reconnect_count"
	MetricFailures            = "failures"
)
