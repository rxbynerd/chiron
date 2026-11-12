package cli

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/config"
)

// TestHelpText_NamesEveryFleetEndpoint: the research and research-config
// help name each credential-bearing fleet endpoint's flag and environment
// variable, so a new FleetEndpoints entry cannot be left out of either.
func TestHelpText_NamesEveryFleetEndpoint(t *testing.T) {
	commands := []struct {
		name string
		cmd  *cobra.Command
	}{
		{"research", newResearchCommand(nil)},
		{"research-config", newResearchConfigCommand()},
	}
	for _, tt := range commands {
		for _, e := range config.FleetEndpoints(&config.FleetConfig{}) {
			t.Run(tt.name+"/"+e.Flag, func(t *testing.T) {
				must := require.New(t)
				must.Contains(tt.cmd.Long, "--"+e.Flag)
				must.Contains(tt.cmd.Long, e.Env)
			})
		}
	}
}

// TestResearchConfigHelp_NamesSendObjectiveRefusal: research-config refuses
// --fleet-search-send-objective, and its help says so.
func TestResearchConfigHelp_NamesSendObjectiveRefusal(t *testing.T) {
	must := require.New(t)
	must.Contains(newResearchConfigCommand().Long, "--"+config.FlagSearchSendObjective)
}
