package run

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/sink"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/transport"
	"github.com/rxbynerd/chiron/internal/types"
)

// fakeResearcher scripts the Researcher seam.
type fakeResearcher struct {
	id          string
	startErr    error
	awaitErr    error
	awaitBlocks bool // block until ctx is done, then return ctx.Err()
	result      *types.Interaction
	resultErr   error

	started  bool
	lastTask researcher.Task
}

func (f *fakeResearcher) Start(_ context.Context, task researcher.Task) (string, error) {
	f.started = true
	f.lastTask = task
	if f.startErr != nil {
		return "", f.startErr
	}
	return f.id, nil
}

func (f *fakeResearcher) Await(ctx context.Context, _ string) error {
	if f.awaitBlocks {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.awaitErr
}

func (f *fakeResearcher) Result(_ context.Context, _ string) (*types.Interaction, error) {
	return f.result, f.resultErr
}

// fakeFormatter returns a fixed report or an error.
type fakeFormatter struct {
	report *types.Report
	err    error
}

func (f *fakeFormatter) Format(_ context.Context, _ *types.Interaction) (*types.Report, error) {
	return f.report, f.err
}

// recordingSink captures the RunResult it is given.
type recordingSink struct {
	result *types.RunResult
	err    error
}

func (s *recordingSink) Write(_ context.Context, result *types.RunResult) error {
	if s.err != nil {
		return s.err
	}
	s.result = result
	return nil
}

// recordingTransport captures emitted events in order.
type recordingTransport struct {
	events []transport.Event
	err    error
}

func (t *recordingTransport) Emit(_ context.Context, ev transport.Event) error {
	if t.err != nil {
		return t.err
	}
	t.events = append(t.events, ev)
	return nil
}

func (t *recordingTransport) Close() error { return nil }

func (t *recordingTransport) kinds() []string {
	kinds := make([]string, len(t.events))
	for i, ev := range t.events {
		kinds[i] = ev.Kind
	}
	return kinds
}

// countingTracer counts Metric emissions by name; spans are no-ops.
type countingTracer struct {
	trace.Noop
	metrics map[string]int
}

func (c *countingTracer) Metric(_ context.Context, name string, _ float64) {
	if c.metrics == nil {
		c.metrics = make(map[string]int)
	}
	c.metrics[name]++
}

func completedInteraction() *types.Interaction {
	return &types.Interaction{
		ID:     "v1_run",
		Agent:  "deep-research-preview-04-2026",
		Query:  "q",
		Status: types.StatusCompleted,
		Usage: types.Usage{
			InputTokens:      250000,
			OutputTokens:     60000,
			SearchCount:      80,
			PollCount:        7,
			EstimatedCostGBP: 1.58,
		},
	}
}

func happyDeps() (Deps, *fakeResearcher, *recordingSink, *recordingTransport) {
	r := &fakeResearcher{id: "v1_run", result: completedInteraction()}
	s := &recordingSink{}
	tr := &recordingTransport{}
	deps := Deps{
		Researcher: r,
		Formatter:  &fakeFormatter{report: &types.Report{Markdown: []byte("# Report\n")}},
		Sink:       s,
		Transport:  tr,
		Tracer:     trace.Noop{},
	}
	return deps, r, s, tr
}

func TestRunHappyPath(t *testing.T) {
	deps, _, snk, tr := happyDeps()
	result, err := Run(context.Background(), deps, Params{Query: "q", Agent: "deep-research"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.InteractionID != "v1_run" || result.Status != types.StatusCompleted {
		t.Errorf("result = %+v, want completed v1_run", result)
	}
	if result.Report == nil || string(result.Report.Markdown) != "# Report\n" {
		t.Errorf("report = %+v, want the formatter's document", result.Report)
	}
	if result.Usage.SearchCount != 80 {
		t.Errorf("usage not carried through: %+v", result.Usage)
	}
	if result.Duration <= 0 {
		t.Errorf("duration = %v, want > 0", result.Duration)
	}
	if snk.result != result {
		t.Error("the sink must receive the same RunResult the caller gets")
	}

	want := []string{
		transport.KindRunStarted,
		transport.KindInteractionCreated,
		transport.KindStatusChanged,
		transport.KindRunCompleted,
		transport.KindCostSummary,
	}
	got := tr.kinds()
	if len(got) != len(want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event kinds = %v, want %v", got, want)
		}
	}

	// The resume handle must be in the interaction_created payload.
	var created interactionCreatedPayload
	if err := json.Unmarshal(tr.events[1].Payload, &created); err != nil {
		t.Fatalf("interaction_created payload: %v", err)
	}
	if created.InteractionID != "v1_run" {
		t.Errorf("interaction_created carries %q, want the interaction id", created.InteractionID)
	}

	var cost costSummaryPayload
	if err := json.Unmarshal(tr.events[4].Payload, &cost); err != nil {
		t.Fatalf("cost_summary payload: %v", err)
	}
	if cost.Usage.EstimatedCostGBP != 1.58 || cost.Usage.SearchCount != 80 {
		t.Errorf("cost_summary usage = %+v, want the run's cost signals", cost.Usage)
	}
}

func TestRunPassesPreviousInteractionID(t *testing.T) {
	// Plan approval and follow-up Q&A both chain to a stored
	// interaction; the core must hand the id through to Start verbatim.
	deps, r, _, _ := happyDeps()
	_, err := Run(context.Background(), deps, Params{Query: "q", PreviousInteractionID: "v1_plan"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.lastTask.PreviousInteractionID != "v1_plan" {
		t.Errorf("Start task = %+v, want previous interaction v1_plan", r.lastTask)
	}
}

func TestResumeHappyPath(t *testing.T) {
	deps, r, snk, tr := happyDeps()
	result, err := Resume(context.Background(), deps, "v1_run")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if r.started {
		t.Error("Resume must never start a new interaction — no new spend")
	}
	if result.InteractionID != "v1_run" || result.Status != types.StatusCompleted {
		t.Errorf("result = %+v, want completed v1_run", result)
	}
	if snk.result != result {
		t.Error("the sink must receive the resumed RunResult")
	}

	// No run_started (there is no query to announce) and no new start
	// phase: the lifecycle is id, status, completion, cost.
	want := []string{
		transport.KindInteractionCreated,
		transport.KindStatusChanged,
		transport.KindRunCompleted,
		transport.KindCostSummary,
	}
	if got := tr.kinds(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("event kinds = %v, want %v", got, want)
	}
}

func TestResumeRequiresID(t *testing.T) {
	deps, _, _, _ := happyDeps()
	if _, err := Resume(context.Background(), deps, ""); err == nil {
		t.Error("Resume with an empty id must fail before touching any seam")
	}
}

func TestRunFailedStatusStillConcludes(t *testing.T) {
	deps, r, snk, _ := happyDeps()
	r.result = &types.Interaction{
		ID:           "v1_run",
		Status:       types.StatusBudgetExceeded,
		StatusDetail: "the server-side budget was exceeded",
	}
	result, err := Run(context.Background(), deps, Params{Query: "q"})
	if err != nil {
		t.Fatalf("Run: a failure-variant status is a concluded run, got error %v", err)
	}
	if result.Status != types.StatusBudgetExceeded {
		t.Errorf("status = %s, want budget_exceeded", result.Status)
	}
	if snk.result == nil {
		t.Error("failure variants still emit a (placeholder) report")
	}
}

func TestRunStartError(t *testing.T) {
	boom := errors.New("create rejected")
	deps, r, snk, tr := happyDeps()
	r.startErr = boom
	if _, err := Run(context.Background(), deps, Params{Query: "q"}); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the researcher's start failure", err)
	}
	if snk.result != nil {
		t.Error("nothing must reach the sink when start fails")
	}
	got := tr.kinds()
	if len(got) != 1 || got[0] != transport.KindRunStarted {
		t.Errorf("events = %v, want run_started only — no interaction id exists to emit", got)
	}
}

func TestRunAwaitError(t *testing.T) {
	boom := errors.New("requires action")
	deps, r, _, tr := happyDeps()
	r.awaitErr = boom
	_, err := Run(context.Background(), deps, Params{Query: "q"})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the await failure", err)
	}
	if !strings.Contains(err.Error(), "v1_run") {
		t.Errorf("error %q must name the interaction id — the resume handle", err)
	}
	got := tr.kinds()
	if len(got) != 2 || got[1] != transport.KindInteractionCreated {
		t.Errorf("events = %v, want the resume handle emitted before the failure", got)
	}
}

func TestRunTimeout(t *testing.T) {
	deps, r, _, tr := happyDeps()
	r.awaitBlocks = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, deps, Params{Query: "q"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	got := tr.kinds()
	if len(got) != 2 || got[1] != transport.KindInteractionCreated {
		t.Errorf("events = %v, want the resume handle emitted before the timeout", got)
	}
}

func TestRunFormatterError(t *testing.T) {
	boom := errors.New("bad front matter")
	deps, _, snk, _ := happyDeps()
	deps.Formatter = &fakeFormatter{err: boom}
	if _, err := Run(context.Background(), deps, Params{Query: "q"}); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the formatter failure", err)
	}
	if snk.result != nil {
		t.Error("nothing must reach the sink when formatting fails")
	}
}

func TestRunSinkError(t *testing.T) {
	boom := errors.New("disk full")
	deps, _, _, tr := happyDeps()
	deps.Sink = &recordingSink{err: boom}
	counter := &countingTracer{}
	deps.Tracer = counter
	if _, err := Run(context.Background(), deps, Params{Query: "q"}); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the sink failure", err)
	}
	for _, kind := range tr.kinds() {
		if kind == transport.KindRunCompleted {
			t.Error("run_completed must not be emitted when the report failed to land")
		}
	}
	// C1-CODE-6: one failed run, one failure metric — the deferred
	// emission must not double-count with recordMetrics' own.
	if n := counter.metrics[trace.MetricFailures]; n != 1 {
		t.Errorf("MetricFailures emitted %d time(s) on a sink failure, want exactly 1", n)
	}
}

func TestRunTransportFailureDoesNotAbort(t *testing.T) {
	deps, _, snk, _ := happyDeps()
	deps.Transport = &recordingTransport{err: errors.New("stderr closed")}
	result, err := Run(context.Background(), deps, Params{Query: "q"})
	if err != nil {
		t.Fatalf("Run: a broken event stream must not abort a paid run, got %v", err)
	}
	if result == nil || snk.result == nil {
		t.Error("the report must still land when events cannot be emitted")
	}
}

func TestRunValidatesDeps(t *testing.T) {
	deps, _, _, _ := happyDeps()
	cases := []struct {
		name  string
		mutil func(*Deps)
	}{
		{"researcher", func(d *Deps) { d.Researcher = nil }},
		{"formatter", func(d *Deps) { d.Formatter = nil }},
		{"sink", func(d *Deps) { d.Sink = nil }},
		{"transport", func(d *Deps) { d.Transport = nil }},
		{"tracer", func(d *Deps) { d.Tracer = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := deps
			tc.mutil(&d)
			if _, err := Run(context.Background(), d, Params{Query: "q"}); err == nil {
				t.Errorf("nil %s must be rejected before any seam is called", tc.name)
			}
		})
	}
	if _, err := Run(context.Background(), deps, Params{}); err == nil {
		t.Error("an empty query must be rejected before money is spent")
	}
}

func TestRunEmptyInteractionIDRejected(t *testing.T) {
	deps, r, _, _ := happyDeps()
	r.id = ""
	if _, err := Run(context.Background(), deps, Params{Query: "q"}); err == nil {
		t.Error("an empty interaction id is not a resume handle; the run must fail")
	}
}

// The empty Multi sink composes with the core for -o none runs.
func TestRunWithDiscardSink(t *testing.T) {
	deps, _, _, tr := happyDeps()
	deps.Sink = sink.Multi()
	if _, err := Run(context.Background(), deps, Params{Query: "q"}); err != nil {
		t.Fatalf("Run with discard sink: %v", err)
	}
	got := tr.kinds()
	if got[len(got)-1] != transport.KindCostSummary {
		t.Errorf("events = %v, want the cost summary even when the report is discarded", got)
	}
}

// TestRunNilInteractionFromResearcher pins C2-TEST-10, the seam
// contract: a Researcher whose Result returns nil, nil is a broken
// implementation, and the run must conclude with a clear error rather
// than handing the formatter a nil interaction to panic on.
func TestRunNilInteractionFromResearcher(t *testing.T) {
	deps, r, _, _ := happyDeps()
	r.result = nil // Result returns nil, nil
	_, err := Run(context.Background(), deps, Params{Query: "q"})
	if err == nil || !strings.Contains(err.Error(), "returned no interaction") {
		t.Fatalf("error = %v, want the nil-interaction guard", err)
	}
}
