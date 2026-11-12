package run

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestRun_HappyPath(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	deps, _, snk, tr := happyDeps()
	result, err := Run(context.Background(), deps, Params{Query: "q", Agent: "deep-research"})
	must.NoError(err)

	check.Equal("v1_run", result.InteractionID)
	check.Equal(types.StatusCompleted, result.Status)
	must.NotNil(result.Report, "the run must carry the formatter's report")
	check.Equal("# Report\n", string(result.Report.Markdown))
	check.Equal(80, result.Usage.SearchCount, "usage must carry through to the result")
	check.Greater(result.Duration, time.Duration(0))
	check.Same(result, snk.result, "the sink must receive the same RunResult the caller gets")

	want := []string{
		transport.KindRunStarted,
		transport.KindInteractionCreated,
		transport.KindStatusChanged,
		transport.KindRunCompleted,
		transport.KindCostSummary,
	}
	must.Equal(want, tr.kinds())

	// The resume handle must be in the interaction_created payload.
	var created interactionCreatedPayload
	must.NoError(json.Unmarshal(tr.events[1].Payload, &created), "interaction_created payload")
	check.Equal("v1_run", created.InteractionID, "interaction_created must carry the interaction id")

	var cost costSummaryPayload
	must.NoError(json.Unmarshal(tr.events[4].Payload, &cost), "cost_summary payload")
	check.InDelta(1.58, cost.Usage.EstimatedCostGBP, 0, "cost_summary must carry the run's cost signals")
	check.Equal(80, cost.Usage.SearchCount, "cost_summary must carry the run's cost signals")
}

func TestRun_PassesPreviousInteractionIDToStart(t *testing.T) {
	// Plan approval and follow-up Q&A both chain to a stored
	// interaction; the core must hand the id through to Start verbatim.
	must := require.New(t)
	deps, r, _, _ := happyDeps()
	_, err := Run(context.Background(), deps, Params{Query: "q", PreviousInteractionID: "v1_plan"})
	must.NoError(err)
	must.Equal("v1_plan", r.lastTask.PreviousInteractionID)
}

func TestResumeHappyPath(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	deps, r, snk, tr := happyDeps()
	result, err := Resume(context.Background(), deps, "v1_run")
	must.NoError(err)
	check.False(r.started, "Resume must never start a new interaction; that would spend money")
	check.Equal("v1_run", result.InteractionID)
	check.Equal(types.StatusCompleted, result.Status)
	check.Same(result, snk.result, "the sink must receive the resumed RunResult")

	// No run_started (there is no query to announce) and no new start
	// phase: the lifecycle is id, status, completion, cost.
	want := []string{
		transport.KindInteractionCreated,
		transport.KindStatusChanged,
		transport.KindRunCompleted,
		transport.KindCostSummary,
	}
	must.Equal(want, tr.kinds())
}

func TestResumeRequiresID(t *testing.T) {
	must := require.New(t)
	deps, _, _, _ := happyDeps()
	_, err := Resume(context.Background(), deps, "")
	must.Error(err, "Resume with an empty id must fail before touching any seam")
}

func TestRun_FailureStatusStillConcludes(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	deps, r, snk, _ := happyDeps()
	r.result = &types.Interaction{
		ID:           "v1_run",
		Status:       types.StatusBudgetExceeded,
		StatusDetail: "the server-side budget was exceeded",
	}
	result, err := Run(context.Background(), deps, Params{Query: "q"})
	must.NoError(err, "a failure-variant status is a concluded run, not an error")
	check.Equal(types.StatusBudgetExceeded, result.Status)
	check.NotNil(snk.result, "failure variants still emit a (placeholder) report")
}

func TestRun_StartErrorStopsBeforeSink(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	boom := errors.New("create rejected")
	deps, r, snk, tr := happyDeps()
	r.startErr = boom
	_, err := Run(context.Background(), deps, Params{Query: "q"})
	must.ErrorIs(err, boom)
	check.Nil(snk.result, "nothing must reach the sink when start fails")
	must.Equal([]string{transport.KindRunStarted}, tr.kinds(),
		"no interaction id exists to emit, so run_started is the only event")
}

func TestRun_AwaitErrorEmitsResumeHandle(t *testing.T) {
	must := require.New(t)

	boom := errors.New("requires action")
	deps, r, _, tr := happyDeps()
	r.awaitErr = boom
	_, err := Run(context.Background(), deps, Params{Query: "q"})
	must.ErrorIs(err, boom)
	must.ErrorContains(err, "v1_run", "the error must name the interaction id, the resume handle")

	got := tr.kinds()
	must.Len(got, 2, "events %v, want the resume handle emitted before the failure", got)
	must.Equal(transport.KindInteractionCreated, got[1])
}

func TestRun_TimeoutReturnsDeadlineAndResumeHandle(t *testing.T) {
	must := require.New(t)

	deps, r, _, tr := happyDeps()
	r.awaitBlocks = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, deps, Params{Query: "q"})
	must.ErrorIs(err, context.DeadlineExceeded)

	got := tr.kinds()
	must.Len(got, 2, "events %v, want the resume handle emitted before the timeout", got)
	must.Equal(transport.KindInteractionCreated, got[1])
}

func TestRun_FormatterErrorSkipsSink(t *testing.T) {
	must := require.New(t)

	boom := errors.New("bad front matter")
	deps, _, snk, _ := happyDeps()
	deps.Formatter = &fakeFormatter{err: boom}
	_, err := Run(context.Background(), deps, Params{Query: "q"})
	must.ErrorIs(err, boom)
	must.Nil(snk.result, "nothing must reach the sink when formatting fails")
}

func TestRunSinkError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	boom := errors.New("disk full")
	deps, _, _, tr := happyDeps()
	deps.Sink = &recordingSink{err: boom}
	counter := &countingTracer{}
	deps.Tracer = counter

	_, err := Run(context.Background(), deps, Params{Query: "q"})
	must.ErrorIs(err, boom)
	check.NotContains(tr.kinds(), transport.KindRunCompleted,
		"run_completed must not be emitted when the report failed to land")

	// A failed run emits exactly one failure metric. The deferred emission
	// must not double-count the one recordMetrics already emits.
	must.Equal(1, counter.metrics[trace.MetricFailures],
		"one failed run must emit exactly one failure metric")
}

func TestRun_TransportFailureDoesNotAbort(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	deps, _, snk, _ := happyDeps()
	deps.Transport = &recordingTransport{err: errors.New("stderr closed")}
	result, err := Run(context.Background(), deps, Params{Query: "q"})
	must.NoError(err, "a broken event stream must not abort a paid run")
	check.NotNil(result)
	check.NotNil(snk.result, "the report must still land when events cannot be emitted")
}

func TestRun_RejectsNilDepsAndEmptyQuery(t *testing.T) {
	must := require.New(t)

	deps, _, _, _ := happyDeps()
	cases := []struct {
		name   string
		mutate func(*Deps)
	}{
		{"researcher", func(d *Deps) { d.Researcher = nil }},
		{"formatter", func(d *Deps) { d.Formatter = nil }},
		{"sink", func(d *Deps) { d.Sink = nil }},
		{"transport", func(d *Deps) { d.Transport = nil }},
		{"tracer", func(d *Deps) { d.Tracer = nil }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			d := deps
			tt.mutate(&d)
			_, err := Run(context.Background(), d, Params{Query: "q"})
			must.Error(err, "a nil dependency must be rejected before any seam is called")
		})
	}

	_, err := Run(context.Background(), deps, Params{})
	must.Error(err, "an empty query must be rejected before money is spent")
}

func TestRun_EmptyInteractionIDRejected(t *testing.T) {
	must := require.New(t)

	deps, r, _, _ := happyDeps()
	r.id = ""
	_, err := Run(context.Background(), deps, Params{Query: "q"})
	must.Error(err, "an empty interaction id is not a resume handle; the run must fail")
}

// The empty Multi sink composes with the core for -o none runs.
func TestRun_DiscardSinkStillEmitsCostSummary(t *testing.T) {
	must := require.New(t)

	deps, _, _, tr := happyDeps()
	deps.Sink = sink.Multi()
	_, err := Run(context.Background(), deps, Params{Query: "q"})
	must.NoError(err)

	got := tr.kinds()
	must.NotEmpty(got)
	must.Equal(transport.KindCostSummary, got[len(got)-1],
		"the cost summary must be emitted even when the report is discarded")
}

// A Researcher whose Result returns nil, nil breaks the seam contract. The run
// must conclude with a clear error rather than hand the formatter a nil
// interaction to panic on.
func TestRunNilInteractionFromResearcher(t *testing.T) {
	must := require.New(t)

	deps, r, _, _ := happyDeps()
	r.result = nil
	_, err := Run(context.Background(), deps, Params{Query: "q"})
	must.ErrorContains(err, "returned no interaction")
}

// valueTracer records the last value of each metric; spans are no-ops.
type valueTracer struct {
	trace.Noop
	values map[string]float64
}

func (v *valueTracer) Metric(_ context.Context, name string, value float64) {
	if v.values == nil {
		v.values = make(map[string]float64)
	}
	v.values[name] = value
}

// The run-level recall count is recorded beside the search count from the
// interaction's usage.
func TestRun_RecordsRecallCount(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	deps, r, _, _ := happyDeps()
	r.result.Usage.RecallCount = 3
	tracer := &valueTracer{}
	deps.Tracer = tracer

	_, err := Run(context.Background(), deps, Params{Query: "q"})
	must.NoError(err)
	check.InDelta(3, tracer.values[trace.MetricRecallCount], 0, "%s", trace.MetricRecallCount)
	check.InDelta(80, tracer.values[trace.MetricSearchCount], 0, "%s", trace.MetricSearchCount)
}

// The run-level web spend and unknown-cost count are recorded from the
// interaction's usage under their own names.
func TestRun_RecordsWebCost(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	deps, r, _, _ := happyDeps()
	r.result.Usage.WebCostUSD = 0.015
	r.result.Usage.WebCostUnknownCalls = 2
	tracer := &valueTracer{}
	deps.Tracer = tracer

	_, err := Run(context.Background(), deps, Params{Query: "q"})
	must.NoError(err)
	check.InDelta(0.015, tracer.values[trace.MetricWebCostUSD], 0, "%s", trace.MetricWebCostUSD)
	check.InDelta(2, tracer.values[trace.MetricWebCostUnknownCalls], 0, "%s", trace.MetricWebCostUnknownCalls)
}
