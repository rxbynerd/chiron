package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The exit codes are the contract scripts parse, so these tests assert
// the integer the process would exit with, through the real command tree.

func TestExitCode_ResearchOutcomes(t *testing.T) {
	tests := []struct {
		status   string
		wantCode int
	}{
		{"completed", 0},
		{"failed", ExitResearchFailed},
		{"requires_action", ExitResearchFailed},
		{"cancelled", ExitResearchStopped},
		{"budget_exceeded", ExitResearchStopped},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			must := require.New(t)
			server := newInteractionsServer(t, tt.status)
			defer server.Close()
			t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
			t.Setenv("GEMINI_API_KEY", "test-key")

			code, _, stderr := executeCode(t, strings.NewReader(""),
				"research", "--query", "smoke question", "-o", "none")

			must.Equal(tt.wantCode, code, "stderr:\n%s", stderr)
		})
	}
}

func TestExitCode_UnknownFlagIsUsageError(t *testing.T) {
	must := require.New(t)

	code, _, stderr := executeCode(t, strings.NewReader(""), "research", "--no-such-flag")

	must.Equal(ExitUsage, code)
	must.True(strings.HasPrefix(stderr, "chiron: "), "stderr: %q", stderr)
}

func TestExitCode_BudgetBlockIsBlocked(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Fail("the budget gate must block before any request", "got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	code, _, stderr := executeCode(t, strings.NewReader(""),
		"research", "--query", "q", "--budget", "1.00", "-o", "none")

	must.Equal(ExitBlocked, code)
	must.True(strings.HasPrefix(stderr, "chiron: research blocked before any spend"), "stderr: %q", stderr)
}

func TestExitCode_DeclinedPlanIsBlocked(t *testing.T) {
	must := require.New(t)

	var bodies []map[string]any
	server := newPlanFlowServer(t, &bodies)
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	// /dev/null is a character device, so it passes for a terminal; the
	// plan reader meets end of input and aborts, which declines the plan.
	devNull, err := os.Open(os.DevNull)
	must.NoError(err)
	defer devNull.Close()

	code, _, stderr := executeCode(t, devNull,
		"research", "--query", "smoke question", "--plan", "-o", "none")

	must.Equal(ExitBlocked, code, "stderr:\n%s", stderr)
	// Planning itself spends, so one create precedes the review, but the
	// declined plan must never start the research run chained from it.
	must.Len(bodies, 1, "want the planning create only")
	must.NotContains(bodies[0], "previous_interaction_id")
}

// executeChildRole marks the child process of
// TestExecute_ExitsWithBlockedStatusThroughProcess.
const executeChildRole = "CHIRON_TEST_EXECUTE_CHILD"

// TestExecute_ExitsWithBlockedStatusThroughProcess runs the real entry
// point in a child process, the only way to observe the integer Execute
// hands to os.Exit and the line it prints on the process's own stderr.
func TestExecute_ExitsWithBlockedStatusThroughProcess(t *testing.T) {
	if os.Getenv(executeChildRole) == "1" {
		os.Args = []string{"chiron", "research", "--query", "q", "--budget", "1.00", "-o", "none"}
		os.Exit(Execute())
	}

	must := require.New(t)
	check := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Fail("the budget gate must block before any request", "got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestExecute_ExitsWithBlockedStatusThroughProcess$")
	// An explicit environment, so nothing ambient reaches the child.
	cmd.Env = []string{
		executeChildRole + "=1",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"CHIRON_GEMINI_BASE_URL=" + server.URL,
		"GEMINI_API_KEY=test-key",
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	var exitErr *exec.ExitError
	must.ErrorAs(err, &exitErr, "child stdout: %s\nstderr: %s", stdout.String(), stderr.String())
	must.Equal(ExitBlocked, exitErr.ExitCode(), "stderr:\n%s", stderr.String())
	must.True(strings.HasPrefix(stderr.String(), "chiron: research blocked before any spend"), "stderr: %q", stderr.String())
}
