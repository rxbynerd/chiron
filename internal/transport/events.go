package transport

// Event kinds emitted by the run core over the Transport seam. The set
// mirrors the run lifecycle in proto/chiron/v1/chiron.proto so the v1
// NDJSON stream and the v2 control-plane stream describe the same
// machine-readable lifecycle.
const (
	// KindRunStarted opens a run: the query has been accepted and the
	// researcher is about to start.
	KindRunStarted = "run_started"

	// KindInteractionCreated carries the interaction ID — the resume
	// handle — emitted as soon as it is known so a crashed run can be
	// recovered with `chiron get <id>`.
	KindInteractionCreated = "interaction_created"

	// KindStatusChanged reports an interaction status transition
	// observed while awaiting completion.
	KindStatusChanged = "status_changed"

	// KindDelta carries an incremental update (a streamed thought
	// summary) while the research task runs.
	KindDelta = "delta"

	// KindRunCompleted closes a run with its terminal status.
	KindRunCompleted = "run_completed"

	// KindCostSummary reports the end-of-run cost signals: token usage,
	// grounding-tool counts, and the estimated cost.
	KindCostSummary = "cost_summary"
)
