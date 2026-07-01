package fleet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/fetch"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/secret"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/types"
)

// This file implements the bounded, in-process research worker loop
// (docs/V2-RESEARCH-AGENT §5): search -> read -> reason -> synthesise over the
// two read-only web tools, driven by a standard frontier model asked for one
// structured action per turn. It is factored so Wave 4's lead can reuse it
// per-brief: RunWorker takes a Brief (objective + output format + source
// guidance + boundaries) and shared WorkerDeps, and returns a Finding
// (prose + citations + usage + status). The single-query --agent worker path
// (Worker, in worker_researcher.go) is one caller; the lead will be another,
// dispatching one RunWorker per decomposed brief under its own fan-out caps.

// Brief is one unit of work for a worker: the objective to research and the
// shape of the answer expected. A lone --agent worker run builds a Brief from
// the user's query; Wave 4's lead builds one Brief per decomposed subtask.
// Blank fields fall back to instructive defaults (worker_prompt.go), so a
// minimally-populated Brief still produces a coherent prompt.
type Brief struct {
	// Objective is the research question or subtask the worker must answer.
	Objective string
	// OutputFormat describes the shape the finding should take (e.g. a short
	// Markdown synthesis, a comparison table). Blank selects a default.
	OutputFormat string
	// SourceGuidance steers which sources to prefer. Blank selects a default.
	SourceGuidance string
	// Boundaries constrains scope (what to include or exclude). Blank selects a
	// default.
	Boundaries string
}

// Finding is the outcome of one worker run: the synthesised prose, the sources
// it cited, the usage it accumulated, and the status describing how it ended.
// It is the unit the --agent worker path maps to a types.Interaction and the
// unit Wave 4's lead will store by reference and synthesise over.
type Finding struct {
	// Text is the worker's synthesised answer — the report-body candidate.
	Text string
	// Citations are the deduplicated sources the worker relied on.
	Citations []types.Citation
	// Usage accumulates the token and search counters across every turn, plus
	// a best-effort estimated cost. Cost is 0 when no rate is configured — the
	// token and search counts are the primary spend signal.
	Usage types.Usage
	// Status is the terminal outcome: Completed when the model delivered a
	// final answer within the caps; Incomplete when a cap stopped the loop;
	// Failed when a model/search/fetch error ended it. Partial citations
	// gathered before a stop are preserved on all outcomes.
	Status types.Status
	// Detail is a human-readable reason accompanying an Incomplete or Failed
	// outcome (the cap that fired, or the scrubbed error), empty on success.
	Detail string
}

// Caps bound one worker run deterministically (docs/V2-RESEARCH-AGENT §1,
// "structural caps"). Exceeding any of them ends the loop with
// StatusIncomplete rather than an error: a bounded, partial finding is a
// legitimate outcome, not a fault.
type Caps struct {
	// MaxTurns caps the number of model turns. It must be positive (config
	// validation guarantees it); it is the primary runaway guard.
	MaxTurns int
	// MaxTokens caps the accumulated model tokens across the run. Zero means
	// uncapped on tokens — the turn and time caps still bound the loop.
	MaxTokens int
	// CeilingGBP caps the accumulated estimated model spend. Zero means
	// uncapped on cost (the usual case in CI, where no price table exists).
	CeilingGBP float64
	// Timeout is the per-worker wall-clock limit. A positive value bounds the
	// loop with a context deadline; zero leaves the caller's context deadline
	// as the only wall-clock bound.
	Timeout time.Duration
}

// WorkerDeps are the shared collaborators a worker loop uses: the three landed
// clients, a tracer for best-effort observability, and the caps. The lead and
// every worker share one set of clients; only the Brief and Caps differ per
// run. Model and Search are required; Fetch is required for the loop to honour
// a fetch action; Tracer may be nil (best-effort tracing is skipped).
type WorkerDeps struct {
	Model  *model.Client
	Search *search.Client
	Fetch  *fetch.Client
	Tracer trace.Tracer
	Caps   Caps
}

// fetchedPage is the loop's internal view of a fetched document — the subset
// of fetch.Page the transcript renders. Decoupling it keeps worker_prompt.go
// free of a direct dependency on the fetch client's concrete Page type in its
// message-builder signature.
type fetchedPage struct {
	URL         string
	ContentType string
	Content     []byte
	Truncated   bool
}

// RunWorker runs the bounded worker loop for one brief and returns a Finding.
// It never returns an error: every outcome — success, a cap stop, or a tool
// failure — is expressed as a Finding with a status and (for non-success) a
// scrubbed detail, so a caller (the --agent worker adapter, or Wave 4's lead)
// gets a uniform result to map or store. Any partial citations gathered before
// a stop are preserved on the returned Finding.
//
// The loop dispatches ONLY the three known actions (worker_action.go). An
// unknown or side-effecting action is refused at parse time and ends the run
// as Failed with no side effect — there is no execution path for it.
func RunWorker(ctx context.Context, deps WorkerDeps, brief Brief) Finding {
	// Bound the whole run by the per-worker timeout, yielding to a tighter
	// caller deadline. A zero Timeout leaves the caller's context as-is.
	if deps.Caps.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deps.Caps.Timeout)
		defer cancel()
	}

	var span trace.Span
	if deps.Tracer != nil {
		ctx, span = deps.Tracer.StartSpan(ctx, trace.SpanWorker)
		span.SetAttr("objective", brief.Objective)
	}

	w := &workerRun{deps: deps, brief: brief}
	finding := w.loop(ctx)

	if span != nil {
		span.SetAttr("status", string(finding.Status))
		span.SetAttr("turns", w.turns)
		if finding.Detail != "" {
			span.SetAttr("detail", finding.Detail)
		}
		// A cap stop or tool failure is a worker-level outcome, not an error to
		// record on the span: the loop concluded and produced a finding. Only a
		// context error (timeout/cancellation surfacing through a tool) is
		// recorded as the span's error, if any.
		span.End(nil)
		emitWorkerMetrics(ctx, deps.Tracer, finding.Usage)
	}
	return finding
}

// workerRun holds the mutable state of one loop: the growing transcript, the
// accumulated usage, the citations gathered so far, and the set of URLs the
// model is permitted to fetch (only URLs it has actually seen in a search
// result). It exists so the loop's steps can share state without threading a
// dozen return values.
type workerRun struct {
	deps  WorkerDeps
	brief Brief

	transcript []model.Message
	usage      types.Usage
	citations  []types.Citation
	// seenURLs is the allow-list for fetch: a URL becomes fetchable only after
	// it appears in a search result. This keeps fetch scoped to search-derived
	// URLs (docs/V2-RESEARCH-AGENT §5, step 4) rather than arbitrary
	// model-chosen destinations, a defence-in-depth complement to the fetch
	// client's own SSRF guard.
	seenURLs map[string]bool
	turns    int
}

// loop runs the turn loop to a terminal Finding. Each iteration checks the
// caps, asks the model for one action, and dispatches it. The caps are checked
// at the top of each turn so a run that has already spent its budget stops
// before paying for another model turn.
func (w *workerRun) loop(ctx context.Context) Finding {
	w.transcript = initialTranscript(w.brief)
	w.seenURLs = map[string]bool{}

	for {
		// A cancelled or timed-out context ends the loop with a bounded
		// outcome, preserving partial citations. A timeout is Incomplete (the
		// wall-clock cap fired); an explicit caller cancellation is also
		// surfaced as Incomplete — the run was stopped, not broken.
		if err := ctx.Err(); err != nil {
			return w.incomplete("worker context ended: " + w.scrub(err.Error()))
		}
		// Structural caps: turn count, accumulated tokens, accumulated cost.
		if w.turns >= w.deps.Caps.MaxTurns {
			return w.incomplete(fmt.Sprintf("reached the %d-turn cap before a final answer", w.deps.Caps.MaxTurns))
		}
		if w.deps.Caps.MaxTokens > 0 && totalTokens(w.usage) >= w.deps.Caps.MaxTokens {
			return w.incomplete(fmt.Sprintf("reached the %d-token cap before a final answer", w.deps.Caps.MaxTokens))
		}
		if w.deps.Caps.CeilingGBP > 0 && w.usage.EstimatedCostGBP >= w.deps.Caps.CeilingGBP {
			return w.incomplete(fmt.Sprintf("reached the £%.2f cost ceiling before a final answer", w.deps.Caps.CeilingGBP))
		}

		done, finding := w.turn(ctx)
		if done {
			return finding
		}
	}
}

// turn runs one iteration: one model call, then dispatch of the returned
// action. It returns done=true with the terminal Finding when the loop should
// stop (a final answer, or a hard failure); done=false to continue.
func (w *workerRun) turn(ctx context.Context) (bool, Finding) {
	w.turns++

	resp, err := w.deps.Model.Generate(ctx, model.Request{
		Messages:   w.transcript,
		MaxTokens:  w.remainingTokenBudget(),
		JSONSchema: actionSchema,
		SchemaName: actionSchemaName,
	})
	if err != nil {
		// A model turn may already be billed; the worker adds NO retry (the
		// model client is single-attempt by contract). The error ends the run
		// as Failed, keeping any citations already gathered.
		return true, w.failed("model turn failed: " + w.scrub(err.Error()))
	}
	w.accumulateModelUsage(resp.Usage)
	w.transcript = append(w.transcript, assistantEcho(resp.Content))

	act, err := parseAction(resp.Content)
	if err != nil {
		// An unparseable or unknown/side-effecting action is a hard failure
		// with NO side effect — this is the closed-vocabulary guarantee. The
		// run ends Failed; partial citations are preserved.
		return true, w.failed("invalid model action: " + w.scrub(err.Error()))
	}

	switch act.Kind {
	case actionSearch:
		return w.doSearch(ctx, act)
	case actionFetch:
		return w.doFetch(ctx, act)
	case actionFinal:
		return true, w.finalise(act)
	default:
		// Unreachable: parseAction rejects every other kind. Kept as a
		// belt-and-braces guard so no future action added to the enum can slip
		// through to an undefined dispatch.
		return true, w.failed(fmt.Sprintf("unhandled action %q", act.Kind))
	}
}

// doSearch runs a search action, appends the results to the transcript, and
// records every returned URL as fetchable. A search error ends the run as
// Failed. Returns done=true only on failure.
func (w *workerRun) doSearch(ctx context.Context, act action) (bool, Finding) {
	results, err := w.deps.Search.Search(ctx, act.Query)
	if err != nil {
		return true, w.failed("search failed: " + w.scrub(err.Error()))
	}
	w.usage.SearchCount++
	for _, r := range results {
		if r.URL != "" {
			w.seenURLs[r.URL] = true
		}
	}
	w.transcript = append(w.transcript, searchResultsMessage(act.Query, results))
	return false, Finding{}
}

// doFetch runs a fetch action for a URL the model has already seen in a search
// result, appends the bounded page text to the transcript, and continues. A
// fetch of a URL that never appeared in a search result is a recoverable
// error: it is fed back to the model as guidance (so it can pick a real URL)
// rather than failing the run — no fetch is performed. A hard fetch error
// (transport/SSRF refusal) ends the run as Failed. Returns done=true only on a
// hard failure.
func (w *workerRun) doFetch(ctx context.Context, act action) (bool, Finding) {
	if !w.seenURLs[act.URL] {
		// The model asked to fetch a URL it was never shown. Do not fetch it;
		// steer the model back to a search-derived URL. This is defence in
		// depth over the fetch client's SSRF guard, and it keeps fetch scoped
		// to search results (§5 step 4).
		w.transcript = append(w.transcript, errorFeedbackMessage(
			fmt.Sprintf("the URL %q did not appear in any search result, so it cannot be fetched.", act.URL)))
		return false, Finding{}
	}
	if w.deps.Fetch == nil {
		return true, w.failed("fetch requested but no fetch client is configured")
	}

	page, err := w.deps.Fetch.Fetch(ctx, act.URL)
	if err != nil {
		return true, w.failed("fetch failed: " + w.scrub(err.Error()))
	}
	w.transcript = append(w.transcript, fetchedPageMessage(fetchedPage{
		URL:         page.URL,
		ContentType: page.ContentType,
		Content:     page.Content,
		Truncated:   page.Truncated,
	}))
	return false, Finding{}
}

// finalise builds the successful Finding from a final action: the answer text
// and the model's citations, merged with any citations already gathered and
// deduplicated. A final answer is StatusCompleted even if the caps were close
// — the model chose to stop.
func (w *workerRun) finalise(act action) Finding {
	for _, c := range act.Citations {
		w.addCitation(c.URL, c.Title)
	}
	return Finding{
		Text:      act.Answer,
		Citations: w.citations,
		Usage:     w.usage,
		Status:    types.StatusCompleted,
	}
}

// incomplete builds a bounded-stop Finding: a cap or the context ended the
// loop before a final answer. Partial citations and accumulated usage are
// preserved so a stopped run still yields whatever it gathered.
func (w *workerRun) incomplete(detail string) Finding {
	return Finding{
		Text:      "",
		Citations: w.citations,
		Usage:     w.usage,
		Status:    types.StatusIncomplete,
		Detail:    detail,
	}
}

// failed builds a failed Finding: a model/search/fetch error, or an invalid
// action, ended the loop. Partial citations and accumulated usage are
// preserved — a failure part-way through must not discard sources already
// found (docs/V2-RESEARCH-AGENT §8).
func (w *workerRun) failed(detail string) Finding {
	return Finding{
		Text:      "",
		Citations: w.citations,
		Usage:     w.usage,
		Status:    types.StatusFailed,
		Detail:    detail,
	}
}

// addCitation appends a citation, deduplicated by URI. A citation with an
// empty URI is dropped (nothing to verify or cite). The first title seen for a
// URI wins.
func (w *workerRun) addCitation(uri, title string) {
	if uri == "" {
		return
	}
	for _, c := range w.citations {
		if c.URI == uri {
			return
		}
	}
	w.citations = append(w.citations, types.Citation{URI: uri, Title: title})
}

// accumulateModelUsage folds one model turn's usage into the running totals.
// Input and output tokens accumulate; TotalTokens is derived on read so the
// token cap and the mapped Usage stay consistent even if a provider omits its
// own total.
func (w *workerRun) accumulateModelUsage(u model.Usage) {
	w.usage.InputTokens += u.InputTokens
	w.usage.OutputTokens += u.OutputTokens
	// EstimatedCostGBP stays best-effort: there is no price table in CI, so it
	// remains 0 unless a future rate is wired in. Tokens and search count are
	// the primary spend signal (docs/DECISIONS.md).
}

// remainingTokenBudget caps a single model turn's completion length to what is
// left under the token cap, so no one turn can overshoot the accumulated cap
// by a full max-completion. Zero (unset) when no token cap is configured — the
// model client leaves max_tokens unset. Guards against a non-positive budget
// (the cap check at the top of the loop should already have stopped, but a
// belt-and-braces floor of 1 keeps the request valid).
func (w *workerRun) remainingTokenBudget() int {
	if w.deps.Caps.MaxTokens <= 0 {
		return 0
	}
	remaining := w.deps.Caps.MaxTokens - totalTokens(w.usage)
	if remaining < 1 {
		return 1
	}
	return remaining
}

// totalTokens is the accumulated token count the token cap is measured
// against: input plus output. types.Usage stores the two counters (no stored
// total) and is a stable domain type this package must not extend, so the sum
// lives here. Tool-use and thought tokens are not counted separately — the
// worker's model turns report only prompt/completion tokens.
func totalTokens(u types.Usage) int {
	return u.InputTokens + u.OutputTokens
}

// scrub redacts credential-shaped material from a detail string before it is
// stored on a Finding or a span. The model/search clients already scrub their
// own errors, but the worker scrubs again unconditionally — a detail composed
// here (or a future error source) must never carry a key into the Interaction,
// the report, or a trace (docs/V2-RESEARCH-AGENT §8, credential scrubbing).
func (w *workerRun) scrub(s string) string {
	return secret.Scrub(s)
}

// emitWorkerMetrics reports the worker's spend signals through the tracer,
// best effort: a failed metric emission must never fail the run. Uses the
// shared metric vocabulary so the search count and token totals roll up the
// same way the run core reports them.
func emitWorkerMetrics(ctx context.Context, tracer trace.Tracer, usage types.Usage) {
	if tracer == nil {
		return
	}
	tracer.Metric(ctx, trace.MetricSearchCount, float64(usage.SearchCount))
	tracer.Metric(ctx, trace.MetricInputTokens, float64(usage.InputTokens))
	tracer.Metric(ctx, trace.MetricOutputTokens, float64(usage.OutputTokens))
	tracer.Metric(ctx, trace.MetricEstimatedCostGBP, usage.EstimatedCostGBP)
}

// errWorkerNoModel is returned by NewWorker when the model client is nil — a
// worker cannot run without one. Search and fetch are checked at construction
// too, so a misconfigured worker fails at the composition root, not mid-run.
var errWorkerNoModel = errors.New("fleet: worker requires a model client")
