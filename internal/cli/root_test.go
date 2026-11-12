package cli

import (
	"testing"

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
