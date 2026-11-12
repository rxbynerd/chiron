package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// executeWithStdin runs the command tree with a scripted stdin — the
// pipeline-composition path.
func executeWithStdin(t *testing.T, stdin io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCommand()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(stdin)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errBuf.String(), err
}

// pipedStdin returns a real pipe carrying content — what a shell
// pipeline actually hands the process. Only *os.File pipes count as
// piped config; bare readers are deliberately not decoded as config.
func pipedStdin(t *testing.T, content string) *os.File {
	t.Helper()
	must := require.New(t)
	r, w, err := os.Pipe()
	must.NoError(err)
	t.Cleanup(func() { r.Close() })
	go func() {
		defer w.Close()
		io.WriteString(w, content)
	}()
	return r
}

func TestResearchConfigEmitsResolvedJSON(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	stdout, _, err := execute(t, "research-config", "--agent", "deep-research-max", "--budget", "3.50")
	must.NoError(err)

	var cfg map[string]any
	must.NoError(json.Unmarshal([]byte(stdout), &cfg), "stdout is not one JSON document:\n%s", stdout)
	check.Equal("deep-research-max", cfg["agent"])
	check.InDelta(3.50, cfg["budget_gbp"], 0)
	// Defaults must be resolved into the document, so a later pipeline
	// stage sees the full effective config.
	check.Equal("30m0s", cfg["timeout"])
	check.Equal("secret://GEMINI_API_KEY", cfg["api_key_ref"])
}

func TestResearchConfigPipelineComposition(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	// PROPOSAL §4.3's pipeline, two stages in-process: the first
	// stage's stdout is literally the second's stdin. The second stage
	// only sets --visualise, so the first stage's tier must survive.
	first, _, err := execute(t, "research-config", "--agent", "deep-research-max")
	must.NoError(err, "first stage")

	second, _, err := executeWithStdin(t, pipedStdin(t, first), "research-config", "--visualise")
	must.NoError(err, "second stage")

	var cfg map[string]any
	must.NoError(json.Unmarshal([]byte(second), &cfg), "second stage stdout is not JSON:\n%s", second)
	check.Equal("deep-research-max", cfg["agent"], "the piped base was clobbered")
	check.Equal(true, cfg["visualise"], "the second stage's flag was lost")
}

func TestBudgetGateBlocksBeforeAnyCreate(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Fail("the budget gate must block before any request", "got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	// deep-research estimates £1.58; a £1 cap must block.
	_, _, err := execute(t, "research", "--query", "q", "--budget", "1.00", "-o", "none")
	var exitErr *ExitError
	must.ErrorAs(err, &exitErr)
	check.Equal(ExitBlocked, exitErr.Code)
	// The message must report estimate vs cap so the caller can act.
	must.ErrorContains(err, "£1.58")
	must.ErrorContains(err, "£1.00")
}

func TestBudgetGateAllowsRunUnderCap(t *testing.T) {
	must := require.New(t)
	server := newInteractionsServer(t, "completed")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	_, _, err := execute(t, "research", "--query", "q", "--budget", "2.00", "-o", "none")
	must.NoError(err, "a £2 cap must admit the £1.58 estimate")
}

func TestGetResumesAndEmitsReport(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			check.Fail("get must never create", "got %s %s", r.Method, r.URL.Path)
			return
		}
		if r.URL.Query().Get("stream") == "true" {
			sseStatus(w, "v1_resume", "completed")
			return
		}
		w.Write([]byte(`{
			"id": "v1_resume",
			"agent": "deep-research-preview-04-2026",
			"status": "completed",
			"steps": [{"type": "model_output", "content": [
				{"type": "text", "text": "# Recovered report"}
			]}]
		}`))
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	stdout, stderr, err := execute(t, "get", "v1_resume")
	must.NoError(err, "stderr: %s", stderr)
	check.Contains(stdout, "# Recovered report")
	// The tool set of a resumed interaction is unknowable (the API does
	// not echo it); the front matter must not claim one.
	check.NotContains(stdout, "tools:", "front matter must not claim a tool set for a resumed interaction")
	// Likewise the query: the interaction's user_input step carries the
	// TEMPLATED prompt, not the raw question, so the adapter records no
	// query for resumed interactions and the front matter omits the
	// field rather than rendering the prompt boilerplate.
	check.NotContains(stdout, "query:", "front matter must not claim a query for a resumed interaction")
}

func TestFollowUpChainsModelInteraction(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var createBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if !check.NoError(json.NewDecoder(r.Body).Decode(&createBody), "decoding create body") {
				return
			}
			w.Write([]byte(`{"id":"v1_followup","status":"in_progress"}`))
			return
		}
		if r.URL.Query().Get("stream") == "true" {
			sseStatus(w, "v1_followup", "completed")
			return
		}
		w.Write([]byte(`{
			"id": "v1_followup",
			"model": "gemini-3.1-pro-preview",
			"status": "completed",
			"steps": [{"type": "model_output", "content": [
				{"type": "text", "text": "The answer is 42."}
			]}]
		}`))
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	stdout, stderr, err := execute(t, "follow-up", "v1_research", "--query", "What is the answer?")
	must.NoError(err, "stderr: %s", stderr)
	check.Contains(stdout, "The answer is 42.")

	check.Equal("gemini-3.1-pro-preview", createBody["model"], "model must be the documented default")
	check.Equal("v1_research", createBody["previous_interaction_id"])
	_, hasAgent := createBody["agent"]
	check.False(hasAgent, "a follow-up create must not carry agent")
}

func TestFollowUpRequiresQuery(t *testing.T) {
	must := require.New(t)

	_, _, err := execute(t, "follow-up", "v1_research")
	must.Error(err, "follow-up without a query must fail before any seam is constructed")
}

// newPlanFlowServer fakes the full --plan sequence: the first create is
// the plan round (collaborative_planning true), the second the approval
// (collaborative_planning false, chained to the plan). Create bodies
// are captured in order.
func newPlanFlowServer(t *testing.T, bodies *[]map[string]any) *httptest.Server {
	t.Helper()
	check := assert.New(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]any
			if !check.NoError(json.NewDecoder(r.Body).Decode(&body), "decoding create body") {
				return
			}
			*bodies = append(*bodies, body)
			if len(*bodies) == 1 {
				w.Write([]byte(`{"id":"v1_plan","status":"in_progress"}`))
			} else {
				w.Write([]byte(`{"id":"v1_research","status":"in_progress"}`))
			}
			return
		}
		if r.URL.Query().Get("stream") == "true" {
			// Only the research run streams; the planner polls.
			sseStatus(w, "v1_research", "completed")
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v1_plan") {
			w.Write([]byte(`{
				"id": "v1_plan",
				"status": "completed",
				"steps": [{"type": "model_output", "content": [
					{"type": "text", "text": "1. Survey the vendors.\n2. Compare datasheets."}
				]}]
			}`))
			return
		}
		w.Write([]byte(`{
			"id": "v1_research",
			"agent": "deep-research-preview-04-2026",
			"status": "completed",
			"steps": [{"type": "model_output", "content": [
				{"type": "text", "text": "# Planned report"}
			]}]
		}`))
	}))
}

func TestResearchPlanAcceptPlanFlow(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var bodies []map[string]any
	server := newPlanFlowServer(t, &bodies)
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	stdout, stderr, err := execute(t, "research", "--query", "smoke question", "--plan", "--accept-plan")
	must.NoError(err, "stderr: %s", stderr)

	// The report on stdout, the plan on stderr — a piped report stays clean.
	check.Contains(stdout, "# Planned report")
	check.NotContains(stdout, "Survey the vendors", "the plan must not leak onto stdout")
	check.Contains(stderr, "Survey the vendors", "the plan must be rendered on stderr")

	must.Len(bodies, 2, "want plan + approval creates")
	planCfg, _ := bodies[0]["agent_config"].(map[string]any)
	check.Equal(true, planCfg["collaborative_planning"], "plan create agent_config")
	approveCfg, _ := bodies[1]["agent_config"].(map[string]any)
	check.Equal(false, approveCfg["collaborative_planning"], "approval agent_config")
	check.Equal("v1_plan", bodies[1]["previous_interaction_id"], "approval must chain to the accepted plan")
}

func TestResearchPlanNonInteractiveNeedsAcceptPlan(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Fail("no request must be sent when the plan cannot be reviewed", "got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	// The test stdin is not a terminal, so --plan alone must abort.
	_, _, err := execute(t, "research", "--query", "q", "--plan", "-o", "none")
	must.ErrorContains(err, "--accept-plan")
}

func TestResearchBudgetGatesThePlanPhaseToo(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Fail("plan rounds spend; the budget gate must block them", "got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	_, _, err := execute(t, "research", "--query", "q", "--plan", "--accept-plan", "--budget", "1.00", "-o", "none")
	var exitErr *ExitError
	must.ErrorAs(err, &exitErr)
	must.Equal(ExitBlocked, exitErr.Code, "want ExitBlocked before any plan round")
}
