package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/chiron/internal/config"
)

func newResearchCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "research [query]",
		Short: "Start a research task, await it, and emit the report",
		Long: `Start a research task, await the long-running agent, retrieve the
result, and emit a cited Markdown report. The main path.

The interaction id is emitted on stderr (NDJSON run events) as soon as
it is known: it is the resume handle for a crashed or timed-out run.

With --plan, the agent first proposes a research plan for interactive
review (accept, refine, or quit) before any research money is spent;
the plan and its prompts render on stderr, keeping stdout clean for
the report. Reviewing needs a terminal on stdin — in a pipeline, pass
--accept-plan to approve the first plan unattended.

With --budget, the run is blocked up front when the tier's estimated
cost exceeds the cap — before any interaction is created.

Exit codes:

  0  the research completed and the report was emitted
  1  usage, configuration, or infrastructure errors (bad flags,
     unresolvable secrets, network failures, timeout)
  2  the research task ended failed or incomplete, or required client
     action (a state deep research cannot legitimately produce)
  3  the research task was cancelled or exceeded the server-side budget
  4  the run was blocked before any spend: the estimate exceeded
     --budget, or the plan review was aborted`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd, args)
			if err != nil {
				return err
			}
			return runResearch(cmd, cfg)
		},
	}
	addResearchFlags(cmd)
	return cmd
}

func newResearchConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "research-config",
		Short: "Emit the resolved ResearchConfig JSON without running",
		Long: `Resolve the base config (stdin or --config) plus any flags and emit the
result as JSON. Composable in a pipeline:

  chiron research-config --agent deep-research-max \
    | chiron research-config --visualise \
    | chiron research --query "..." --out report.md`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd, nil)
			if err != nil {
				return err
			}
			return cfg.EncodeJSON(cmd.OutOrStdout())
		},
	}
	addResearchFlags(cmd)
	return cmd
}

func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <interaction-id>",
		Short: "Re-fetch and format an interaction (resume after a crash)",
		Long: `Re-fetch a completed or in-progress interaction by its ID and format the
report. State is held server-side, so a crashed run is recovered with no
local state. An in-progress interaction is awaited to completion,
respecting --timeout; a finished one is emitted immediately. No new
interaction is created and nothing new is spent.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd, nil)
			if err != nil {
				return err
			}
			return runGet(cmd, cfg, args[0])
		},
	}
	addResearchFlags(cmd)
	return cmd
}

func newFollowUpCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "follow-up <interaction-id>",
		Short: "Ask a follow-up question against a completed interaction",
		Long: `Ask a follow-up question about a completed research interaction. The
question is answered by a model over the stored interaction
(previous_interaction_id), not by a new research task — quick and far
cheaper than re-researching. --model overrides the default model.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd, nil)
			if err != nil {
				return err
			}
			return runFollowUp(cmd, cfg, args[0])
		},
	}
	addResearchFlags(cmd)
	return cmd
}

// addResearchFlags binds the shared research flag surface (PROPOSAL §4.3)
// plus the CLI-level --config flag, which names the base config rather
// than being part of it.
func addResearchFlags(cmd *cobra.Command) {
	config.RegisterFlags(cmd.Flags())
	cmd.Flags().String("config", "", `base ResearchConfig path ("-" or piped stdin for composition)`)
	cmd.MarkFlagsMutuallyExclusive("stream", "quiet")
}

// resolveConfig produces the run's ResearchConfig: defaults, overlaid by
// the base config (--config file, or stdin when piped), overlaid by
// explicitly set flags, with the positional query applied last.
func resolveConfig(cmd *cobra.Command, args []string) (config.ResearchConfig, error) {
	cfg, err := loadBase(cmd)
	if err != nil {
		return config.ResearchConfig{}, err
	}

	if err := config.ApplyFlags(&cfg, cmd.Flags()); err != nil {
		return config.ResearchConfig{}, err
	}

	if len(args) > 0 && args[0] != "" {
		if cmd.Flags().Changed("query") {
			return config.ResearchConfig{}, errors.New("query given both as --query and as an argument")
		}
		cfg.Query = args[0]
	}

	if err := cfg.Validate(); err != nil {
		return config.ResearchConfig{}, fmt.Errorf("invalid research config: %w", err)
	}
	return cfg, nil
}

// loadBase reads the base config from --config (a path, or "-" for
// stdin), or from stdin when it is piped — the pipeline-composition path.
// With neither, the defaults are the base.
func loadBase(cmd *cobra.Command) (config.ResearchConfig, error) {
	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return config.ResearchConfig{}, err
	}

	switch {
	case path == "-":
		return config.Decode(cmd.InOrStdin())
	case path != "":
		f, err := os.Open(path)
		if err != nil {
			return config.ResearchConfig{}, fmt.Errorf("open base config: %w", err)
		}
		defer f.Close()
		return config.Decode(f)
	case stdinIsPiped(cmd.InOrStdin()):
		return config.Decode(cmd.InOrStdin())
	default:
		return config.Default(), nil
	}
}

// stdinIsPiped reports whether the command's input is a pipe or file
// rather than a terminal, so interactive runs never block on stdin.
// Unknown reader types are NOT implicit config sources: a library host
// that hands the command tree a live reader it controls must not have
// it silently drained and decoded as YAML.
func stdinIsPiped(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}

// stdinIsTerminal reports whether the command's input is an interactive
// terminal — the only stdin that can approve spend (--plan review).
// Deliberately not the negation of stdinIsPiped: an unknown reader type
// is neither an implicit config source nor a terminal, so it can
// neither smuggle config in nor approve a paid run.
func stdinIsTerminal(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
