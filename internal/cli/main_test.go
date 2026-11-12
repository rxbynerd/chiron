package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// ambientEnvPrefixes and ambientEnvNames are the environment variables the
// package's tests must never inherit from the developer's shell: any
// variable with one of the prefixes, plus the credential names that do not
// share a prefix. A test that needs a value sets it with t.Setenv.
var (
	ambientEnvPrefixes = []string{"OTEL_", "CHIRON_"}
	ambientEnvNames    = []string{"GEMINI_API_KEY", "EXA_API_KEY", "LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY"}
)

// childRoleVars are the variables a test sets to re-run the test binary as
// a child process that plays a role. The parent builds such a child's
// environment, so TestMain leaves it as it was given.
var childRoleVars = []string{headerChildRole, executeChildRole}

// clearedByTestMain reports whether TestMain removes the named variable
// from the environment.
func clearedByTestMain(name string) bool {
	if slices.Contains(ambientEnvNames, name) {
		return true
	}
	for _, prefix := range ambientEnvPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func runningAsChild() bool {
	for _, name := range childRoleVars {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

func TestMain(m *testing.M) {
	if !runningAsChild() {
		for _, kv := range os.Environ() {
			name, _, _ := strings.Cut(kv, "=")
			if !clearedByTestMain(name) {
				continue
			}
			if err := os.Unsetenv(name); err != nil {
				fmt.Fprintf(os.Stderr, "unsetting %s: %v\n", name, err)
				os.Exit(1)
			}
		}
	}
	os.Exit(m.Run())
}
