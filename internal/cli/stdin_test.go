package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Unknown reader types are neither implicit config sources nor terminals.
// Only a real pipe/file *os.File stdin carries config; only a real terminal
// can approve spend.

func TestStdinIsPipedRejectsNonFileReaders(t *testing.T) {
	must := require.New(t)
	must.False(stdinIsPiped(strings.NewReader("")), "a non-file reader must not be treated as piped config")
}

func TestStdinIsTerminalRejectsNonFileReaders(t *testing.T) {
	must := require.New(t)
	must.False(stdinIsTerminal(strings.NewReader("")), "a non-file reader must never count as a terminal that can approve spend")
}

func TestNonFileStdinIsNotDecodedAsConfig(t *testing.T) {
	must := require.New(t)
	// The reader holds invalid YAML. The run must succeed on the defaults
	// and the explicit flags alone, so the reader is never decoded as config.
	stdout, _, err := executeWithStdin(t, strings.NewReader(":::not yaml:::"),
		"research-config", "--agent", "deep-research-max")
	must.NoError(err, "a non-file stdin must not be decoded as config")

	var cfg map[string]any
	must.NoError(json.Unmarshal([]byte(stdout), &cfg), "stdout is not one JSON document:\n%s", stdout)
	must.Equal("deep-research-max", cfg["agent"], "agent should come from the explicit flag")
}
