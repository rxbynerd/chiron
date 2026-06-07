package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/chiron/internal/config"
)

// errNotImplemented marks command bodies that later milestones fill in
// (PROPOSAL §9). Flag parsing and config resolution already work, so a
// stubbed command still validates its inputs.
func errNotImplemented(cmd *cobra.Command) error {
	return fmt.Errorf("chiron %s is not implemented yet (see docs/PROPOSAL.md §9)", cmd.Name())
}

func newResearchCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "research [query]",
		Short: "Start a research task, await it, and emit the report",
		Long: `Start a research task, await the long-running agent (streaming or
polling), retrieve the result, and emit a cited Markdown report. The main path.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd, args)
			if err != nil {
				return err
			}
			_ = cfg
			return errNotImplemented(cmd)
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
			_ = cfg
			return errNotImplemented(cmd)
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
local state.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd, nil)
			if err != nil {
				return err
			}
			_ = cfg
			return errNotImplemented(cmd)
		},
	}
	addResearchFlags(cmd)
	return cmd
}

func newFollowUpCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "follow-up <interaction-id>",
		Short: "Ask a follow-up question against a completed interaction",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd, nil)
			if err != nil {
				return err
			}
			_ = cfg
			return errNotImplemented(cmd)
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
func stdinIsPiped(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		// Non-file readers (tests, future embedding) are explicit inputs.
		return true
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}
