package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/secret"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/types"
)

// synthesiseMaxTokens caps the synthesis completion. A report of several
// sections fits well within it, and it stays under the output ceiling of
// models that reject a larger cap.
const synthesiseMaxTokens = 16384

// maxSynthesisFindingBytes bounds each finding's text in the synthesis and
// citation prompts, measured after defang, so five findings stay near 60 KiB.
const maxSynthesisFindingBytes = 12 << 10

// maxFindingSourcesBytes bounds each finding's rendered source list in a lead
// prompt.
const maxFindingSourcesBytes = 4 << 10

// maxGapDetailBytes bounds one brief's reason for yielding no finding, or a
// partial one, in a lead prompt and in the run's detail.
const maxGapDetailBytes = 512

// maxStitchedHeadingRunes bounds a heading in the stitched fallback body.
const maxStitchedHeadingRunes = 200

// findingsReadTimeout bounds reading every brief's finding back, all reads
// together, so a slow store holds synthesis for at most this long however
// many briefs there are. The reads are detached from the run's
// cancellation, because a finding is paid for whether or not the run is
// still wanted, and the fallback body needs it.
const findingsReadTimeout = 10 * time.Second

// collectedFinding is a finding the lead read back for synthesis: it has
// text, did not fail, and its stored identity matches the brief that
// produced it.
type collectedFinding struct {
	BriefID   string
	Objective string
	Finding   Finding
}

// findingGap is a planned brief that yielded no finding with text.
type findingGap struct {
	BriefID   string
	Objective string
	// Reason is one line, scrubbed, defanged and bounded to
	// maxGapDetailBytes.
	Reason string
}

// findingSet is a pool's results read back by reference, in brief order.
type findingSet struct {
	Findings []collectedFinding
	Gaps     []findingGap
}

// synthesisResult is the synthesise step's outcome over Set, the findings
// it was given. Status is Completed; Incomplete for a body cut off at the
// completion cap; or Failed when there is no synthesised body, in which case
// Body is the stitched fallback, or empty when no finding was collected.
// Truncated counts the finding texts cut to maxSynthesisFindingBytes.
type synthesisResult struct {
	Set       findingSet
	Body      string
	Status    types.Status
	Detail    string
	Usage     types.Usage
	Truncated int
}

// synthesise makes one plain-text model call, under a synthesise span, that
// writes the report body from the findings collectFindings read back. The
// call is never retried. When it fails, or its reply is empty or filtered,
// the body is the collected findings stitched together, so no paid finding
// is lost; with no finding collected, no call is made.
func (l *lead) synthesise(ctx context.Context, query string, set findingSet) synthesisResult {
	ctx, span := l.deps.Tracer.StartSpan(ctx, trace.SpanSynthesise)
	res := l.synthesiseInSpan(ctx, query, set)

	span.SetAttr("findings_included", len(res.Set.Findings))
	span.SetAttr("findings_truncated", res.Truncated)
	span.SetAttr("findings_gapped", len(res.Set.Gaps))
	span.SetAttr("input_tokens", res.Usage.InputTokens)
	span.SetAttr("output_tokens", res.Usage.OutputTokens)
	span.SetAttr("status", string(res.Status))
	if res.Detail != "" {
		span.SetAttr("detail", res.Detail)
	}
	var spanErr error
	if res.Status == types.StatusFailed {
		spanErr = errors.New(res.Detail)
	}
	span.End(spanErr)
	return res
}

func (l *lead) synthesiseInSpan(ctx context.Context, query string, set findingSet) synthesisResult {
	res := synthesisResult{Set: set}
	if len(res.Set.Findings) == 0 {
		res.Status = types.StatusFailed
		res.Detail = "no worker produced a finding with text"
		return res
	}
	fallback := func(reason string) synthesisResult {
		res.Body = stitchFindings(res.Set.Findings)
		res.Status = types.StatusFailed
		res.Detail = fallbackDetail(reason, "; the report is the worker findings, stitched unedited")
		return res
	}

	format := defaultReportFormat
	if l.deps.ReportTemplate != nil {
		rendered, err := l.deps.ReportTemplate.Render(query)
		if err != nil {
			return fallback(err.Error())
		}
		format = rendered
	}
	findings, truncated := renderFindings(res.Set.Findings)
	res.Truncated = truncated

	// One attempt only: the model client never retries a POST and the lead
	// adds no retry either.
	resp, err := l.deps.Model.Generate(ctx, model.Request{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: synthesisSystemPrompt(format)},
			{Role: model.RoleUser, Content: synthesisUserMessage(query, findings, res.Set.Gaps)},
		},
		MaxTokens: synthesiseMaxTokens,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fallback("the run ended during the synthesis call: " + ctxErr.Error())
		}
		return fallback("the synthesis call failed: " + err.Error())
	}
	res.Usage = types.Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}

	body := strings.TrimSpace(sanitiseAnswer(resp.Content))
	switch {
	case resp.FinishReason == "content_filter":
		return fallback("the synthesis reply was refused by the model's content filter")
	case body == "":
		return fallback("the synthesis reply was empty")
	}
	res.Body = body
	res.Status = types.StatusCompleted
	if resp.FinishReason == "length" {
		res.Status = types.StatusIncomplete
		res.Detail = fmt.Sprintf("the synthesis was cut off at the %d-token completion cap", synthesiseMaxTokens)
	}
	return res
}

// collectFindings reads every brief's finding back by reference, never from
// an in-process copy, under one findingsReadTimeout deadline shared by all
// the reads. A brief whose finding reads back from the run's session, names
// the worker and brief the pool reported for it, did not fail and has text
// is collected; every other brief is a gap with its reason.
func (l *lead) collectFindings(ctx context.Context, plan leadPlan, pooled poolResult) findingSet {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.deps.findingsReadDeadline())
	defer cancel()
	objectives := make(map[string]string, len(plan.Briefs))
	for _, pb := range plan.Briefs {
		objectives[pb.ID] = pb.Brief.Objective
	}
	var set findingSet
	for _, r := range pooled.Briefs {
		f, reason := l.readBack(ctx, r)
		if reason != "" {
			set.Gaps = append(set.Gaps, findingGap{BriefID: r.BriefID, Objective: objectives[r.BriefID], Reason: gapReason(reason)})
			continue
		}
		set.Findings = append(set.Findings, collectedFinding{BriefID: r.BriefID, Objective: objectives[r.BriefID], Finding: f})
	}
	return set
}

// readBack returns r's stored finding, or the reason it yields none. No read
// starts once ctx has ended.
func (l *lead) readBack(ctx context.Context, r briefResult) (Finding, string) {
	switch {
	case r.Disposition != dispositionRan:
		return Finding{}, orDefault(r.Detail, "no worker ran the brief")
	case r.Ref == (memory.Reference{}):
		return Finding{}, orDefault(r.Detail, "the finding was not stored")
	case r.Ref.Namespace != l.deps.Namespace:
		return Finding{}, "the finding's reference is outside the run's session"
	case ctx.Err() != nil:
		return Finding{}, "the read-back deadline passed before the finding was read"
	}
	sf, err := l.readFindingSafely(ctx, r.Ref)
	if err != nil {
		return Finding{}, "the finding could not be read back: " + err.Error()
	}
	if sf.WorkerID != r.WorkerID || sf.BriefID != r.BriefID {
		return Finding{}, "the stored finding names another worker or brief"
	}
	f := sf.Finding
	switch {
	case f.Status == types.StatusFailed:
		return Finding{}, withDetail("the worker failed", f.Detail)
	case strings.TrimSpace(f.Text) == "":
		return Finding{}, withDetail(fmt.Sprintf("the worker ended %s with no text", echoStatus(f.Status)), f.Detail)
	}
	return f, ""
}

// errStoreReadPanicked stands for a store read that panicked. The recovered
// value is never included, for the reason panicRecoveryDetail gives.
var errStoreReadPanicked = errors.New("the store panicked")

// readFindingSafely runs readFinding, recovering a panic in the store into
// errStoreReadPanicked, so one brief's read cannot lose the other briefs'
// findings.
func (l *lead) readFindingSafely(ctx context.Context, ref memory.Reference) (sf storedFinding, err error) {
	defer func() {
		if recover() != nil {
			sf, err = storedFinding{}, errStoreReadPanicked
		}
	}()
	return readFinding(ctx, l.deps.Store, ref)
}

// fallbackDetail is a degraded pass's detail: reason, scrubbed, then note,
// which says what the pass fell back to. The whole is bounded to
// maxDetailBytes by cutting the reason, so the note always survives.
func fallbackDetail(reason, note string) string {
	return boundBytes(secret.Scrub(reason), maxDetailBytes-len(note)) + note
}

// gapReason makes a reason safe for a lead prompt and the run's detail: one
// line, scrubbed, defanged, then bounded, in that order, so the bound cannot
// split a credential into fragments too short for the scrubber.
func gapReason(s string) string {
	return boundBytes(defang(secret.Scrub(oneLine(s))), maxGapDetailBytes)
}

// echoStatus renders a stored status, which a store could fill with any
// terminal-looking text, for a prompt or detail.
func echoStatus(s types.Status) string {
	return boundRunes(oneLine(secret.Scrub(string(s))), maxFindingEchoRunes)
}

// withDetail appends detail to reason when there is one.
func withDetail(reason, detail string) string {
	if strings.TrimSpace(detail) == "" {
		return reason
	}
	return reason + ": " + detail
}

// orDefault returns s, or def when s is blank.
func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// stitchedNote opens the stitched fallback body.
const stitchedNote = "_Synthesis did not complete, so each research worker's finding follows unedited._"

// stitchFindings is the deterministic body used when synthesis fails: a
// note, then each collected finding in brief order under a heading naming
// its objective, the whole sanitised as a synthesised body is.
func stitchFindings(findings []collectedFinding) string {
	var b strings.Builder
	b.WriteString(stitchedNote)
	for _, cf := range findings {
		heading := boundRunes(oneLine(cf.Objective), maxStitchedHeadingRunes)
		if heading == "" {
			heading = cf.BriefID
		}
		fmt.Fprintf(&b, "\n\n## %s\n\n%s", heading, strings.TrimSpace(cf.Finding.Text))
	}
	return strings.TrimSpace(sanitiseAnswer(b.String()))
}
