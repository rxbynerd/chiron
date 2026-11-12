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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/types"
)

// newInteractionsServer fakes the Interactions API: create returns an
// in-progress interaction; a streaming GET serves an SSE stream with a
// thought summary and a status update (the default --stream path); a
// plain GET returns the final resource with a report, a citation and a
// chart (the --quiet poll path and the post-await re-fetch).
func newInteractionsServer(t *testing.T, finalStatus string) *httptest.Server {
	t.Helper()
	check := assert.New(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.NotEmpty(r.Header.Get("x-goog-api-key"), "request without the API key header")
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
	must := require.New(t)
	check := assert.New(t)
	server := newInteractionsServer(t, "completed")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	stdout, stderr, err := execute(t, "research", "--query", "smoke question", "-o", "text")
	must.NoError(err, "research stderr:\n%s", stderr)

	// The report lands on stdout: front matter (with the tool set and
	// sources) and body, clean enough to pipe into a file.
	check.Contains(stdout, "# Smoke report", "stdout missing report body")
	check.Contains(stdout, "query: smoke question", "stdout missing front-matter query")
	check.Contains(stdout, "tools: [google_search, url_context, code_execution]", "stdout missing the tool set")
	check.Contains(stdout, "https://example.org/src", "stdout missing the cited source")

	// Run events go to stderr as NDJSON, resume handle first among
	// them; the streamed thought summary appears as a delta event
	// between the resume handle and the status change.
	want := []string{"run_started", "interaction_created", "delta", "status_changed", "run_completed", "cost_summary"}
	check.Equal(want, eventKinds(t, stderr), "event kinds")
	check.Contains(stderr, "v1_smoke", "stderr events must carry the interaction id — the resume handle")
	check.Contains(stderr, "weighing sources", "stderr must carry the streamed thought summary")
	check.NotContains(stdout, "weighing sources", "thought summaries must never reach stdout — it belongs to the report")
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
	must := require.New(t)
	var kinds []string
	for line := range strings.Lines(stderr) {
		var ev struct {
			Kind string `json:"kind"`
		}
		must.NoError(json.Unmarshal([]byte(line), &ev), "stderr is not NDJSON: %q", line)
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}

func TestResearchQuietPolls(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err, "research --quiet stderr:\n%s", stderr)
	check.Zero(streamGets.Load(), "--quiet must not attach a stream")
	check.Contains(stdout, "# Smoke report", "stdout missing report body")
	check.NotContains(eventKinds(t, stderr), "delta", "--quiet must not emit delta events")
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
			must := require.New(t)
			check := assert.New(t)
			var (
				mu     sync.Mutex
				bodies []map[string]any
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					var body map[string]any
					check.NoError(json.NewDecoder(r.Body).Decode(&body), "decoding create body")
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
			_, stderr, err := execute(t, args...)
			must.NoError(err, "research stderr:\n%s", stderr)

			mu.Lock()
			defer mu.Unlock()
			must.Len(bodies, 1, "create requests")
			cfg, _ := bodies[0]["agent_config"].(map[string]any)
			must.NotNil(cfg, "create request missing agent_config")
			must.Equal(tt.want, cfg["thinking_summaries"], "cfg.Stream must toggle the API request")
		})
	}
}

func TestResearchEndToEndJSON(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	server := newInteractionsServer(t, "completed")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	stdout, stderr, err := execute(t, "research", "smoke question", "-o", "json")
	must.NoError(err, "research stderr:\n%s", stderr)

	var result types.RunResult
	must.NoError(json.Unmarshal([]byte(stdout), &result), "stdout is not one RunResult JSON document:\n%s", stdout)
	check.Equal("v1_smoke", result.InteractionID, "interaction id")
	check.Equal(types.StatusCompleted, result.Status, "status")
	check.Equal(5, result.Usage.SearchCount, "search count from grounding")
	check.NotZero(result.Usage.EstimatedCostGBP, "cost estimate")
	must.NotNil(result.Report, "report must be inline")
	check.Len(result.Report.Assets, 1, "chart asset inline")
}

func TestResearchEndToEndFileSink(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	server := newInteractionsServer(t, "completed")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	dir := t.TempDir()
	out := filepath.Join(dir, "report.md")
	stdout, stderr, err := execute(t, "research", "--query", "smoke question", "--out", out)
	must.NoError(err, "research stderr:\n%s", stderr)
	check.Empty(stdout, "with --out and -o text the report must not be duplicated on stdout")

	report, err := os.ReadFile(out)
	must.NoError(err, "report not written")
	check.Contains(string(report), "# Smoke report", "report body")

	chart, err := os.ReadFile(filepath.Join(dir, "chart-1.png"))
	must.NoError(err, "chart asset not written next to the report")
	check.Equal("png-bytes", string(chart), "chart bytes")
}

func TestResearchFailedExitCode(t *testing.T) {
	must := require.New(t)
	server := newInteractionsServer(t, "failed")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	_, _, err := execute(t, "research", "--query", "smoke question", "-o", "none")
	var exitErr *ExitError
	must.ErrorAs(err, &exitErr, "want an ExitError")
	must.Equal(ExitResearchFailed, exitErr.Code, "exit code for a failed task")
}

func TestResearchBudgetExceededExitCode(t *testing.T) {
	must := require.New(t)
	server := newInteractionsServer(t, "budget_exceeded")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	_, _, err := execute(t, "research", "--query", "smoke question", "-o", "none")
	var exitErr *ExitError
	must.ErrorAs(err, &exitErr, "want an ExitError")
	must.Equal(ExitResearchStopped, exitErr.Code, "exit code for budget_exceeded")
}

func TestResearchRequiresActionExitCode(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// requires_action aborts the await with a typed error; the CLI maps
	// it to the failed-research exit code — it is a research outcome,
	// not an infrastructure fault.
	server := newInteractionsServer(t, "requires_action")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	_, _, err := execute(t, "research", "--query", "smoke question", "-o", "none")
	var exitErr *ExitError
	must.ErrorAs(err, &exitErr, "want an ExitError")
	check.Equal(ExitResearchFailed, exitErr.Code, "exit code for requires_action")
	check.ErrorContains(err, "requires client action", "error must carry the requires_action detail")
}

func TestResearchRequiresQuery(t *testing.T) {
	must := require.New(t)
	_, _, err := execute(t, "research")
	must.Error(err, "research without a query must fail before any seam is constructed")
}

// TestWorkerAgentIsWired proves --agent worker constructs a real in-process
// worker researcher at the composition root: the run reaches and completes the
// worker loop through the unchanged run core. The model searches once and
// answers citing the search result, so the run needs a loopback model endpoint
// and a fake search MCP; the report lands on stdout with the worker's front
// matter. The fuller golden-report assertion lives in the fleet package.
func TestWorkerAgentIsWired(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop", Usage: model.Usage{InputTokens: 20, OutputTokens: 6, TotalTokens: 26}},
		modeltest.FakeReply{
			Content:      `{"action":"final","answer":"# Worker answer\n\nThe sky is blue.","citations":[{"url":"https://example.org/sky","title":"Sky"}]}`,
			FinishReason: "stop",
			Usage:        model.Usage{InputTokens: 40, OutputTokens: 12, TotalTokens: 52},
		},
	)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "Sky", URL: "https://example.org/sky"}})
	defer searchSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")

	stdout, stderr, err := execute(t,
		"research", "--query", "why is the sky blue",
		"--agent", "worker", "-o", "text",
		"--fleet-model-endpoint", modelSrv.URL(),
		"--fleet-model-name", "test-model",
		"--fleet-model-key-ref", "secret://MODEL_KEY",
		"--fleet-search-endpoint", searchSrv.URL(),
	)
	must.NoError(err, "research --agent worker stderr:\n%s", stderr)
	check.Contains(stdout, "# Worker answer", "stdout missing the worker's report body")
	check.Contains(stdout, "agent: worker", "stdout missing the worker agent in front matter")
	check.Contains(stdout, "https://example.org/sky", "stdout missing the cited source")
}

func TestResearchUnresolvableSecret(t *testing.T) {
	must := require.New(t)
	server := newInteractionsServer(t, "completed")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	// An empty value is as unresolvable as an unset one, and t.Setenv
	// restores the caller's environment afterwards.
	t.Setenv("GEMINI_API_KEY", "")

	_, _, err := execute(t, "research", "--query", "q")
	must.Error(err, "an unresolvable secret must fail the run")
	_, isExitErr := errors.AsType[*ExitError](err)
	must.False(isExitErr, "secret failures are usage errors, not research outcomes")
}

// TestResearch_ThoughtSummaries_AreScrubbed covers credentials that --input
// documents may contain and the agent quotes back in its thinking: thought
// deltas pass through secret.Scrub before reaching stderr, the same guarantee
// every other output path carries.
func TestResearch_ThoughtSummaries_AreScrubbed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err, "research stderr:\n%s", stderr)
	check.NotContains(stderr, leaked, "the credential reached stderr unscrubbed")
	check.Contains(stderr, "[REDACTED:google-api-key]", "stderr must carry the redaction marker where the credential was")
}
