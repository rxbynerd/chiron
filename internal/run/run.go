// Package run is Chiron's pure-function research core: the deterministic
// loop start → await → retrieve → format → emit (PROPOSAL §4.1–§4.2). It
// depends only on the injected seam interfaces — Researcher, Formatter,
// ReportSink, Transport, Tracer — and on the domain types; no Gemini or
// wire shapes, no globals, no environment access. Timeouts arrive on the
// context. Swapping the v1 Gemini researcher for the v2 fleet
// orchestrator does not touch this loop.
package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rxbynerd/chiron/internal/formatter"
	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/sink"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/transport"
	"github.com/rxbynerd/chiron/internal/types"
)

// Deps are the injected seams. Every field is required: the core calls
// each one unconditionally, binding no-op implementations (trace.Noop,
// an empty sink.Multi) where a run wants none.
type Deps struct {
	Researcher researcher.Researcher
	Formatter  formatter.Formatter
	Sink       sink.ReportSink
	Transport  transport.Transport
	Tracer     trace.Tracer
}

func (d Deps) validate() error {
	switch {
	case d.Researcher == nil:
		return errors.New("run: nil Researcher")
	case d.Formatter == nil:
		return errors.New("run: nil Formatter")
	case d.Sink == nil:
		return errors.New("run: nil Sink")
	case d.Transport == nil:
		return errors.New("run: nil Transport")
	case d.Tracer == nil:
		return errors.New("run: nil Tracer")
	}
	return nil
}

// Params declares one research run to the core. Agent is informational
// — the tier name carried on events and spans; the researcher already
// knows its own binding.
type Params struct {
	Query string
	Agent string
}

// Transport event payloads. Shapes mirror the RunEvent payloads in
// proto/chiron/v1/chiron.proto so the v1 NDJSON stream and the v2
// control-plane stream describe the same lifecycle.
type (
	runStartedPayload struct {
		Query string `json:"query"`
		Agent string `json:"agent,omitempty"`
	}
	interactionCreatedPayload struct {
		InteractionID string `json:"interaction_id"`
	}
	statusChangedPayload struct {
		InteractionID string       `json:"interaction_id"`
		Status        types.Status `json:"status"`
		Detail        string       `json:"detail,omitempty"`
	}
	runCompletedPayload struct {
		InteractionID string       `json:"interaction_id"`
		Status        types.Status `json:"status"`
		DurationMS    int64        `json:"duration_ms"`
	}
	costSummaryPayload struct {
		InteractionID string      `json:"interaction_id"`
		Usage         types.Usage `json:"usage"`
	}
)

// Run executes one research run: start the task, emit the interaction
// ID as the resume handle the moment it is known, await completion,
// retrieve and format the result, and write it to the sink. It returns
// the RunResult for every terminal interaction status — failed,
// cancelled and budget_exceeded runs still produce a (placeholder)
// report and a cost summary; mapping failure statuses to exit codes is
// the caller's concern. It returns an error only when the run itself
// could not conclude: bad deps, researcher errors, formatter or sink
// failures, or context timeout/cancellation.
//
// Transport emission is best effort: once money is being spent, a
// broken event stream must not abort the run — the report is the
// artefact, events are observability. Emit failures are recorded on
// the root span instead.
func Run(ctx context.Context, deps Deps, params Params) (result *types.RunResult, err error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}
	if params.Query == "" {
		return nil, errors.New("run: query must not be empty")
	}

	started := time.Now()
	ctx, root := deps.Tracer.StartSpan(ctx, trace.SpanResearch)
	defer func() {
		if err != nil {
			deps.Tracer.Metric(ctx, trace.MetricFailures, 1)
		}
		root.End(err)
	}()
	root.SetAttr("query", params.Query)
	if params.Agent != "" {
		root.SetAttr("agent", params.Agent)
	}

	emit(ctx, deps, root, transport.KindRunStarted, runStartedPayload{Query: params.Query, Agent: params.Agent})

	// Start: the interaction ID is the resume handle — emitted before
	// anything else can fail, so a crashed run recovers server-side
	// state with `chiron get <id>`.
	startCtx, startSpan := deps.Tracer.StartSpan(ctx, trace.SpanStart)
	id, err := deps.Researcher.Start(startCtx, researcher.Task{Query: params.Query})
	if err == nil && id == "" {
		err = errors.New("run: researcher returned an empty interaction id")
	}
	startSpan.SetAttr("interaction_id", id)
	startSpan.End(err)
	if err != nil {
		return nil, fmt.Errorf("run: starting research: %w", err)
	}
	root.SetAttr("interaction_id", id)
	emit(ctx, deps, root, transport.KindInteractionCreated, interactionCreatedPayload{InteractionID: id})

	// Await + retrieve: block until the task is terminal, then fetch
	// its final state. Both live under the await span — retrieval is
	// the await's conclusion, not a separate phase (trace vocabulary is
	// fixed in internal/trace/names.go).
	awaitCtx, awaitSpan := deps.Tracer.StartSpan(ctx, trace.SpanAwait)
	var in *types.Interaction
	if err = deps.Researcher.Await(awaitCtx, id); err == nil {
		in, err = deps.Researcher.Result(awaitCtx, id)
		if err == nil && in == nil {
			err = errors.New("run: researcher returned no interaction")
		}
	}
	if in != nil {
		awaitSpan.SetAttr("status", string(in.Status))
	}
	awaitSpan.End(err)
	if err != nil {
		return nil, fmt.Errorf("run: awaiting interaction %s: %w", id, err)
	}
	emit(ctx, deps, root, transport.KindStatusChanged, statusChangedPayload{
		InteractionID: id, Status: in.Status, Detail: in.StatusDetail,
	})

	// Format: failure variants still render a complete placeholder
	// document (the formatter's contract), so `chiron research` never
	// concludes with nothing.
	formatCtx, formatSpan := deps.Tracer.StartSpan(ctx, trace.SpanFormat)
	report, err := deps.Formatter.Format(formatCtx, in)
	formatSpan.End(err)
	if err != nil {
		return nil, fmt.Errorf("run: formatting interaction %s: %w", id, err)
	}

	result = &types.RunResult{
		InteractionID: id,
		Status:        in.Status,
		Report:        report,
		Usage:         in.Usage,
		Duration:      time.Since(started),
	}

	// Emit: the report must land; a sink failure fails the run (the
	// interaction id already emitted above still allows recovery).
	emitCtx, emitSpan := deps.Tracer.StartSpan(ctx, trace.SpanEmit)
	err = deps.Sink.Write(emitCtx, result)
	emitSpan.End(err)
	if err != nil {
		return nil, fmt.Errorf("run: writing report for interaction %s: %w", id, err)
	}

	emit(ctx, deps, root, transport.KindRunCompleted, runCompletedPayload{
		InteractionID: id, Status: in.Status, DurationMS: result.Duration.Milliseconds(),
	})
	emit(ctx, deps, root, transport.KindCostSummary, costSummaryPayload{InteractionID: id, Usage: in.Usage})

	recordMetrics(ctx, deps.Tracer, result)
	return result, nil
}

// emit sends one event, best effort: a failure is recorded on the root
// span rather than aborting a paid run mid-flight.
func emit(ctx context.Context, deps Deps, root trace.Span, kind string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		root.SetAttr("transport_error."+kind, err.Error())
		return
	}
	if err := deps.Transport.Emit(ctx, transport.Event{Kind: kind, Payload: data}); err != nil {
		root.SetAttr("transport_error."+kind, err.Error())
	}
}

// recordMetrics reports the per-run measurements (PROPOSAL §4.5) under
// the fixed names in internal/trace/names.go. A terminal failure
// variant counts as a failure even though the run itself concluded.
func recordMetrics(ctx context.Context, tracer trace.Tracer, result *types.RunResult) {
	tracer.Metric(ctx, trace.MetricTaskDurationSeconds, result.Duration.Seconds())
	tracer.Metric(ctx, trace.MetricPollCount, float64(result.Usage.PollCount))
	tracer.Metric(ctx, trace.MetricSearchCount, float64(result.Usage.SearchCount))
	tracer.Metric(ctx, trace.MetricInputTokens, float64(result.Usage.InputTokens))
	tracer.Metric(ctx, trace.MetricOutputTokens, float64(result.Usage.OutputTokens))
	tracer.Metric(ctx, trace.MetricEstimatedCostGBP, result.Usage.EstimatedCostGBP)
	tracer.Metric(ctx, trace.MetricReconnectCount, float64(result.Usage.ReconnectCount))
	if result.Status != types.StatusCompleted {
		tracer.Metric(ctx, trace.MetricFailures, 1)
	}
}
