package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// C1-CODE-4: unknown reader types are neither implicit config sources
// nor terminals. Only a real pipe/file *os.File stdin carries config;
// only a real terminal can approve spend.

func TestStdinIsPipedRejectsNonFileReaders(t *testing.T) {
	if stdinIsPiped(strings.NewReader("")) {
		t.Error("a non-file reader must not be treated as piped config")
	}
}

func TestStdinIsTerminalRejectsNonFileReaders(t *testing.T) {
	if stdinIsTerminal(strings.NewReader("")) {
		t.Error("a non-file reader must never count as a terminal that can approve spend")
	}
}

func TestNonFileStdinIsNotDecodedAsConfig(t *testing.T) {
	// The reader's content is deliberately invalid YAML: had loadBase
	// drained it as an implicit base config, the command would fail.
	// With explicit flags only, the run must complete on the defaults
	// plus those flags, leaving the host's reader uninterpreted.
	stdout, _, err := executeWithStdin(t, strings.NewReader(":::not yaml:::"),
		"research-config", "--agent", "deep-research-max")
	if err != nil {
		t.Fatalf("a non-file stdin must not be decoded as config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if cfg["agent"] != "deep-research-max" {
		t.Errorf("agent = %v, want the explicit flag value", cfg["agent"])
	}
}
