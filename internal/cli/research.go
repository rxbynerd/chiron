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

// runResearch executes `chiron research` end-to-end: resolve the API
// key, construct the seams from the resolved config, and hand off to
// the run core. M2 awaits by polling; the M5 streaming surface arrives
// with --stream support.
func runResearch(cmd *cobra.Command, cfg config.ResearchConfig) error {
	if cfg.Query == "" {
		return errors.New("research: a query is required (--query or the positional argument)")
	}

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

	res, err := gemini.New(gemini.Options{
		APIKey:       apiKey,
		BaseURL:      os.Getenv(envGeminiBaseURL),
		Tier:         cfg.Agent,
		Visualise:    cfg.Visualise,
		Tools:        cfg.Tools,
		MCP:          cfg.MCP,
		FileSearch:   cfg.FileSearch,
		Inputs:       cfg.Inputs,
		TemplatePath: cfg.Template,
	})
	if err != nil {
		return err
	}

	events := transport.NewStdio(cmd.ErrOrStderr())
	defer events.Close()

	result, err := run.Run(ctx, run.Deps{
		Researcher: res,
		Formatter:  formatter.NewMarkdown(),
		Sink:       buildSink(cmd, cfg),
		Transport:  events,
		Tracer:     tracer,
	}, run.Params{Query: cfg.Query, Agent: cfg.Agent})
	if err != nil {
		// requires_action is a research outcome, not an infrastructure
		// fault: deep research cannot legitimately request client
		// action (docs/INTERACTIONS-API.md §4), so the task is broken,
		// not Chiron — exit as a failed run, detail in the error.
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
