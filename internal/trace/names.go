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
// search -> read -> synthesise loop. These extend the fixed run-core
// vocabulary above for the fleet path without changing the run core's
// contract.
const (
	SpanWorker = "worker"
)

// Fleet span names, reserved for the lead orchestrator
// (docs/V2-RESEARCH-AGENT §6). Beneath the research root the lead opens one
// decompose span, then one delegate span per planned brief, with the
// worker's SpanWorker span as its child when a worker ran the brief, then
// synthesise and cite. SpanControlPlane wraps the control plane's
// scheduling of a run.
const (
	SpanDecompose    = "decompose"
	SpanDelegate     = "delegate"
	SpanSynthesise   = "synthesise"
	SpanCite         = "cite"
	SpanControlPlane = "control_plane"
)

// Fleet span attribute keys. A delegate span carries its brief, and the
// worker when one ran it; the decompose span carries how many briefs it
// produced.
const (
	AttrWorkerID   = "worker_id"
	AttrBriefID    = "brief_id"
	AttrBriefCount = "brief_count"
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
