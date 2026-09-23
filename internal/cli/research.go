package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/formatter"
	"github.com/rxbynerd/chiron/internal/interactions"
	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/planner"
	"github.com/rxbynerd/chiron/internal/researcher/fleet"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/fetch"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/gemini"
	"github.com/rxbynerd/chiron/internal/run"
	"github.com/rxbynerd/chiron/internal/secret"
	"github.com/rxbynerd/chiron/internal/sink"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/transport"
	"github.com/rxbynerd/chiron/internal/types"
)

// Process exit codes (documented in the research command's help). The
// research outcome is machine-readable from the exit code alone, so
// pipelines need not parse the report to learn whether to trust it.
const (
	// ExitUsage covers usage, configuration, and infrastructure
	// errors: bad flags, unresolvable secrets, network failures,
	// timeouts. Cobra's own errors land here via main.
	ExitUsage = 1
	// ExitResearchFailed: the task reached failed or incomplete, or
	// reported requires_action — a state deep research cannot
	// legitimately produce and Chiron cannot service.
	ExitResearchFailed = 2
	// ExitResearchStopped: the task was cancelled or exceeded the
	// server-side budget — stopped, rather than broken.
	ExitResearchStopped = 3
	// ExitBlocked: the run was stopped client-side before any research
	// spend — the cost estimate exceeded the --budget cap, or the user
	// declined the plan at the --plan gate. Nothing was started;
	// distinct from ExitResearchStopped, where a running task was
	// stopped server-side.
	ExitBlocked = 4
)

// envGeminiBaseURL overrides the Gemini API endpoint — httptest servers
// in the CLI smoke tests, never production use. Reading it here keeps
// the run core free of environment access.
const envGeminiBaseURL = "CHIRON_GEMINI_BASE_URL"

// ExitError carries a process exit code with its cause, so main can
// exit distinctly for research failures without parsing error text.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }

func (e *ExitError) Unwrap() error { return e.Err }

// runResearch executes `chiron research` end-to-end: select the
// researcher for the chosen agent, gate the budget, optionally review the
// plan (--plan), then hand off to the run core. The deep-research tiers
// bind the Gemini adapter; --agent worker binds Chiron's in-process worker
// (runWorkerResearch); --agent fleet has no researcher yet.
func runResearch(cmd *cobra.Command, cfg config.ResearchConfig) error {
	if cfg.Query == "" {
		return errors.New("research: a query is required (--query or the positional argument)")
	}
	// An agent with no researcher fails here, before any seam is built or
	// secret resolved.
	if err := checkResearcherWired(cfg.Agent); err != nil {
		return err
	}
	if cfg.Agent == config.AgentWorker {
		return runWorkerResearch(cmd, cfg)
	}

	return withRunSeams(cmd, cfg, func(ctx context.Context, apiKey string, deps run.Deps) error {
		opts, err := geminiOptions(cfg, apiKey)
		if err != nil {
			return err
		}
		bindThoughtDisplay(ctx, &opts, deps.Transport)
		res, err := gemini.New(opts)
		if err != nil {
			return err
		}
		// The budget gate runs before ANY create — the research one and
		// the plan rounds alike, since both spend.
		if err := gateBudget(cfg, res.EstimatedCostGBP()); err != nil {
			return err
		}
		deps.Researcher = res

		var previousID string
		if cfg.Plan {
			previousID, err = reviewPlan(ctx, cmd, cfg, opts, deps.Tracer)
			if err != nil {
				return err
			}
		}

		result, err := run.Run(ctx, deps, run.Params{
			Query:                 cfg.Query,
			Agent:                 cfg.Agent,
			PreviousInteractionID: previousID,
		})
		return concludeRun(result, err)
	})
}

// runWorkerResearch runs `chiron research --agent worker`: build the
// in-process worker researcher from cfg.Fleet, then drive the unchanged run
// core. The deep-research levers (--plan, --budget and the Gemini tool
// flags) are rejected by config validation for this agent rather than
// ignored; the worker's spend bounds are the fleet caps.
func runWorkerResearch(cmd *cobra.Command, cfg config.ResearchConfig) error {
	return withRunLifecycle(cmd, cfg, func(ctx context.Context, deps run.Deps, stderr io.Writer) error {
		res, err := buildWorker(ctx, cfg, deps.Tracer, stderr)
		if err != nil {
			return err
		}
		deps.Researcher = res
		result, err := run.Run(ctx, deps, run.Params{
			Query: cfg.Query,
			Agent: cfg.Agent,
		})
		return concludeRun(result, err)
	})
}

// fetchRequestTimeout bounds one web_fetch call. A single slow page must
// not consume the worker's whole wall-clock budget.
const fetchRequestTimeout = 30 * time.Second

// fetchMaxContentBytes bounds the raw body web_fetch reads before the worker
// reduces it to text (fleet.max_page_bytes bounds the text).
const fetchMaxContentBytes = 1 << 20

// buildWorker constructs the in-process worker researcher from the resolved
// config. It resolves the model, search and knowledge key references here, at
// the composition root, and fails before any request if a required endpoint
// or key is missing or unresolvable, so a misconfigured worker never emits a
// resume handle for a run that cannot proceed. WorkerTimeout bounds the whole
// run and each model, search and knowledge call; fetch has its own tighter
// per-call bound.
func buildWorker(ctx context.Context, cfg config.ResearchConfig, tracer trace.Tracer, stderr io.Writer) (*fleet.Worker, error) {
	fc := cfg.Fleet
	if fc.ModelEndpoint == "" {
		return nil, errors.New("research --agent worker: fleet.model_endpoint is required")
	}
	if fc.ModelName == "" {
		return nil, errors.New("research --agent worker: fleet.model_name is required")
	}
	if fc.ModelKeyRef == "" {
		return nil, errors.New("research --agent worker: fleet.model_key_ref is required")
	}
	if fc.SearchEndpoint == "" {
		return nil, errors.New("research --agent worker: fleet.search_endpoint is required")
	}
	if err := requireKnowledge(fc); err != nil {
		return nil, err
	}

	modelKey, err := secret.Default().Resolve(ctx, fc.ModelKeyRef)
	if err != nil {
		return nil, err
	}
	// The search MCP may be keyless; resolve only when a reference is set, so
	// an empty ref sends no Authorization header rather than failing.
	var searchKey string
	if fc.SearchKeyRef != "" {
		searchKey, err = secret.Default().Resolve(ctx, fc.SearchKeyRef)
		if err != nil {
			return nil, err
		}
	}

	allowLoopback, err := fetchAllowLoopbackFromEnv()
	if err != nil {
		return nil, err
	}

	callTimeout := time.Duration(fc.WorkerTimeout)
	fetchTimeout := fetchRequestTimeout
	if callTimeout < fetchTimeout {
		fetchTimeout = callTimeout
	}

	modelClient, err := model.New(model.Options{
		Endpoint:       fc.ModelEndpoint,
		Model:          fc.ModelName,
		APIKey:         modelKey,
		RequestTimeout: callTimeout,
	})
	if err != nil {
		return nil, err
	}
	searchClient, err := search.New(search.Options{
		Endpoint:       fc.SearchEndpoint,
		APIKey:         searchKey,
		RequestTimeout: callTimeout,
	})
	if err != nil {
		return nil, err
	}
	recaller, rememberer, err := buildKnowledge(ctx, fc, callTimeout)
	if err != nil {
		return nil, err
	}
	if !fc.KnowledgeRemember {
		rememberer = nil
	}
	fetchClient, err := fetch.New(fetch.Options{
		RequestTimeout:  fetchTimeout,
		MaxContentBytes: fetchMaxContentBytes,
		AllowLoopback:   allowLoopback,
	})
	if err != nil {
		return nil, err
	}

	return fleet.NewWorker(fleet.WorkerDeps{
		Model:              modelClient,
		Search:             searchClient,
		Fetch:              fetchClient,
		Knowledge:          recaller,
		Remember:           rememberer,
		KnowledgeNamespace: memory.Namespace(fc.KnowledgeSpace),
		Tracer:             tracer,
		Logger:             slog.New(secret.NewScrubHandler(slog.NewTextHandler(stderr, nil))),
		Caps: fleet.Caps{
			MaxTurns:         fc.MaxTurns,
			MaxTokens:        fc.MaxTokens,
			CeilingGBP:       fc.CeilingGBP,
			InputGBPPerMTok:  fc.PriceInputGBPPerMTok,
			OutputGBPPerMTok: fc.PriceOutputGBPPerMTok,
			Timeout:          time.Duration(fc.WorkerTimeout),
			MaxPageBytes:     fc.MaxPageBytes,
			RecallLimit:      fc.KnowledgeLimit,
		},
	})
}

// fetchAllowLoopbackEnv is the test-only switch that lets web_fetch reach
// loopback destinations, so an end-to-end run can fetch from an httptest
// server. Absence is the safe default; it must never be set in production
// (AGENTS.md, security-sensitive environment variables).
const fetchAllowLoopbackEnv = "CHIRON_FETCH_ALLOW_LOOPBACK"

// fetchAllowLoopbackFromEnv reads the loopback switch, accepting only "1"
// or an unset/empty value so a typo cannot silently widen the guard.
func fetchAllowLoopbackFromEnv() (bool, error) {
	switch v := os.Getenv(fetchAllowLoopbackEnv); v {
	case "":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("%s: %q is not valid; set it to 1 for loopback test servers only, or leave it unset", fetchAllowLoopbackEnv, v)
	}
}

// refuseWorkerInteractionID rejects a worker-minted id for the commands that
// re-attach to server-side state: an in-process run keeps none, so there is
// nothing to fetch or follow up.
func refuseWorkerInteractionID(command, id string) error {
	if !fleet.IsWorkerInteractionID(id) {
		return nil
	}
	return fmt.Errorf("%s: %s is an in-process worker id; worker runs hold no server-side state and cannot be re-fetched or followed up", command, id)
}

// runGet executes `chiron get <id>`: re-attach to a stored interaction,
// await whatever state remains (respecting --timeout), and emit the
// report through the normal sinks. No create, no spend, no budget gate.
func runGet(cmd *cobra.Command, cfg config.ResearchConfig, id string) error {
	if err := refuseWorkerInteractionID("get", id); err != nil {
		return err
	}
	return withRunSeams(cmd, cfg, func(ctx context.Context, apiKey string, deps run.Deps) error {
		opts, err := geminiOptions(cfg, apiKey)
		if err != nil {
			return err
		}
		bindThoughtDisplay(ctx, &opts, deps.Transport)
		res, err := gemini.New(opts)
		if err != nil {
			return err
		}
		deps.Researcher = res
		result, err := run.Resume(ctx, deps, id)
		return concludeRun(result, err)
	})
}

// runFollowUp executes `chiron follow-up <id> --query "..."`: a new
// interaction chained to a stored one via previous_interaction_id,
// carrying model rather than agent (docs/INTERACTIONS-API.md §3).
func runFollowUp(cmd *cobra.Command, cfg config.ResearchConfig, previousID string) error {
	if cfg.Query == "" {
		return errors.New("follow-up: a query is required (--query)")
	}
	if err := refuseWorkerInteractionID("follow-up", previousID); err != nil {
		return err
	}
	model := cfg.Model
	if model == "" {
		model = gemini.DefaultFollowUpModel
	}

	return withRunSeams(cmd, cfg, func(ctx context.Context, apiKey string, deps run.Deps) error {
		opts, err := geminiOptions(cfg, apiKey)
		if err != nil {
			return err
		}
		bindThoughtDisplay(ctx, &opts, deps.Transport)
		res, err := gemini.NewFollowUp(opts, model)
		if err != nil {
			return err
		}
		deps.Researcher = res
		result, err := run.Run(ctx, deps, run.Params{
			Query:                 cfg.Query,
			Agent:                 model,
			PreviousInteractionID: previousID,
		})
		return concludeRun(result, err)
	})
}

// withRunSeams owns the lifecycle the Gemini-bound commands share: the
// timeout context, the resolved Gemini API key, the tracer (flushed on
// exit), and the transport and sink seams. The Researcher field is left
// for the caller — research, get and follow-up bind different modes of
// the gemini adapter. The in-process agents run under withRunLifecycle
// directly and resolve their own key references, so a worker run does not
// require the Gemini key.
func withRunSeams(cmd *cobra.Command, cfg config.ResearchConfig, f func(ctx context.Context, apiKey string, deps run.Deps) error) error {
	return withRunLifecycle(cmd, cfg, func(ctx context.Context, deps run.Deps, _ io.Writer) error {
		apiKey, err := secret.Default().Resolve(ctx, cfg.APIKeyRef)
		if err != nil {
			return err
		}
		return f(ctx, apiKey, deps)
	})
}

// withRunLifecycle owns the seam lifecycle every research command shares,
// independent of which Researcher binds: the timeout context, the tracer
// (flushed on exit), and the transport and sink seams. It resolves no
// credential — key resolution belongs to the researcher-specific callback
// (the Gemini key in withRunSeams, the fleet key refs in the worker
// binding), so each agent requires only the secrets it actually uses. The
// Researcher field of the passed Deps is unset; the callback binds it.
// withRunLifecycle wraps the command's stderr in one locked writer and hands
// it to the transport and to f, so the event stream and any logger a
// researcher writes to share a single lock rather than racing on the same
// underlying writer.
func withRunLifecycle(cmd *cobra.Command, cfg config.ResearchConfig, f func(ctx context.Context, deps run.Deps, stderr io.Writer) error) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(cfg.Timeout))
	defer cancel()

	tracer, shutdown, err := newTracer(ctx)
	if err != nil {
		return err
	}
	if shutdown != nil {
		// Flush pending spans before exit; a fresh context so the
		// run's deadline cannot swallow the flush.
		defer func() {
			flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer flushCancel()
			_ = shutdown(flushCtx)
		}()
	}

	stderr := &lockedWriter{w: cmd.ErrOrStderr()}
	events := transport.NewStdio(stderr)
	defer events.Close()

	return f(ctx, run.Deps{
		Formatter: formatter.NewMarkdown(),
		Sink:      buildSink(cmd, cfg),
		Transport: events,
		Tracer:    tracer,
	}, stderr)
}

// lockedWriter serialises writes from goroutines that hold different locks
// over one destination.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// checkResearcherWired reports whether the selected agent has a researcher
// bound at this composition root. The fleet orchestrator passes config
// validation but has no implementation, so it fails here with a clear error
// rather than a nil researcher or a Gemini fallback.
func checkResearcherWired(agent string) error {
	switch agent {
	case config.AgentDeepResearch, config.AgentDeepResearchMax, config.AgentWorker:
		return nil
	default:
		return fmt.Errorf("agent %q: %w", agent, fleet.ErrNotImplemented)
	}
}

// geminiOptions maps the resolved config onto the adapter's options —
// the one place the flag surface meets the wire surface. It fails when
// the base-URL override is invalid, before any client is constructed.
// cfg.Stream drives both halves of the streaming surface: the await
// strategy (SSE with reconnect vs silent polling) and the API request
// (thinking_summaries auto vs none — there is nothing to display
// summaries on in a --quiet run, so they are not requested).
func geminiOptions(cfg config.ResearchConfig, apiKey string) (gemini.Options, error) {
	baseURL, err := geminiBaseURL()
	if err != nil {
		return gemini.Options{}, err
	}
	return gemini.Options{
		APIKey:            apiKey,
		BaseURL:           baseURL,
		Tier:              cfg.Agent,
		Visualise:         cfg.Visualise,
		Stream:            cfg.Stream,
		ThinkingSummaries: cfg.Stream,
		Tools:             cfg.Tools,
		MCP:               cfg.MCP,
		FileSearch:        cfg.FileSearch,
		Inputs:            cfg.Inputs,
		TemplatePath:      cfg.Template,
	}, nil
}

// bindThoughtDisplay routes streamed thought summaries onto the run's
// transport as delta events — the M5 display surface: NDJSON on stderr
// beside the rest of the lifecycle events, never stdout, which belongs
// to the report. Emission is best effort, matching the run core's
// treatment of the transport: a broken event stream must not abort a
// paid run.
func bindThoughtDisplay(ctx context.Context, opts *gemini.Options, tr transport.Transport) {
	if !opts.Stream {
		return
	}
	type deltaPayload struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	opts.OnThought = func(text string) {
		// Scrubbed like every other output path: --input documents may
		// contain credentials the agent quotes back in its thinking.
		payload, err := json.Marshal(deltaPayload{Type: "thought_summary", Text: secret.Scrub(text)})
		if err != nil {
			return
		}
		_ = tr.Emit(ctx, transport.Event{Kind: transport.KindDelta, Payload: payload})
	}
	opts.OnStreamDegraded = func() {
		// The in-flight signal that streaming gave up and the await
		// degraded to polling — without it, an operator watching the
		// event stream cannot tell a connectivity failure from --quiet.
		payload, err := json.Marshal(deltaPayload{Type: "stream_degraded", Text: "streaming failed repeatedly; awaiting by polling"})
		if err != nil {
			return
		}
		_ = tr.Emit(ctx, transport.Event{Kind: transport.KindDelta, Payload: payload})
	}
}

// geminiBaseURL reads and validates CHIRON_GEMINI_BASE_URL before any
// client exists. The API key travels in a header on every request to
// this base, so an unvalidated override is a key-exfiltration and SSRF
// channel (CWE-918, CWE-319): https:// is required, with http://
// permitted for loopback hosts only — the CLI smoke tests' httptest
// servers — so a cleartext or internal-network endpoint can never
// receive the key. The variable's absence is the safe default; see
// AGENTS.md for the operational caveat.
func geminiBaseURL() (string, error) {
	raw := os.Getenv(envGeminiBaseURL)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !allowedBaseScheme(u) {
		return "", fmt.Errorf("%s must be an absolute https:// URL (http:// only for loopback test servers), got %q", envGeminiBaseURL, raw)
	}
	return raw, nil
}

// allowedBaseScheme admits https anywhere and http on loopback only.
func allowedBaseScheme(u *url.URL) bool {
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}

// gateBudget enforces --budget before any create. The estimate is the
// tier's planning figure (the cost table in researcher/gemini); a
// blocked run exits ExitBlocked with both figures, so the caller can
// raise the cap or pick the cheaper tier knowingly.
func gateBudget(cfg config.ResearchConfig, estimateGBP float64) error {
	if cfg.BudgetGBP <= 0 || estimateGBP <= cfg.BudgetGBP {
		return nil
	}
	return &ExitError{
		Code: ExitBlocked,
		Err: fmt.Errorf("research blocked before any spend: estimated cost £%.2f (%s) exceeds the £%.2f budget cap — raise --budget or choose a cheaper tier",
			estimateGBP, cfg.Agent, cfg.BudgetGBP),
	}
}

// reviewPlan runs the collaborative-planning gate (--plan): propose a
// plan, review it interactively — or approve the first one unattended
// with --accept-plan — and return the accepted plan's interaction id
// for the research run to chain from. Plans and prompts render on
// stderr: stdout belongs to the report. Without a terminal on stdin
// there is no one to approve the spend, so the run aborts unless
// --accept-plan says otherwise.
func reviewPlan(ctx context.Context, cmd *cobra.Command, cfg config.ResearchConfig, opts gemini.Options, tracer trace.Tracer) (string, error) {
	if !stdinIsTerminal(cmd.InOrStdin()) && !cfg.AcceptPlan {
		return "", errors.New("research --plan: stdin is not a terminal, so the plan cannot be reviewed interactively — pass --accept-plan to approve the first plan unattended")
	}

	p, err := gemini.NewPlanner(opts)
	if err != nil {
		return "", err
	}
	session := &planner.Session{
		Planner:    p,
		In:         cmd.InOrStdin(),
		Out:        cmd.ErrOrStderr(),
		AutoAccept: cfg.AcceptPlan,
	}
	// The plan phase can spend minutes and several API round-trips;
	// the span makes it visible to trace backends rather than a gap
	// before the research root span. It necessarily precedes that root
	// span — planning happens before run.Run — so it traces as its own
	// root rather than a child.
	planCtx, span := tracer.StartSpan(ctx, trace.SpanPlan)
	id, err := session.Run(planCtx, cfg.Query)
	if id != "" {
		span.SetAttr("interaction_id", id)
	}
	span.End(err)
	if errors.Is(err, planner.ErrAborted) {
		// Declining the plan blocks the run before any research spend —
		// the same contract as the budget gate.
		return "", &ExitError{Code: ExitBlocked, Err: err}
	}
	return id, err
}

// concludeRun maps the run core's outcome onto the exit-code contract:
// requires_action is a research outcome (deep research cannot
// legitimately request client action — docs/INTERACTIONS-API.md §4),
// not an infrastructure fault, so it exits as a failed run with the
// detail in the error.
func concludeRun(result *types.RunResult, err error) error {
	if err != nil {
		if errors.Is(err, interactions.ErrRequiresAction) {
			return &ExitError{Code: ExitResearchFailed, Err: err}
		}
		return err
	}
	return exitForStatus(result)
}

// exitForStatus maps a concluded run's terminal status onto the exit
// code contract. The report (or its placeholder) has already been
// emitted; this is the outcome signal.
func exitForStatus(result *types.RunResult) error {
	switch result.Status {
	case types.StatusCompleted:
		return nil
	case types.StatusCancelled, types.StatusBudgetExceeded:
		return &ExitError{
			Code: ExitResearchStopped,
			Err:  fmt.Errorf("research stopped: interaction %s ended %s", result.InteractionID, result.Status),
		}
	default: // failed, incomplete, or anything the API adds.
		return &ExitError{
			Code: ExitResearchFailed,
			Err:  fmt.Errorf("research failed: interaction %s ended %s", result.InteractionID, result.Status),
		}
	}
}

// newTracer binds OTel when an OTLP endpoint is configured in the
// environment (the standard OTEL_EXPORTER_OTLP_* variables), and the
// no-op tracer otherwise — building an exporter with nowhere to send
// spans would only buffer and drop them. The shutdown func is non-nil
// only for the OTel binding.
func newTracer(ctx context.Context) (trace.Tracer, func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" &&
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return trace.Noop{}, nil, nil
	}
	return trace.NewOTel(ctx, "")
}

// buildSink composes the report sinks from the output levers
// (PROPOSAL §4.3): --out binds the file sink (report + chart assets);
// -o selects the stdout surface — text (the Markdown document, unless
// --out already owns it), json (the RunResult envelope, useful beside
// --out), or none. With neither, the report is discarded and the run's
// events and exit code still carry the outcome.
func buildSink(cmd *cobra.Command, cfg config.ResearchConfig) sink.ReportSink {
	var sinks []sink.ReportSink
	if cfg.Out != "" {
		sinks = append(sinks, sink.NewFile(cfg.Out))
	}
	switch cfg.Output {
	case config.OutputText:
		if cfg.Out == "" {
			sinks = append(sinks, sink.NewStdoutMarkdown(cmd.OutOrStdout()))
		}
	case config.OutputJSON:
		sinks = append(sinks, sink.NewStdoutJSON(cmd.OutOrStdout()))
	}
	return sink.Multi(sinks...)
}
