package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestStdin_ClassifiesFiles(t *testing.T) {
	tests := []struct {
		name         string
		open         func(t *testing.T) *os.File
		wantPiped    bool
		wantTerminal bool
	}{
		{
			name: "pipe read end",
			open: func(t *testing.T) *os.File {
				must := require.New(t)
				r, w, err := os.Pipe()
				must.NoError(err)
				t.Cleanup(func() { r.Close(); w.Close() })
				return r
			},
			wantPiped: true,
		},
		{
			// A character device counts as a terminal; the plan reader
			// aborts at end of input, so it cannot approve spend.
			name: "dev null",
			open: func(t *testing.T) *os.File {
				must := require.New(t)
				f, err := os.Open(os.DevNull)
				must.NoError(err)
				t.Cleanup(func() { f.Close() })
				return f
			},
			wantTerminal: true,
		},
		{
			name: "regular file",
			open: func(t *testing.T) *os.File {
				must := require.New(t)
				f, err := os.Create(filepath.Join(t.TempDir(), "stdin"))
				must.NoError(err)
				t.Cleanup(func() { f.Close() })
				return f
			},
			wantPiped: true,
		},
		{
			// Stat fails on a closed file, so it is neither.
			name: "closed file",
			open: func(t *testing.T) *os.File {
				must := require.New(t)
				f, err := os.Create(filepath.Join(t.TempDir(), "stdin"))
				must.NoError(err)
				must.NoError(f.Close())
				return f
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			f := tt.open(t)

			must.Equal(tt.wantPiped, stdinIsPiped(f), "stdinIsPiped")
			must.Equal(tt.wantTerminal, stdinIsTerminal(f), "stdinIsTerminal")
		})
	}
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
