package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
)

// workerArgs returns the flags that point --agent worker at the given fakes.
func workerArgs(modelSrv *model.FakeServer, searchSrv *search.FakeServer, extra ...string) []string {
	args := []string{
		"research", "--query", "why is the sky blue",
		"--agent", "worker",
		"--fleet-model-endpoint", modelSrv.URL(),
		"--fleet-model-name", "test-model",
		"--fleet-model-key-ref", "secret://MODEL_KEY",
		"--fleet-search-endpoint", searchSrv.URL(),
	}
	return append(args, extra...)
}

// TestWorkerSearchFetchFinalThroughCLI drives the complete worker path
// through the compiled command tree: search, fetch a loopback page (admitted
// by the test-only loopback switch), and answer citing it.
func TestWorkerSearchFetchFinalThroughCLI(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Sky</h1><p>Rayleigh scattering.</p></body></html>"))
	}))
	defer page.Close()
	searchSrv := search.NewFakeServer([]search.Result{{Title: "Sky article", URL: page.URL, Snippet: "scattering"}})
	defer searchSrv.Close()
	modelSrv := model.NewFakeServer(
		model.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		model.FakeReply{Content: `{"action":"fetch","url":"` + page.URL + `"}`, FinishReason: "stop"},
		model.FakeReply{Content: `{"action":"final","answer":"# Answer\n\nRayleigh scattering.","citations":[{"url":"` + page.URL + `","title":"Sky article"}]}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("CHIRON_FETCH_ALLOW_LOOPBACK", "1")

	stdout, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "text")...)
	if err != nil {
		t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr)
	}
	if got := modelSrv.CallCount(); got != 3 {
		t.Errorf("model call count = %d, want 3", got)
	}
	if !strings.Contains(stdout, "Rayleigh scattering") || !strings.Contains(stdout, "[Sky article]("+page.URL+")") {
		t.Errorf("report lacks the body or the fetched source:\n%s", stdout)
	}
	if !strings.Contains(stderr, `"interaction_created"`) || !strings.Contains(stderr, "wkr_") {
		t.Errorf("stderr lacks the early interaction id event:\n%s", stderr)
	}
}

// TestWorkerLoopbackSwitchRejectsOtherValues: the loopback switch accepts
// only "1", so a typo cannot widen the SSRF guard.
func TestWorkerLoopbackSwitchRejectsOtherValues(t *testing.T) {
	searchSrv := search.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := model.NewFakeServer()
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("CHIRON_FETCH_ALLOW_LOOPBACK", "true")

	_, _, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "none")...)
	if err == nil || !strings.Contains(err.Error(), "CHIRON_FETCH_ALLOW_LOOPBACK") {
		t.Fatalf("err = %v, want the switch rejected", err)
	}
	if got := modelSrv.CallCount(); got != 0 {
		t.Errorf("model call count = %d, want 0 before the switch is validated", got)
	}
}

// TestWorkerFailureExitCode: a worker run that ends failed (an invalid model
// action) exits with the research-failed code after emitting a placeholder
// report, matching the exit-code contract.
func TestWorkerFailureExitCode(t *testing.T) {
	searchSrv := search.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := model.NewFakeServer(model.FakeReply{Content: `{"action":"shell","query":"rm -rf /"}`, FinishReason: "stop"})
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")

	stdout, _, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "text")...)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != ExitResearchFailed {
		t.Fatalf("err = %v, want ExitError code %d", err, ExitResearchFailed)
	}
	if !strings.Contains(stdout, "status: failed") || !strings.Contains(stdout, "invalid model action") {
		t.Errorf("placeholder report lacks the failed status detail:\n%s", stdout)
	}
}

// TestWorkerRejectsDeepResearchLevers: --budget and --plan are refused with
// --agent worker before any secret is resolved or request made.
func TestWorkerRejectsDeepResearchLevers(t *testing.T) {
	searchSrv := search.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := model.NewFakeServer()
	defer modelSrv.Close()

	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"budget", []string{"--budget", "1"}, "budget:"},
		{"plan", []string{"--plan"}, "plan:"},
		{"tools", []string{"--tools", "google_search"}, "tools:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := execute(t, workerArgs(modelSrv, searchSrv, append([]string{"-o", "none"}, tt.args...)...)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want an error naming %s", err, tt.want)
			}
			if _, ok := errors.AsType[*ExitError](err); ok {
				t.Errorf("a rejected lever is a usage error: %v", err)
			}
		})
	}
	if got := modelSrv.CallCount(); got != 0 {
		t.Errorf("model call count = %d, want 0", got)
	}
}

// TestGetAndFollowUpRefuseWorkerIDs: the commands that re-attach to
// server-side state refuse a worker-minted id with a clear message, before
// resolving the Gemini key.
func TestGetAndFollowUpRefuseWorkerIDs(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"get", []string{"get", "wkr_0123456789abcdef0123456789abcdef", "-o", "none"}},
		{"follow-up", []string{"follow-up", "wkr_0123456789abcdef0123456789abcdef", "--query", "more?", "-o", "none"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := execute(t, tt.args...)
			if err == nil || !strings.Contains(err.Error(), "in-process worker id") {
				t.Fatalf("err = %v, want the worker-id refusal", err)
			}
		})
	}
}

// TestWorkerMissingFleetFieldsFailBeforeAnyRequest: each required fleet
// field is named in the error when absent, and nothing is dialled.
func TestWorkerMissingFleetFieldsFailBeforeAnyRequest(t *testing.T) {
	searchSrv := search.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := model.NewFakeServer()
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")

	for _, tt := range []struct {
		name string
		drop string
		want string
	}{
		{"model endpoint", "--fleet-model-endpoint", "fleet.model_endpoint"},
		{"model name", "--fleet-model-name", "fleet.model_name"},
		{"model key ref", "--fleet-model-key-ref", "fleet.model_key_ref"},
		{"search endpoint", "--fleet-search-endpoint", "fleet.search_endpoint"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			full := workerArgs(modelSrv, searchSrv, "-o", "none")
			var args []string
			for i := 0; i < len(full); i++ {
				if full[i] == tt.drop {
					i++
					continue
				}
				args = append(args, full[i])
			}
			_, _, err := execute(t, args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to name %s", err, tt.want)
			}
		})
	}
	if modelSrv.CallCount() != 0 || searchSrv.CallCount() != 0 {
		t.Error("a misconfigured worker must not dial any endpoint")
	}
}
