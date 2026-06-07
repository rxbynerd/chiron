// Package cli defines the chiron command tree and binds flags onto the
// declarative ResearchConfig. Commands stay thin: they resolve configuration
// and hand off to the run core; no research behaviour lives here.
package cli

import (
	"github.com/spf13/cobra"
)

// NewRootCommand builds the chiron command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "chiron",
		Short: "Chiron is the Equestrianism suite's researcher",
		Long: `Chiron investigates a topic with a long-running research agent and
returns a cited, structured Markdown report. It is research-only: it never
mutates a workspace, runs no shell, and applies no edits.`,
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
