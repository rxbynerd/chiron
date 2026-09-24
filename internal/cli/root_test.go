package cli

import (
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/version"
)

// TestVersionFlag confirms --version reports the same build version
// stamped into the research packet's provenance comment.
func TestVersionFlag(t *testing.T) {
	old := version.Version
	version.Version = "v9.9.9-test"
	defer func() { version.Version = old }()

	stdout, _, err := execute(t, "--version")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(stdout, "v9.9.9-test") {
		t.Errorf("stdout missing version %q:\n%s", "v9.9.9-test", stdout)
	}
}
