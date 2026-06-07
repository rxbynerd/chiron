package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
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
// piped config; bare readers deliberately do not (C1-CODE-4).
func pipedStdin(t *testing.T, content string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	go func() {
		defer w.Close()
		io.WriteString(w, content)
	}()
	return r
}

func TestResearchConfigEmitsResolvedJSON(t *testing.T) {
	stdout, _, err := execute(t, "research-config", "--agent", "deep-research-max", "--budget", "3.50")
	if err != nil {
		t.Fatalf("research-config: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if cfg["agent"] != "deep-research-max" {
		t.Errorf("agent = %v", cfg["agent"])
	}
	if cfg["budget_gbp"] != 3.50 {
		t.Errorf("budget_gbp = %v", cfg["budget_gbp"])
	}
	// Defaults must be resolved into the document, so a later pipeline
	// stage sees the full effective config.
	if cfg["timeout"] != "30m0s" {
		t.Errorf("timeout = %v, want the resolved default", cfg["timeout"])
	}
	if cfg["api_key_ref"] != "secret://GEMINI_API_KEY" {
		t.Errorf("api_key_ref = %v", cfg["api_key_ref"])
	}
}

func TestResearchConfigPipelineComposition(t *testing.T) {
	// PROPOSAL §4.3's pipeline, two stages in-process: the first
	// stage's stdout is literally the second's stdin. The second stage
	// only sets --visualise, so the first stage's tier must survive.
	first, _, err := execute(t, "research-config", "--agent", "deep-research-max")
	if err != nil {
		t.Fatalf("first stage: %v", err)
	}

	second, _, err := executeWithStdin(t, pipedStdin(t, first), "research-config", "--visualise")
	if err != nil {
		t.Fatalf("second stage: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(second), &cfg); err != nil {
		t.Fatalf("second stage stdout is not JSON: %v\n%s", err, second)
	}
	if cfg["agent"] != "deep-research-max" {
		t.Errorf("agent = %v — the piped base was clobbered", cfg["agent"])
	}
	if cfg["visualise"] != true {
		t.Errorf("visualise = %v — the second stage's flag was lost", cfg["visualise"])
	}
}

func TestBudgetGateBlocksBeforeAnyCreate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the budget gate must block before any request, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	// deep-research estimates £1.58; a £1 cap must block.
	_, _, err := execute(t, "research", "--query", "q", "--budget", "1.00", "-o", "none")
	exitErr, ok := errors.AsType[*ExitError](err)
	if !ok {
		t.Fatalf("error = %v, want an ExitError", err)
	}
	if exitErr.Code != ExitBlocked {
		t.Errorf("exit code = %d, want %d for a budget block", exitErr.Code, ExitBlocked)
	}
	// The message must report estimate vs cap so the caller can act.
	if !strings.Contains(err.Error(), "£1.58") || !strings.Contains(err.Error(), "£1.00") {
		t.Errorf("error %q must carry estimate and cap", err)
	}
}

func TestBudgetGateAllowsRunUnderCap(t *testing.T) {
	server := newInteractionsServer(t, "completed")
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	if _, _, err := execute(t, "research", "--query", "q", "--budget", "2.00", "-o", "none"); err != nil {
		t.Errorf("a £2 cap must admit the £1.58 estimate: %v", err)
	}
}

func TestGetResumesAndEmitsReport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("get must never create: got %s %s", r.Method, r.URL.Path)
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
	if err != nil {
		t.Fatalf("get: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "# Recovered report") {
		t.Errorf("stdout missing the recovered report:\n%s", stdout)
	}
	// The tool set of a resumed interaction is unknowable (the API does
	// not echo it); the front matter must not claim one.
	if strings.Contains(stdout, "tools:") {
		t.Errorf("front matter claims a tool set for a resumed interaction:\n%s", stdout)
	}
	// Likewise the query: the interaction's user_input step carries the
	// TEMPLATED prompt, not the raw question, so the adapter records no
	// query for resumed interactions and the front matter omits the
	// field rather than rendering the prompt boilerplate.
	if strings.Contains(stdout, "query:") {
		t.Errorf("front matter claims a query for a resumed interaction:\n%s", stdout)
	}
}

func TestFollowUpChainsModelInteraction(t *testing.T) {
	var createBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatalf("decoding create body: %v", err)
			}
			w.Write([]byte(`{"id":"v1_followup","status":"in_progress"}`))
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
	if err != nil {
		t.Fatalf("follow-up: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "The answer is 42.") {
		t.Errorf("stdout missing the follow-up answer:\n%s", stdout)
	}

	if createBody["model"] != "gemini-3.1-pro-preview" {
		t.Errorf("model = %v, want the documented default", createBody["model"])
	}
	if createBody["previous_interaction_id"] != "v1_research" {
		t.Errorf("previous_interaction_id = %v", createBody["previous_interaction_id"])
	}
	if _, hasAgent := createBody["agent"]; hasAgent {
		t.Error("a follow-up create must not carry agent")
	}
}

func TestFollowUpRequiresQuery(t *testing.T) {
	if _, _, err := execute(t, "follow-up", "v1_research"); err == nil {
		t.Error("follow-up without a query must fail before any seam is constructed")
	}
}

// newPlanFlowServer fakes the full --plan sequence: the first create is
// the plan round (collaborative_planning true), the second the approval
// (collaborative_planning false, chained to the plan). Create bodies
// are captured in order.
func newPlanFlowServer(t *testing.T, bodies *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decoding create body: %v", err)
			}
			*bodies = append(*bodies, body)
			if len(*bodies) == 1 {
				w.Write([]byte(`{"id":"v1_plan","status":"in_progress"}`))
			} else {
				w.Write([]byte(`{"id":"v1_research","status":"in_progress"}`))
			}
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
	var bodies []map[string]any
	server := newPlanFlowServer(t, &bodies)
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	stdout, stderr, err := execute(t, "research", "--query", "smoke question", "--plan", "--accept-plan")
	if err != nil {
		t.Fatalf("research --plan: %v\nstderr: %s", err, stderr)
	}

	// The report on stdout, the plan on stderr — a piped report stays clean.
	if !strings.Contains(stdout, "# Planned report") {
		t.Errorf("stdout missing the report:\n%s", stdout)
	}
	if strings.Contains(stdout, "Survey the vendors") {
		t.Errorf("the plan leaked onto stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "Survey the vendors") {
		t.Errorf("stderr missing the rendered plan:\n%s", stderr)
	}

	if len(bodies) != 2 {
		t.Fatalf("creates = %d, want plan + approval", len(bodies))
	}
	planCfg, _ := bodies[0]["agent_config"].(map[string]any)
	if planCfg == nil || planCfg["collaborative_planning"] != true {
		t.Errorf("plan create agent_config = %v, want collaborative_planning true", planCfg)
	}
	approveCfg, _ := bodies[1]["agent_config"].(map[string]any)
	if approveCfg == nil || approveCfg["collaborative_planning"] != false {
		t.Errorf("approval agent_config = %v, want collaborative_planning false", approveCfg)
	}
	if bodies[1]["previous_interaction_id"] != "v1_plan" {
		t.Errorf("approval previous_interaction_id = %v, want the accepted plan", bodies[1]["previous_interaction_id"])
	}
}

func TestResearchPlanNonInteractiveNeedsAcceptPlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request must be sent when the plan cannot be reviewed, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	// The test stdin is not a terminal, so --plan alone must abort.
	_, _, err := execute(t, "research", "--query", "q", "--plan", "-o", "none")
	if err == nil || !strings.Contains(err.Error(), "--accept-plan") {
		t.Errorf("err = %v, want the --accept-plan guidance", err)
	}
}

func TestResearchBudgetGatesThePlanPhaseToo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("plan rounds spend; the budget gate must block them, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	_, _, err := execute(t, "research", "--query", "q", "--plan", "--accept-plan", "--budget", "1.00", "-o", "none")
	exitErr, ok := errors.AsType[*ExitError](err)
	if !ok || exitErr.Code != ExitBlocked {
		t.Errorf("err = %v, want ExitBlocked before any plan round", err)
	}
}
