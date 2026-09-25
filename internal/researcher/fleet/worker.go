package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/fetch"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/secret"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/types"
)

// Brief is one unit of work for a worker: the objective to research and the
// shape of the answer expected. Blank fields fall back to instructive defaults
// (worker_prompt.go), so a minimally-populated Brief still produces a coherent
// prompt. The JSON names match the lead's decomposition schema.
type Brief struct {
	// Objective is the research question or subtask the worker must answer.
	Objective string `json:"objective"`
	// OutputFormat describes the shape the finding should take (e.g. a short
	// Markdown synthesis, a comparison table). Blank selects a default.
	OutputFormat string `json:"output_format"`
	// SourceGuidance steers which sources to prefer. Blank selects a default.
	SourceGuidance string `json:"source_guidance"`
	// Boundaries constrains scope (what to include or exclude). Blank selects a
	// default.
	Boundaries string `json:"boundaries"`
}

// Finding is the outcome of one worker run: the synthesised prose, the sources
// it relied on, the usage it accumulated, and the status describing how it
// ended.
type Finding struct {
	// Text is the worker's synthesised answer, the report-body candidate.
	Text string
	// Citations are the deduplicated sources the worker fetched or cited. Only
	// URLs the worker actually saw in a search result or fetched, and locators
	// of knowledge store hits it recalled, are admitted; anything else the
	// model cites is dropped.
	Citations []types.Citation
	// Usage accumulates the token, search and recall counters across every
	// turn, plus the estimated cost when Caps carries a price; otherwise
	// EstimatedCostGBP stays 0 and the token counts are the spend signal.
	Usage types.Usage
	// Status is the terminal outcome: Completed when the model delivered a
	// final answer within the caps; Incomplete when a cap, the context, or a
	// truncated reply stopped the loop; Failed when a model error, an invalid
	// action, or repeated tool failures ended it. Citations gathered before a
	// stop are preserved on every outcome.
	Status types.Status
	// Detail is a human-readable reason accompanying an Incomplete or Failed
	// outcome, empty on success. It is scrubbed and bounded in length.
	Detail string
	// Turns is the number of model turns the loop started, counting a turn
	// whose model call failed.
	Turns int
}

// Progress is one best-effort report from a worker turn, made after the
// model's action has parsed and before it is dispatched. The token and cost
// fields are the run's accumulated usage including this turn's model call.
type Progress struct {
	// Turn is the 1-based turn number; MaxTurns is Caps.MaxTurns.
	Turn, MaxTurns int
	// Action is the parsed action kind: search, fetch, recall or final.
	Action string
	// Detail names the action's target without its content: the query for
	// search and recall, the URL reduced to scheme://host for fetch, empty
	// for final. It is one line of printable text, scrubbed and bounded to
	// maxProgressDetailRunes.
	Detail                    string
	InputTokens, OutputTokens int
	EstimatedCostGBP          float64
}

// Caps bound one worker run deterministically. Exceeding a cap ends the loop
// with StatusIncomplete rather than an error: a bounded, partial finding is a
// legitimate outcome, not a fault.
type Caps struct {
	// MaxTurns caps the number of model turns. It must be positive; it is the
	// primary runaway guard.
	MaxTurns int
	// MaxTokens caps the accumulated model tokens (input plus output) across
	// the run. Zero means uncapped on tokens.
	MaxTokens int
	// CeilingGBP caps the accumulated estimated model spend. It only has effect
	// when InputGBPPerMTok/OutputGBPPerMTok are set, because the estimate is
	// derived from them; config validation refuses a ceiling without a price.
	CeilingGBP float64
	// InputGBPPerMTok and OutputGBPPerMTok price a million prompt and
	// completion tokens. Zero leaves EstimatedCostGBP at 0.
	InputGBPPerMTok  float64
	OutputGBPPerMTok float64
	// Timeout is the per-worker wall-clock limit. A positive value bounds the
	// loop with a context deadline; zero leaves the caller's context deadline
	// as the only wall-clock bound.
	Timeout time.Duration
	// MaxPageBytes bounds the text of one fetched page after HTML is reduced
	// to text, before it enters the transcript. Zero selects
	// DefaultMaxPageBytes.
	MaxPageBytes int
	// RecallLimit is the number of hits one recall asks the knowledge store
	// for. Zero selects DefaultRecallLimit.
	RecallLimit int
	// MaxRecallBytes bounds one rendered recall result in the transcript.
	// Zero selects DefaultMaxPageBytes.
	MaxRecallBytes int
}

// DefaultMaxPageBytes is the page-text bound used when Caps.MaxPageBytes is
// zero. Every page in the transcript is re-sent on each later turn, so the
// bound is small relative to a model context window.
const DefaultMaxPageBytes = 64 << 10

// DefaultRecallLimit is the hits-per-recall used when Caps.RecallLimit is
// zero.
const DefaultRecallLimit = 5

// turnCompletionTokens caps a single turn's completion. An action is a short
// JSON object except for the final answer, which fits comfortably; providers
// reject a completion cap above the model's maximum, so the remaining token
// budget is never passed through verbatim.
const turnCompletionTokens = 8192

// maxConsecutiveToolFailures is how many search, fetch or recall failures in
// a row the loop feeds back to the model before treating the tools as
// unavailable and ending the run Failed. Each feedback costs one model turn,
// so this is a spend bound as much as a robustness one.
const maxConsecutiveToolFailures = 3

// progressTimeout bounds one Progress delivery, including any wait for the
// Worker's delivery gate. The loop never waits on a hook for longer.
const progressTimeout = 2 * time.Second

// maxProgressDetailRunes bounds Progress.Detail.
const maxProgressDetailRunes = 200

// maxDetailBytes bounds Finding.Detail; a provider error body can be large
// and the detail is copied into the report front matter and traces.
const maxDetailBytes = 2048

// WorkerDeps are the shared collaborators a worker loop uses: the three
// clients, the optional knowledge store halves, a tracer for best-effort
// observability, and the caps. Model and Search are required; Fetch is
// required for the loop to honour a fetch action; the rest may be nil.
type WorkerDeps struct {
	Model  *model.Client
	Search *search.Client
	Fetch  *fetch.Client
	// Knowledge, when non-nil, adds the recall action. nil omits recall from
	// the loop's schema, prompt and tool list.
	Knowledge memory.Recaller
	// Remember, when non-nil, saves a bounded summary of a Completed finding
	// after the loop returns (Worker only). It is independent of Knowledge.
	Remember memory.Rememberer
	// ReportTemplate, when non-nil, is rendered with the query to fill the
	// Brief's OutputFormat (Worker only; RunWorker callers set
	// Brief.OutputFormat themselves). nil selects the built-in format.
	ReportTemplate *ReportTemplate
	// Logger receives the save-back outcome. nil discards it; the composition
	// root binds a scrubbing handler on the command's stderr.
	Logger *slog.Logger
	// KnowledgeNamespace is passed to Recall and Remember: Alexandria's space;
	// informational for Billet.
	KnowledgeNamespace memory.Namespace
	Tracer             trace.Tracer
	Caps               Caps
	// Progress, when non-nil, receives one Progress per turn whose action
	// parsed. Delivery is best effort within progressTimeout: a panic is
	// recovered, a hook still running at the deadline is abandoned, and
	// nothing the hook does affects the Finding.
	Progress func(ctx context.Context, p Progress)

	// progressTimeoutOverride replaces progressTimeout when positive, so
	// tests can shorten it.
	progressTimeoutOverride time.Duration
}

// fetchedPage is the loop's view of a fetched document: the subset of
// fetch.Page the transcript renders.
type fetchedPage struct {
	URL         string
	ContentType string
	Content     []byte
	Truncated   bool
}

// RunWorker runs the bounded worker loop for one brief and returns a Finding.
// It never returns an error: every outcome (success, a cap stop, a context
// end, or a hard failure) is expressed as a Finding with a status and, for
// non-success, a scrubbed detail. Citations gathered before a stop are
// preserved.
//
// The loop dispatches only the known actions (worker_action.go). An unknown or
// side-effecting action is refused at parse time and ends the run Failed with
// no side effect. RunWorker never calls deps.Remember; saving a finding is the
// Worker's concern.
func RunWorker(ctx context.Context, deps WorkerDeps, brief Brief) Finding {
	return runWorker(ctx, deps, brief, nil, nil)
}

// afterLoop runs once the loop has produced its Finding, while the worker
// span is still open; span is nil when no tracer is configured.
type afterLoop func(ctx context.Context, f Finding, span trace.Span)

// runWorker runs the loop. A non-nil progressGate holds each Progress
// delivery until the gate is closed, dropping it if the progress deadline
// passes first.
func runWorker(ctx context.Context, deps WorkerDeps, brief Brief, progressGate <-chan struct{}, after afterLoop) Finding {
	if deps.Caps.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deps.Caps.Timeout)
		defer cancel()
	}

	var span trace.Span
	if deps.Tracer != nil {
		ctx, span = deps.Tracer.StartSpan(ctx, trace.SpanWorker)
		span.SetAttr("objective", boundDetail(brief.Objective))
	}

	w := &workerRun{deps: deps, brief: brief, progressGate: progressGate}
	finding := w.loop(ctx)
	if after != nil {
		after(ctx, finding, span)
	}

	if span != nil {
		span.SetAttr("status", string(finding.Status))
		span.SetAttr("turns", w.turns)
		span.SetAttr("dropped_citations", w.droppedCitations)
		if finding.Detail != "" {
			span.SetAttr("detail", finding.Detail)
		}
		// Per-worker spend lives on the span; the run core records the
		// run-level metrics once from the returned Usage.
		span.SetAttr("search_count", finding.Usage.SearchCount)
		span.SetAttr("recall_count", finding.Usage.RecallCount)
		span.SetAttr("input_tokens", finding.Usage.InputTokens)
		span.SetAttr("output_tokens", finding.Usage.OutputTokens)
		span.SetAttr("estimated_cost_gbp", finding.Usage.EstimatedCostGBP)
		var spanErr error
		if finding.Status == types.StatusFailed {
			spanErr = errors.New(finding.Detail)
		}
		span.End(spanErr)
	}
	return finding
}

// workerRun holds the mutable state of one loop: the growing transcript, the
// accumulated usage, the citations gathered so far, and the URLs the model is
// permitted to fetch and cite (only URLs it has actually seen in a search
// result or fetched).
type workerRun struct {
	deps         WorkerDeps
	brief        Brief
	progressGate <-chan struct{}

	transcript []model.Message
	usage      types.Usage
	citations  []types.Citation
	// seenURLs is the allow-list for fetch and for final citations: a URL
	// qualifies once it appears in a search result or is the final URL of a
	// successful fetch. This keeps fetch scoped to search-derived URLs, a
	// defence-in-depth complement to the fetch client's own SSRF guard, and
	// keeps the report's sources to ones the worker actually encountered.
	seenURLs map[string]bool
	// citableRefs holds the locators of recalled knowledge store hits. They
	// are citable but never added to seenURLs, so the model cannot spend a
	// fetch on a page behind the store's own authentication.
	citableRefs map[string]bool
	// titles remembers the search-result title for a URL, or a recalled
	// hit's name for its locator, so a citation can carry it.
	titles map[string]string

	turns            int
	toolFailures     int
	droppedCitations int
}

// loop runs the turn loop to a terminal Finding. The caps are checked at the
// top of each turn so a run that has already spent its budget stops before
// paying for another model turn.
func (w *workerRun) loop(ctx context.Context) Finding {
	w.transcript = initialTranscript(w.brief, w.recallEnabled())
	w.seenURLs = map[string]bool{}
	w.citableRefs = map[string]bool{}
	w.titles = map[string]string{}

	for {
		if err := ctx.Err(); err != nil {
			return w.incomplete("worker context ended: " + w.scrub(err.Error()))
		}
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
// stop; done=false to continue.
func (w *workerRun) turn(ctx context.Context) (bool, Finding) {
	w.turns++

	resp, err := w.deps.Model.Generate(ctx, model.Request{
		Messages:   w.transcript,
		MaxTokens:  w.turnCompletionCap(),
		JSONSchema: actionSchemaFor(w.recallEnabled()),
		SchemaName: actionSchemaName,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return true, w.incomplete("worker context ended during a model turn: " + w.scrub(ctxErr.Error()))
		}
		// The model client is single-attempt by contract; the worker adds no
		// retry either, because the turn may already be billed.
		return true, w.failed("model turn failed: " + w.scrub(err.Error()))
	}
	w.accumulateModelUsage(resp.Usage)
	w.transcript = append(w.transcript, assistantEcho(resp.Content))

	if resp.FinishReason == "length" {
		return true, w.incomplete(fmt.Sprintf("the model's reply was cut off at the %d-token completion cap", w.turnCompletionCap()))
	}

	act, err := parseAction(resp.Content, w.recallEnabled())
	if err != nil {
		// Refusing here, with no side effect, is the closed-vocabulary
		// guarantee.
		return true, w.failed("invalid model action: " + w.scrub(err.Error()))
	}
	w.reportProgress(ctx, act)

	switch act.Kind {
	case actionSearch:
		return w.doSearch(ctx, act)
	case actionFetch:
		return w.doFetch(ctx, act)
	case actionRecall:
		return w.doRecall(ctx, act)
	case actionFinal:
		return true, w.finalise(act)
	default:
		return true, w.failed(fmt.Sprintf("unhandled action %q", act.Kind))
	}
}

// doSearch runs a search action, appends the results to the transcript, and
// records every returned URL as fetchable and citable. A search failure is
// fed back to the model unless the consecutive-failure bound is reached.
func (w *workerRun) doSearch(ctx context.Context, act action) (bool, Finding) {
	results, err := w.deps.Search.Search(ctx, act.Query)
	if err != nil {
		return w.toolFailure(ctx, "search failed: "+w.scrub(err.Error()))
	}
	w.toolFailures = 0
	w.usage.SearchCount++
	for _, r := range results {
		if r.URL != "" {
			w.seenURLs[r.URL] = true
			if _, ok := w.titles[r.URL]; !ok && r.Title != "" {
				w.titles[r.URL] = r.Title
			}
		}
	}
	w.transcript = append(w.transcript, searchResultsMessage(act.Query, results))
	return false, Finding{}
}

// doFetch runs a fetch action for a URL the model has already seen in a search
// result, reduces the page to bounded text, appends it to the transcript, and
// records the page as a citation. A fetch of an unseen URL is refused without
// any request and fed back as guidance. A fetch failure (refused destination,
// HTTP error, transport error, non-text content) is fed back to the model
// unless the consecutive-failure bound is reached.
func (w *workerRun) doFetch(ctx context.Context, act action) (bool, Finding) {
	if !w.seenURLs[act.URL] {
		w.transcript = append(w.transcript, errorFeedbackMessage(
			fmt.Sprintf("the URL %q did not appear in any search result, so it cannot be fetched.", act.URL)))
		return false, Finding{}
	}
	if w.deps.Fetch == nil {
		return true, w.failed("fetch requested but no fetch client is configured")
	}

	page, err := w.deps.Fetch.Fetch(ctx, act.URL)
	if err != nil {
		return w.toolFailure(ctx, "fetch failed: "+w.scrub(err.Error()))
	}
	fp := fetchedPage{
		URL:         page.URL,
		ContentType: page.ContentType,
		Content:     page.Content,
		Truncated:   page.Truncated,
	}
	rendered, ok := pageText(fp, w.maxPageBytes())
	if !ok {
		return w.toolFailure(ctx, fmt.Sprintf("the content at %s (%s) is not text and cannot be read.", fp.URL, fp.ContentType))
	}
	w.toolFailures = 0

	// A fetched page is a source the worker read, so it is citable under its
	// final URL as well as the requested one.
	w.seenURLs[page.URL] = true
	w.addCitation(page.URL, w.titles[act.URL])

	w.transcript = append(w.transcript, fetchedPageMessage(fp.URL, fp.ContentType, rendered.text, fp.Truncated || rendered.truncated))
	return false, Finding{}
}

// doRecall queries the knowledge store, appends the hits to the transcript,
// and records every hit locator as citable (never fetchable). A recall
// failure shares the search and fetch failure bound, so a dead store cannot
// extend a run.
func (w *workerRun) doRecall(ctx context.Context, act action) (bool, Finding) {
	hits, err := w.deps.Knowledge.Recall(ctx, w.deps.KnowledgeNamespace, memory.Query{
		Text:  act.Query,
		Limit: w.recallLimit(),
	})
	if err != nil {
		return w.toolFailure(ctx, "recall failed: "+w.scrub(err.Error()))
	}
	w.toolFailures = 0
	w.usage.RecallCount++
	for _, h := range hits {
		loc := h.Reference.Locator
		if loc == "" {
			continue
		}
		w.citableRefs[loc] = true
		if _, ok := w.titles[loc]; !ok && h.Memory.Meta.Name != "" {
			w.titles[loc] = h.Memory.Meta.Name
		}
	}
	w.transcript = append(w.transcript, recallResultsMessage(act.Query, hits, w.maxRecallBytes()))
	return false, Finding{}
}

// toolFailure handles a failed search, fetch or recall: a context end is
// Incomplete; otherwise the failure is fed back to the model so it can choose
// another source, until maxConsecutiveToolFailures in a row ends the run
// Failed.
func (w *workerRun) toolFailure(ctx context.Context, detail string) (bool, Finding) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return true, w.incomplete("worker context ended during a tool call: " + w.scrub(ctxErr.Error()))
	}
	w.toolFailures++
	if w.toolFailures >= maxConsecutiveToolFailures {
		return true, w.failed(fmt.Sprintf("%d consecutive tool failures; last: %s", w.toolFailures, detail))
	}
	w.transcript = append(w.transcript, toolFailureMessage(detail))
	return false, Finding{}
}

// finalise builds the successful Finding from a final action: the sanitised
// answer text and the model's citations, restricted to URLs the worker
// actually saw and locators it recalled, merged with the pages it fetched and
// deduplicated. A recalled locator carries the store's name for it.
func (w *workerRun) finalise(act action) Finding {
	for _, c := range act.Citations {
		switch {
		case w.seenURLs[c.URL]:
			w.addCitation(c.URL, c.Title)
		case w.citableRefs[c.URL]:
			title := w.titles[c.URL]
			if title == "" {
				title = c.Title
			}
			w.addCitation(c.URL, title)
		default:
			w.droppedCitations++
		}
	}
	return Finding{
		Text:      sanitiseAnswer(act.Answer),
		Citations: w.citations,
		Usage:     w.usage,
		Status:    types.StatusCompleted,
		Turns:     w.turns,
	}
}

// reportProgress delivers this turn's Progress to deps.Progress within one
// progress deadline. With a gate, delivery first waits for it to open; a
// delivery whose deadline passes first, or whose run has ended, is dropped.
func (w *workerRun) reportProgress(ctx context.Context, act action) {
	hook := w.deps.Progress
	if hook == nil {
		return
	}
	p := Progress{
		Turn:             w.turns,
		MaxTurns:         w.deps.Caps.MaxTurns,
		Action:           string(act.Kind),
		Detail:           progressDetail(act),
		InputTokens:      w.usage.InputTokens,
		OutputTokens:     w.usage.OutputTokens,
		EstimatedCostGBP: w.usage.EstimatedCostGBP,
	}

	ctx, cancel := context.WithTimeout(ctx, w.deps.progressDeadline())
	defer cancel()
	if w.progressGate != nil {
		select {
		case <-w.progressGate:
		case <-ctx.Done():
		}
	}
	if ctx.Err() != nil {
		return
	}
	if r := callProgress(ctx, hook, p); r != nil {
		w.deps.logger().Debug("worker: progress hook panicked", "recover", w.scrub(fmt.Sprint(r)))
	}
}

// callProgress runs hook in its own goroutine and waits until it returns or
// ctx ends, so a hook that ignores ctx cannot hold the loop past the
// deadline. A panic in the hook is recovered and returned, never re-raised:
// progress is observability, never a reason to fail a paid run. The recovered
// value travels over a buffered channel rather than a named return, because
// the goroutine can still be writing after the ctx.Done branch returns.
func callProgress(ctx context.Context, hook func(context.Context, Progress), p Progress) any {
	recovered := make(chan any, 1)
	go func() {
		defer func() { recovered <- recover() }()
		hook(ctx, p)
	}()
	select {
	case r := <-recovered:
		return r
	case <-ctx.Done():
		return nil
	}
}

// progressDetail names an action's target for a Progress report: the query
// for search and recall, the URL's origin for fetch, nothing for final. The
// text is flattened, scrubbed, then bounded, in that order, so the bound
// cannot split a credential into fragments too short for the scrubber.
func progressDetail(act action) string {
	var s string
	switch act.Kind {
	case actionSearch, actionRecall:
		s = act.Query
	case actionFetch:
		s = urlOrigin(act.URL)
	}
	return strings.TrimSpace(boundRunes(secret.Scrub(printableOneLine(s)), maxProgressDetailRunes))
}

// urlOrigin reduces a URL to scheme://host, lowercased, keeping a port but
// dropping userinfo, path, query and fragment. A URL without both a scheme
// and a host yields "".
func urlOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// printableOneLine flattens s to one line of printable text, dropping control
// and format characters such as terminal escapes and bidirectional overrides.
func printableOneLine(s string) string {
	return oneLine(strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			return r
		}
		return -1
	}, s))
}

// incomplete builds a bounded-stop Finding: a cap, the context, or a truncated
// reply ended the loop before a final answer.
func (w *workerRun) incomplete(detail string) Finding {
	return Finding{
		Citations: w.citations,
		Usage:     w.usage,
		Status:    types.StatusIncomplete,
		Detail:    boundDetail(detail),
		Turns:     w.turns,
	}
}

// failed builds a failed Finding: a model error, an invalid action, or
// repeated tool failures ended the loop.
func (w *workerRun) failed(detail string) Finding {
	return Finding{
		Citations: w.citations,
		Usage:     w.usage,
		Status:    types.StatusFailed,
		Detail:    boundDetail(detail),
		Turns:     w.turns,
	}
}

// addCitation appends a citation, deduplicated by URI. A citation with an
// empty URI is dropped. The title passes through citationTitle; the first
// title seen for a URI wins, except that an empty title is replaced by a
// later non-empty one.
func (w *workerRun) addCitation(uri, title string) {
	if uri == "" {
		return
	}
	title = citationTitle(title)
	for i, c := range w.citations {
		if c.URI == uri {
			if c.Title == "" && title != "" {
				w.citations[i].Title = title
			}
			return
		}
	}
	w.citations = append(w.citations, types.Citation{URI: uri, Title: title})
}

// maxCitationTitleRunes bounds a citation title, which the report, the
// Interaction and a saved finding all carry.
const maxCitationTitleRunes = 200

// angleSpan matches anything between a '<' and the next '>'.
var angleSpan = regexp.MustCompile(`<[^<>]*>`)

// citationTitle makes a store-, search- or model-supplied title safe to
// carry: markup-like spans removed, flattened to one line, and bounded to
// maxCitationTitleRunes.
func citationTitle(s string) string {
	return boundRunes(oneLine(angleSpan.ReplaceAllString(s, "")), maxCitationTitleRunes)
}

// accumulateModelUsage folds one model turn's usage into the running totals
// and, when a price is configured, into the cost estimate.
func (w *workerRun) accumulateModelUsage(u model.Usage) {
	w.usage.InputTokens += u.InputTokens
	w.usage.OutputTokens += u.OutputTokens
	w.usage.EstimatedCostGBP += float64(u.InputTokens)*w.deps.Caps.InputGBPPerMTok/1e6 +
		float64(u.OutputTokens)*w.deps.Caps.OutputGBPPerMTok/1e6
}

// turnCompletionCap is the completion cap for one model turn: the fixed
// per-turn cap, narrowed to whatever remains under the token cap so no turn
// overshoots the accumulated cap by a full completion.
func (w *workerRun) turnCompletionCap() int {
	capTokens := turnCompletionTokens
	if w.deps.Caps.MaxTokens > 0 {
		remaining := w.deps.Caps.MaxTokens - totalTokens(w.usage)
		if remaining < 1 {
			remaining = 1
		}
		if remaining < capTokens {
			capTokens = remaining
		}
	}
	return capTokens
}

func (w *workerRun) recallEnabled() bool {
	return w.deps.Knowledge != nil
}

func (w *workerRun) recallLimit() int {
	if w.deps.Caps.RecallLimit > 0 {
		return w.deps.Caps.RecallLimit
	}
	return DefaultRecallLimit
}

func (w *workerRun) maxRecallBytes() int {
	if w.deps.Caps.MaxRecallBytes > 0 {
		return w.deps.Caps.MaxRecallBytes
	}
	return DefaultMaxPageBytes
}

func (w *workerRun) maxPageBytes() int {
	if w.deps.Caps.MaxPageBytes > 0 {
		return w.deps.Caps.MaxPageBytes
	}
	return DefaultMaxPageBytes
}

// totalTokens is the accumulated token count the token cap is measured
// against: input plus output. types.Usage stores the two counters and no
// total, so the sum lives here.
func totalTokens(u types.Usage) int {
	return u.InputTokens + u.OutputTokens
}

// scrub redacts credential-shaped material from a detail string. The clients
// scrub their own errors; the worker scrubs again unconditionally so a detail
// composed here can never carry a key into the Interaction, the report, or a
// trace.
func (w *workerRun) scrub(s string) string {
	return secret.Scrub(s)
}

// boundDetail truncates a detail string to maxDetailBytes at a rune boundary.
func boundDetail(s string) string { return boundBytes(s, maxDetailBytes) }

// errWorkerNoModel is returned by NewWorker when the model client is nil.
var errWorkerNoModel = errors.New("fleet: worker requires a model client")

// progressDeadline is the bound on one Progress delivery.
func (d WorkerDeps) progressDeadline() time.Duration {
	if d.progressTimeoutOverride > 0 {
		return d.progressTimeoutOverride
	}
	return progressTimeout
}

// logger returns the injected logger, or one that discards every record.
func (d WorkerDeps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.New(slog.DiscardHandler)
}
