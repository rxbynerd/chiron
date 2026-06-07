package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/formatter"
	"github.com/rxbynerd/chiron/internal/interactions"
	"github.com/rxbynerd/chiron/internal/planner"
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

// runResearch executes `chiron research` end-to-end: gate the budget,
// optionally review the plan (--plan), then hand off to the run core.
// M2 awaits by polling; the M5 streaming surface arrives with --stream
// support.
func runResearch(cmd *cobra.Command, cfg config.ResearchConfig) error {
	if cfg.Query == "" {
		return errors.New("research: a query is required (--query or the positional argument)")
	}

	return withRunSeams(cmd, cfg, func(ctx context.Context, apiKey string, deps run.Deps) error {
		opts := geminiOptions(cfg, apiKey)
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
			previousID, err = reviewPlan(ctx, cmd, cfg, opts)
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

// runGet executes `chiron get <id>`: re-attach to a stored interaction,
// await whatever state remains (respecting --timeout), and emit the
// report through the normal sinks. No create, no spend, no budget gate.
func runGet(cmd *cobra.Command, cfg config.ResearchConfig, id string) error {
	return withRunSeams(cmd, cfg, func(ctx context.Context, apiKey string, deps run.Deps) error {
		res, err := gemini.New(geminiOptions(cfg, apiKey))
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
	model := cfg.Model
	if model == "" {
		model = gemini.DefaultFollowUpModel
	}

	return withRunSeams(cmd, cfg, func(ctx context.Context, apiKey string, deps run.Deps) error {
		res, err := gemini.NewFollowUp(geminiOptions(cfg, apiKey), model)
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

// withRunSeams owns the lifecycle every API-bound command shares: the
// timeout context, the resolved API key, the tracer (flushed on exit),
// and the transport and sink seams. The Researcher field is left for
// the caller — research, get and follow-up bind different modes of the
// gemini adapter.
func withRunSeams(cmd *cobra.Command, cfg config.ResearchConfig, f func(ctx context.Context, apiKey string, deps run.Deps) error) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(cfg.Timeout))
	defer cancel()

	apiKey, err := secret.Default().Resolve(ctx, cfg.APIKeyRef)
	if err != nil {
		return err
	}

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

	events := transport.NewStdio(cmd.ErrOrStderr())
	defer events.Close()

	return f(ctx, apiKey, run.Deps{
		Formatter: formatter.NewMarkdown(),
		Sink:      buildSink(cmd, cfg),
		Transport: events,
		Tracer:    tracer,
	})
}

// geminiOptions maps the resolved config onto the adapter's options —
// the one place the flag surface meets the wire surface.
func geminiOptions(cfg config.ResearchConfig, apiKey string) gemini.Options {
	return gemini.Options{
		APIKey:       apiKey,
		BaseURL:      os.Getenv(envGeminiBaseURL),
		Tier:         cfg.Agent,
		Visualise:    cfg.Visualise,
		Tools:        cfg.Tools,
		MCP:          cfg.MCP,
		FileSearch:   cfg.FileSearch,
		Inputs:       cfg.Inputs,
		TemplatePath: cfg.Template,
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
func reviewPlan(ctx context.Context, cmd *cobra.Command, cfg config.ResearchConfig, opts gemini.Options) (string, error) {
	if stdinIsPiped(cmd.InOrStdin()) && !cfg.AcceptPlan {
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
	id, err := session.Run(ctx, cfg.Query)
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
