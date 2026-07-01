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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rxbynerd/chiron/internal/types"
)

// newInteractionsServer fakes the Interactions API: create returns an
// in-progress interaction; a streaming GET serves an SSE stream with a
// thought summary and a status update (the default --stream path); a
// plain GET returns the final resource with a report, a citation and a
// chart (the --quiet poll path and the post-await re-fetch).
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
		if r.URL.Query().Get("stream") == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("id: e1\nevent: step.delta\ndata: {\"index\":0,\"delta\":{\"type\":\"thought_summary_delta\",\"text\":\"weighing sources\"}}\n\n"))
			flush(w)
			w.Write([]byte("id: e2\nevent: interaction.status_update\ndata: {\"interaction_id\":\"v1_smoke\",\"status\":\"" + finalStatus + "\"}\n\n"))
			flush(w)
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

	// Run events go to stderr as NDJSON, resume handle first among
	// them; the streamed thought summary appears as a delta event
	// between the resume handle and the status change.
	want := []string{"run_started", "interaction_created", "delta", "status_changed", "run_completed", "cost_summary"}
	if kinds := eventKinds(t, stderr); strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Errorf("event kinds = %v, want %v", kinds, want)
	}
	if !strings.Contains(stderr, "v1_smoke") {
		t.Error("stderr events must carry the interaction id — the resume handle")
	}
	if !strings.Contains(stderr, "weighing sources") {
		t.Error("stderr must carry the streamed thought summary")
	}
	if strings.Contains(stdout, "weighing sources") {
		t.Error("thought summaries must never reach stdout — it belongs to the report")
	}
}

// sseStatus serves a minimal SSE stream concluding the interaction with
// one status update — the default --stream await path for fakes whose
// interesting detail lives in the plain-GET resource.
func sseStatus(w http.ResponseWriter, id, status string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Write([]byte("id: e1\nevent: interaction.status_update\ndata: {\"interaction_id\":\"" + id + "\",\"status\":\"" + status + "\"}\n\n"))
	flush(w)
}

// flush pushes buffered SSE frames to the client, so a handler that
// keeps the connection open after writing cannot leave the scanner
// blocked (matches the gemini package's sseWrite helper).
func flush(w http.ResponseWriter) {
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

// eventKinds parses the NDJSON event stream from stderr into its kinds.
func eventKinds(t *testing.T, stderr string) []string {
	t.Helper()
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
	return kinds
}

func TestResearchQuietPolls(t *testing.T) {
	var streamGets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") == "true" {
			streamGets.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"id":"v1_quiet","status":"in_progress"}`))
			return
		}
		w.Write([]byte(`{
			"id": "v1_quiet",
			"status": "completed",
			"steps": [{"type": "model_output", "content": [{"type": "text", "text": "# Smoke report"}]}]
		}`))
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	stdout, stderr, err := execute(t, "research", "--query", "smoke question", "--quiet")
	if err != nil {
		t.Fatalf("research --quiet: %v\nstderr: %s", err, stderr)
	}
	if streamGets.Load() != 0 {
		t.Errorf("--quiet attached %d streams, want none", streamGets.Load())
	}
	if !strings.Contains(stdout, "# Smoke report") {
		t.Errorf("stdout missing report body:\n%s", stdout)
	}
	for _, kind := range eventKinds(t, stderr) {
		if kind == "delta" {
			t.Error("--quiet must not emit delta events")
		}
	}
}

func TestStreamTogglesThinkingSummaries(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"stream default", nil, "auto"},
		{"quiet", []string{"--quiet"}, "none"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu     sync.Mutex
				bodies []map[string]any
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decoding create body: %v", err)
					}
					mu.Lock()
					bodies = append(bodies, body)
					mu.Unlock()
					w.Write([]byte(`{"id":"v1_toggle","status":"in_progress"}`))
					return
				}
				if r.URL.Query().Get("stream") == "true" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Write([]byte("id: e1\nevent: interaction.status_update\ndata: {\"interaction_id\":\"v1_toggle\",\"status\":\"completed\"}\n\n"))
					return
				}
				w.Write([]byte(`{"id":"v1_toggle","status":"completed"}`))
			}))
			defer server.Close()
			t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
			t.Setenv("GEMINI_API_KEY", "test-key")

			args := append([]string{"research", "--query", "q", "-o", "none"}, tt.args...)
			if _, stderr, err := execute(t, args...); err != nil {
				t.Fatalf("research: %v\nstderr: %s", err, stderr)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 1 {
				t.Fatalf("creates = %d, want 1", len(bodies))
			}
			cfg, _ := bodies[0]["agent_config"].(map[string]any)
			if cfg == nil {
				t.Fatal("create request missing agent_config")
			}
			if got := cfg["thinking_summaries"]; got != tt.want {
				t.Errorf("thinking_summaries = %v, want %q — cfg.Stream must toggle the API request", got, tt.want)
			}
		})
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

func TestResearchRequiresActionExitCode(t *testing.T) {
	// requires_action aborts the await with a typed error; the CLI maps
	// it to the failed-research exit code — it is a research outcome,
	// not an infrastructure fault.
	server := newInteractionsServer(t, "requires_action")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	_, _, err := execute(t, "research", "--query", "smoke question", "-o", "none")
	exitErr, ok := errors.AsType[*ExitError](err)
	if !ok {
		t.Fatalf("error = %v, want an ExitError", err)
	}
	if exitErr.Code != ExitResearchFailed {
		t.Errorf("exit code = %d, want %d for requires_action", exitErr.Code, ExitResearchFailed)
	}
	if !strings.Contains(err.Error(), "requires client action") {
		t.Errorf("error %q must carry the requires_action detail", err)
	}
}

func TestResearchRequiresQuery(t *testing.T) {
	if _, _, err := execute(t, "research"); err == nil {
		t.Error("research without a query must fail before any seam is constructed")
	}
}

// TestInProcessAgentsNotYetWired pins the interim composition-root
// contract (V2-RESEARCH-AGENT §4): worker and fleet pass config validation
// but their researchers are not constructed yet, so the run fails with a
// clear typed error at the seam-selection point — never a nil researcher
// or a silent Gemini fallback. The guard fires before any secret is
// resolved or any request is made, so no fake server is needed. A later
// chunk replaces this with real construction.
func TestInProcessAgentsNotYetWired(t *testing.T) {
	for _, tt := range []string{"worker", "fleet"} {
		t.Run(tt, func(t *testing.T) {
			_, _, err := execute(t, "research", "--query", "q", "--agent", tt, "-o", "none")
			if err == nil {
				t.Fatalf("--agent %s must fail until the researcher is wired", tt)
			}
			if !strings.Contains(err.Error(), "not yet wired") {
				t.Errorf("err = %v, want the not-yet-wired message", err)
			}
			// It is an infrastructure/usage error, not a research
			// outcome — no ExitError, so no research exit code.
			if _, ok := errors.AsType[*ExitError](err); ok {
				t.Errorf("a not-yet-wired agent is a usage error, not a research outcome: %v", err)
			}
		})
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

// TestThoughtSummariesAreScrubbed pins C2-SEC-1: --input documents may
// contain credentials the agent quotes back in its thinking, so thought
// deltas pass through secret.Scrub before reaching stderr — the same
// structural guarantee every other output path carries.
func TestThoughtSummariesAreScrubbed(t *testing.T) {
	// A Google-API-key-shaped credential (AIza + 35 key characters).
	const leaked = "AIzaSyA1234567890abcdefghijklmnopqrstuv"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"id":"v1_scrub","status":"in_progress"}`))
			return
		}
		if r.URL.Query().Get("stream") == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("id: e1\nevent: step.delta\ndata: {\"index\":0,\"delta\":{\"type\":\"thought_summary_delta\",\"text\":\"the key " + leaked + " appears in the document\"}}\n\n"))
			flush(w)
			w.Write([]byte("id: e2\nevent: interaction.status_update\ndata: {\"interaction_id\":\"v1_scrub\",\"status\":\"completed\"}\n\n"))
			flush(w)
			return
		}
		w.Write([]byte(`{"id":"v1_scrub","status":"completed"}`))
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	_, stderr, err := execute(t, "research", "--query", "q", "-o", "none")
	if err != nil {
		t.Fatalf("research: %v\nstderr: %s", err, stderr)
	}
	if strings.Contains(stderr, leaked) {
		t.Errorf("the credential reached stderr unscrubbed:\n%s", stderr)
	}
	if !strings.Contains(stderr, "[REDACTED:google-api-key]") {
		t.Errorf("stderr must carry the redaction marker where the credential was:\n%s", stderr)
	}
}
