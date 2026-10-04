// Package cli defines the chiron command tree and binds flags onto the
// declarative ResearchConfig. Commands stay thin: they resolve configuration
// and hand off to the run core; no research behaviour lives here.
package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/chiron/internal/secret"
	"github.com/rxbynerd/chiron/internal/version"
)

// Execute runs the chiron command tree and returns the process exit
// code for main to pass to os.Exit: 0 on success, an ExitError's code
// for distinct research outcomes, and ExitUsage for everything else
// (see the research command's help for the contract).
func Execute() int {
	if err := NewRootCommand().Execute(); err != nil {
		// Scrubbed like every other output path: API error messages
		// must not become the one channel a credential can transit.
		fmt.Fprintln(os.Stderr, "chiron:", secret.Scrub(err.Error()))
		if exitErr, ok := errors.AsType[*ExitError](err); ok {
			return exitErr.Code
		}
		return ExitUsage
	}
	return 0
}

// NewRootCommand builds the chiron command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "chiron",
		Short: "Chiron is the Equestrianism suite's researcher",
		Long: `Chiron investigates a topic with a long-running research agent and
returns a cited, structured Markdown report. It is research-only: it never
mutates a workspace, runs no shell, and applies no edits.`,
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newResearchCommand(),
		newResearchConfigCommand(),
		newGetCommand(),
		newFollowUpCommand(),
	)

	return root
}
