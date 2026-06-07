package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/types"
)

// newInteractionsServer fakes the Interactions API: create returns an
// in-progress interaction, the first GET completes it with a report,
// a citation and a chart.
func newInteractionsServer(t *testing.T, finalStatus string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") == "" {
			t.Error("request without the API key header")
		}
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"id":"v1_smoke","status":"in_progress","created":"2026-06-07T12:00:00Z"}`))
			return
		}
		w.Write([]byte(`{
			"id": "v1_smoke",
			"agent": "deep-research-preview-04-2026",
			"status": "` + finalStatus + `",
			"created": "2026-06-07T12:00:00Z",
			"updated": "2026-06-07T12:20:00Z",
			"steps": [
				{"type": "model_output", "content": [
					{"type": "image", "mime_type": "image/png", "data": "cG5nLWJ5dGVz"},
					{"type": "text", "text": "# Smoke report\n\nFindings.", "annotations": [
						{"type": "url_citation", "url": "https://example.org/src", "title": "Source"}
					]}
				]}
			],
			"usage": {
				"total_input_tokens": 1000,
				"total_output_tokens": 200,
				"grounding_tool_count": [{"type": "google_search", "count": 5}]
			}
		}`))
	}))
}

// execute runs the chiron command tree as a user would, capturing
// stdout and stderr.
func execute(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCommand()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errBuf.String(), err
}

func TestResearchEndToEndText(t *testing.T) {
	server := newInteractionsServer(t, "completed")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	stdout, stderr, err := execute(t, "research", "--query", "smoke question", "-o", "text")
	if err != nil {
		t.Fatalf("research: %v\nstderr: %s", err, stderr)
	}

	// The report lands on stdout: front matter (with the tool set and
	// sources) and body, clean enough to pipe into a file.
	if !strings.Contains(stdout, "# Smoke report") {
		t.Errorf("stdout missing report body:\n%s", stdout)
	}
	if !strings.Contains(stdout, "query: smoke question") {
		t.Errorf("stdout missing front-matter query:\n%s", stdout)
	}
	if !strings.Contains(stdout, "tools: [google_search, url_context, code_execution]") {
		t.Errorf("stdout missing the tool set:\n%s", stdout)
	}
	if !strings.Contains(stdout, "https://example.org/src") {
		t.Errorf("stdout missing the cited source:\n%s", stdout)
	}

	// Run events go to stderr as NDJSON, resume handle first among them.
	var kinds []string
	for line := range strings.Lines(stderr) {
		var ev struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stderr is not NDJSON: %q: %v", line, err)
		}
		kinds = append(kinds, ev.Kind)
	}
	want := []string{"run_started", "interaction_created", "status_changed", "run_completed", "cost_summary"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Errorf("event kinds = %v, want %v", kinds, want)
	}
	if !strings.Contains(stderr, "v1_smoke") {
		t.Error("stderr events must carry the interaction id — the resume handle")
	}
}

func TestResearchEndToEndJSON(t *testing.T) {
	server := newInteractionsServer(t, "completed")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	stdout, stderr, err := execute(t, "research", "smoke question", "-o", "json")
	if err != nil {
		t.Fatalf("research: %v\nstderr: %s", err, stderr)
	}

	var result types.RunResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not one RunResult JSON document: %v\n%s", err, stdout)
	}
	if result.InteractionID != "v1_smoke" || result.Status != types.StatusCompleted {
		t.Errorf("result = %s/%s", result.InteractionID, result.Status)
	}
	if result.Usage.SearchCount != 5 || result.Usage.EstimatedCostGBP == 0 {
		t.Errorf("usage = %+v, want search count and cost estimate", result.Usage)
	}
	if result.Report == nil || len(result.Report.Assets) != 1 {
		t.Errorf("report = %+v, want the chart asset inline", result.Report)
	}
}

func TestResearchEndToEndFileSink(t *testing.T) {
	server := newInteractionsServer(t, "completed")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	dir := t.TempDir()
	out := filepath.Join(dir, "report.md")
	stdout, stderr, err := execute(t, "research", "--query", "smoke question", "--out", out)
	if err != nil {
		t.Fatalf("research: %v\nstderr: %s", err, stderr)
	}
	if stdout != "" {
		t.Errorf("with --out and -o text the report must not be duplicated on stdout, got:\n%s", stdout)
	}

	report, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("report not written: %v", err)
	}
	if !strings.Contains(string(report), "# Smoke report") {
		t.Errorf("report = %s", report)
	}
	chart, err := os.ReadFile(filepath.Join(dir, "chart-1.png"))
	if err != nil {
		t.Fatalf("chart asset not written next to the report: %v", err)
	}
	if string(chart) != "png-bytes" {
		t.Errorf("chart bytes = %q", chart)
	}
}

func TestResearchFailedExitCode(t *testing.T) {
	server := newInteractionsServer(t, "failed")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	_, _, err := execute(t, "research", "--query", "smoke question", "-o", "none")
	exitErr, ok := errors.AsType[*ExitError](err)
	if !ok {
		t.Fatalf("error = %v, want an ExitError", err)
	}
	if exitErr.Code != ExitResearchFailed {
		t.Errorf("exit code = %d, want %d for a failed task", exitErr.Code, ExitResearchFailed)
	}
}

func TestResearchBudgetExceededExitCode(t *testing.T) {
	server := newInteractionsServer(t, "budget_exceeded")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	_, _, err := execute(t, "research", "--query", "smoke question", "-o", "none")
	exitErr, ok := errors.AsType[*ExitError](err)
	if !ok {
		t.Fatalf("error = %v, want an ExitError", err)
	}
	if exitErr.Code != ExitResearchStopped {
		t.Errorf("exit code = %d, want %d for budget_exceeded", exitErr.Code, ExitResearchStopped)
	}
}

func TestResearchRequiresQuery(t *testing.T) {
	if _, _, err := execute(t, "research"); err == nil {
		t.Error("research without a query must fail before any seam is constructed")
	}
}

func TestResearchUnresolvableSecret(t *testing.T) {
	server := newInteractionsServer(t, "completed")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	// An empty value is as unresolvable as an unset one, and t.Setenv
	// restores the caller's environment afterwards.
	t.Setenv("GEMINI_API_KEY", "")

	_, _, err := execute(t, "research", "--query", "q")
	if err == nil {
		t.Fatal("an unresolvable secret must fail the run")
	}
	if _, ok := errors.AsType[*ExitError](err); ok {
		t.Error("secret failures are usage errors, not research outcomes")
	}
}
