package cli

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/version"
)

// TestVersionFlag_ReportsBuildVersion confirms --version reports the same
// build version stamped into the research packet's provenance comment.
func TestVersionFlag_ReportsBuildVersion(t *testing.T) {
	must := require.New(t)

	old := version.Version
	version.Version = "v9.9.9-test"
	defer func() { version.Version = old }()

	stdout, _, err := execute(t, "--version")
	must.NoError(err)
	must.Contains(stdout, "v9.9.9-test")
}

// failingCommand returns a root command whose only behaviour is to return
// err (nil for success), with cobra's own error printing silenced as in
// the real command tree.
func failingCommand(err error) *cobra.Command {
	root := &cobra.Command{
		Use:           "chiron",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(*cobra.Command, []string) error { return err },
	}
	root.SetArgs([]string{})
	return root
}

func TestRunRoot_MapsErrorsToExitCodes(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantCode   int
		wantStderr string
	}{
		{name: "success", err: nil, wantCode: 0, wantStderr: ""},
		{
			name:       "exit error carries its code",
			err:        &ExitError{Code: ExitResearchStopped, Err: errors.New("research stopped")},
			wantCode:   ExitResearchStopped,
			wantStderr: "chiron: research stopped\n",
		},
		{
			name:       "wrapped exit error carries its code",
			err:        fmt.Errorf("outer: %w", &ExitError{Code: ExitBlocked, Err: errors.New("blocked")}),
			wantCode:   ExitBlocked,
			wantStderr: "chiron: outer: blocked\n",
		},
		{
			name:       "plain error is a usage error",
			err:        errors.New("boom"),
			wantCode:   ExitUsage,
			wantStderr: "chiron: boom\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			var stderr bytes.Buffer
			code := runRoot(failingCommand(tt.err), &stderr)

			must.Equal(tt.wantCode, code)
			must.Equal(tt.wantStderr, stderr.String())
		})
	}
}

func TestRunRoot_ScrubsTheFinalErrorLine(t *testing.T) {
	must := require.New(t)

	const key = "AIzaSyD-test0123456789abcdefghijklmnopqr"
	err := &ExitError{
		Code: ExitUsage,
		Err:  fmt.Errorf("upstream rejected the call: x-goog-api-key: %s", key),
	}

	var stderr bytes.Buffer
	code := runRoot(failingCommand(err), &stderr)

	must.Equal(ExitUsage, code)
	must.Equal("chiron: upstream rejected the call: x-goog-api-key: [REDACTED:x-goog-api-key]\n", stderr.String())
	must.NotContains(stderr.String(), key)
}
