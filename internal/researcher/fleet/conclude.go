package fleet

import (
	"context"

	"github.com/rxbynerd/chiron/internal/secret"
	"github.com/rxbynerd/chiron/internal/types"
)

// fleetOutcome is the part of a fleet run's Interaction the lead decides
// once the pool has returned.
type fleetOutcome struct {
	// Body is the report body in Markdown, sanitised: the synthesis, the
	// stitched findings when synthesis failed, or empty when no worker
	// produced text.
	Body string
	// Citations are the citation pass's subset of the worker citations, or
	// every worker citation when a lead pass degraded.
	Citations []types.Citation
	Status    types.Status
	// Detail says why the run is not Completed, scrubbed and bounded to
	// maxDetailBytes.
	Detail string
	// Usage is the run's one Usage, from runUsage.
	Usage types.Usage
}

// applyTo sets in's outcome fields: the body as its one text output, the
// citations, status, detail and usage. The id, agent, query, tools and
// timestamps are the caller's.
func (o fleetOutcome) applyTo(in *types.Interaction) {
	in.Outputs = nil
	if o.Body != "" {
		in.Outputs = []types.Output{{Type: types.OutputText, Text: o.Body}}
	}
	in.Citations = o.Citations
	in.Status = o.Status
	in.StatusDetail = o.Detail
	in.Usage = o.Usage
}

// conclude runs the lead's closing passes over a finished pool and composes
// the run's outcome: synthesis, then the citation pass when synthesis wrote
// a body, and the run's one Usage from the decomposition, the workers and
// both passes.
func (l *lead) conclude(ctx context.Context, query string, plan leadPlan, planUsage types.Usage, pooled poolResult) fleetOutcome {
	syn := l.synthesise(ctx, query, plan, pooled)
	out := fleetOutcome{Body: syn.Body}
	var cited citeResult
	switch syn.Status {
	case types.StatusCompleted, types.StatusIncomplete:
		cited = l.cite(ctx, syn.Body, syn.Set.Findings)
		out.Citations = cited.Citations
	default:
		out.Citations = workerCitations(syn.Set.Findings)
	}
	out.Status, out.Detail = outcomeStatus(syn, cited)
	out.Usage = l.runUsage(pooled.Usage, planUsage, syn.Usage, cited.Usage)
	return out
}

// outcomeStatus applies the fleet's status rules. A run is Failed when no
// worker produced a finding with text; Completed when every brief yielded a
// completed finding and synthesis and the citation pass both completed;
// otherwise Incomplete. The detail names each lead pass that degraded, then
// each brief that yielded no finding or a partial one.
func outcomeStatus(syn synthesisResult, cited citeResult) (types.Status, string) {
	parts := []string{syn.Detail, cited.Detail}
	for _, g := range syn.Set.Gaps {
		parts = append(parts, g.BriefID+" produced no finding: "+g.Reason)
	}
	partial := false
	for _, cf := range syn.Set.Findings {
		if cf.Finding.Status == types.StatusCompleted {
			continue
		}
		partial = true
		parts = append(parts, gapReason(withDetail(cf.BriefID+" is partial: the worker ended "+echoStatus(cf.Finding.Status), cf.Finding.Detail)))
	}
	var detail string
	for _, p := range parts {
		if p != "" {
			detail = joinDetail(detail, p)
		}
	}
	detail = boundDetail(secret.Scrub(detail))

	switch {
	case len(syn.Set.Findings) == 0:
		return types.StatusFailed, detail
	case syn.Status == types.StatusCompleted && cited.Status == types.StatusCompleted && len(syn.Set.Gaps) == 0 && !partial:
		return types.StatusCompleted, ""
	default:
		return types.StatusIncomplete, detail
	}
}

// runUsage is a fleet run's one Usage and the only usage the fleet reports:
// the workers' summed usage plus the lead's own calls. A lead call's usage
// carries tokens; their sum is priced here with the lead's prices, so a
// caller's estimate on a lead call is never counted twice. The run core
// records the run-level metrics from this Usage once; the fleet's spans
// carry per-call spend as attributes only.
func (l *lead) runUsage(workers types.Usage, leadCalls ...types.Usage) types.Usage {
	var calls types.Usage
	for _, u := range leadCalls {
		calls = addUsage(calls, u)
	}
	calls.EstimatedCostGBP = costGBP(calls.InputTokens, calls.OutputTokens, l.deps.InputGBPPerMTok, l.deps.OutputGBPPerMTok)
	return addUsage(workers, calls)
}
