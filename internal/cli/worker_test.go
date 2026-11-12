package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
)

// workerArgs returns the flags that point --agent worker at the given fakes.
func workerArgs(modelSrv *modeltest.FakeServer, searchSrv *searchtest.FakeServer, extra ...string) []string {
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
	must := require.New(t)
	check := assert.New(t)

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Sky</h1><p>Rayleigh scattering.</p></body></html>"))
	}))
	defer page.Close()
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "Sky article", URL: page.URL, Snippet: "scattering"}})
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"fetch","url":"` + page.URL + `"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"final","answer":"# Answer\n\nRayleigh scattering.","citations":[{"url":"` + page.URL + `","title":"Sky article"}]}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("CHIRON_FETCH_ALLOW_LOOPBACK", "1")

	stdout, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "text")...)
	must.NoError(err, "research --agent worker failed; stderr:\n%s", stderr)
	check.Equal(3, modelSrv.CallCount(), "model call count")
	check.Contains(stdout, "Rayleigh scattering", "report lacks the answer body")
	check.Contains(stdout, "[Sky article]("+page.URL+")", "report lacks the fetched source")
	check.Contains(stderr, `"interaction_created"`, "stderr lacks the interaction_created event")
	check.Contains(stderr, "wkr_", "stderr lacks the worker interaction id")
}

// TestWorker_CustomSearchTool_IsConfigurable: fleet.search_tool and
// fleet.search_query_arg let the worker reach a search MCP server that names
// its tool and query argument differently from the reference backend's
// search/query (docs/DECISIONS.md, 2026-09-25 "SP-A").
func TestWorker_CustomSearchTool_IsConfigurable(t *testing.T) {
	t.Run("succeeds when configured", func(t *testing.T) {
		must := require.New(t)

		page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body><h1>Sky</h1><p>Rayleigh scattering.</p></body></html>"))
		}))
		defer page.Close()
		searchSrv := searchtest.NewFakeServer(
			[]search.Result{{Title: "Sky article", URL: page.URL, Snippet: "scattering"}},
			searchtest.WithExpectedTool("web_search", "q"),
		)
		defer searchSrv.Close()
		modelSrv := modeltest.NewFakeServer(
			modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
			modeltest.FakeReply{Content: `{"action":"fetch","url":"` + page.URL + `"}`, FinishReason: "stop"},
			modeltest.FakeReply{Content: `{"action":"final","answer":"# Answer\n\nRayleigh scattering.","citations":[{"url":"` + page.URL + `","title":"Sky article"}]}`, FinishReason: "stop"},
		)
		defer modelSrv.Close()
		t.Setenv("MODEL_KEY", "test-model-key")
		t.Setenv("CHIRON_FETCH_ALLOW_LOOPBACK", "1")

		args := workerArgs(modelSrv, searchSrv, "-o", "text",
			"--fleet-search-tool", "web_search", "--fleet-search-query-arg", "q")
		stdout, stderr, err := execute(t, args...)
		must.NoError(err, "research --agent worker failed; stderr:\n%s", stderr)
		must.Contains(stdout, "Rayleigh scattering", "report lacks the answer body")
		must.Contains(stdout, "[Sky article]("+page.URL+")", "report lacks the fetched source")
	})

	t.Run("fails when not configured", func(t *testing.T) {
		must := require.New(t)

		searchSrv := searchtest.NewFakeServer(nil, searchtest.WithExpectedTool("web_search", "q"))
		defer searchSrv.Close()
		var replies []modeltest.FakeReply
		for i := 0; i < 3; i++ {
			replies = append(replies, modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"})
		}
		modelSrv := modeltest.NewFakeServer(replies...)
		defer modelSrv.Close()
		t.Setenv("MODEL_KEY", "test-model-key")

		// workerArgs alone leaves fleet.search_tool/query_arg at their
		// search/query defaults, which the fake above refuses.
		stdout, _, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "text")...)
		var exitErr *ExitError
		must.ErrorAs(err, &exitErr)
		must.Equal(ExitResearchFailed, exitErr.Code, "exit code")
		must.Contains(stdout, "consecutive tool failures", "report lacks the tool-failure detail")
	})
}

// TestWorker_LoopbackSwitch_RejectsOtherValues: the loopback switch accepts
// only "1", so a typo cannot widen the SSRF guard.
func TestWorker_LoopbackSwitch_RejectsOtherValues(t *testing.T) {
	must := require.New(t)

	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("CHIRON_FETCH_ALLOW_LOOPBACK", "true")

	_, _, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "none")...)
	must.ErrorContains(err, "CHIRON_FETCH_ALLOW_LOOPBACK")
	must.Zero(modelSrv.CallCount(), "the model must not be called before the switch is validated")
}

// TestWorker_FailedRun_ExitsWithResearchFailedCode: a worker run that ends
// failed (an invalid model action) exits with the research-failed code after
// emitting a placeholder report, matching the exit-code contract.
func TestWorker_FailedRun_ExitsWithResearchFailedCode(t *testing.T) {
	must := require.New(t)

	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(modeltest.FakeReply{Content: `{"action":"shell","query":"rm -rf /"}`, FinishReason: "stop"})
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")

	stdout, _, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "text")...)
	var exitErr *ExitError
	must.ErrorAs(err, &exitErr)
	must.Equal(ExitResearchFailed, exitErr.Code, "exit code")
	must.Contains(stdout, "status: failed", "placeholder report lacks the failed status")
	must.Contains(stdout, "invalid model action", "placeholder report lacks the failure detail")
}

// TestWorker_DeepResearchLevers_AreRefused: --budget and --plan are refused
// with --agent worker before any secret is resolved or request made.
func TestWorker_DeepResearchLevers_AreRefused(t *testing.T) {
	must := require.New(t)

	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer()
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
			must := require.New(t)

			_, _, err := execute(t, workerArgs(modelSrv, searchSrv, append([]string{"-o", "none"}, tt.args...)...)...)
			must.ErrorContains(err, tt.want)
			_, isExitErr := errors.AsType[*ExitError](err)
			must.False(isExitErr, "a rejected lever is a usage error, not an *ExitError: %v", err)
		})
	}
	must.Zero(modelSrv.CallCount(), "model call count")
}

// TestGetAndFollowUp_RefuseWorkerIDs: the commands that re-attach to
// server-side state refuse a worker-minted id with a clear message, before
// resolving the Gemini key.
func TestGetAndFollowUp_RefuseWorkerIDs(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"get", []string{"get", "wkr_0123456789abcdef0123456789abcdef", "-o", "none"}},
		{"follow-up", []string{"follow-up", "wkr_0123456789abcdef0123456789abcdef", "--query", "more?", "-o", "none"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			_, _, err := execute(t, tt.args...)
			must.ErrorContains(err, "in-process worker id")
		})
	}
}

// TestWorker_MissingFleetField_FailsBeforeAnyRequest: each required fleet
// field is named in the error when absent, and nothing is dialled.
func TestWorker_MissingFleetField_FailsBeforeAnyRequest(t *testing.T) {
	must := require.New(t)

	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer()
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
			must := require.New(t)

			args := withoutFlag(workerArgs(modelSrv, searchSrv, "-o", "none"), tt.drop)
			_, _, err := execute(t, args...)
			must.ErrorContains(err, tt.want)
		})
	}
	must.Zero(modelSrv.CallCount(), "a misconfigured worker must not call the model endpoint")
	must.Zero(searchSrv.CallCount(), "a misconfigured worker must not call the search endpoint")
}

// TestWorker_KnowledgeComposition_RejectsMisconfiguration: the knowledge
// requirements fail the run before any secret is resolved or endpoint
// dialled. The model key ref names an unset variable, so an error that
// reached secret resolution would name it instead of the knowledge field.
func TestWorker_KnowledgeComposition_RejectsMisconfiguration(t *testing.T) {
	must := require.New(t)

	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	const unsetModelKey = "CHIRON_TEST_UNSET_MODEL_KEY"

	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"provider without endpoint", []string{"--fleet-knowledge-provider", "billet"}, "fleet.knowledge_endpoint is required"},
		{"alexandria without key", []string{
			"--fleet-knowledge-provider", "alexandria",
			"--fleet-knowledge-endpoint", "https://alexandria.example",
		}, "fleet.knowledge_key_ref is required"},
		{"stray endpoint without provider", []string{"--fleet-knowledge-endpoint", "https://billet.internal/"}, "fleet.knowledge_provider is empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			args := workerArgs(modelSrv, searchSrv, append([]string{"-o", "none", "--fleet-model-key-ref", "secret://" + unsetModelKey}, tt.args...)...)
			_, _, err := execute(t, args...)
			must.ErrorContains(err, tt.want)
			must.NotContains(err.Error(), unsetModelKey, "a secret was resolved before the knowledge check")
		})
	}
	must.Zero(modelSrv.CallCount(), "a misconfigured knowledge store must not call the model endpoint")
	must.Zero(searchSrv.CallCount(), "a misconfigured knowledge store must not call the search endpoint")
}

// TestWorker_KnowledgeKey_UnresolvableStopsRun: an unresolvable knowledge key
// ref stops the run before any request and never leaks a resolved value.
func TestWorker_KnowledgeKey_UnresolvableStopsRun(t *testing.T) {
	must := require.New(t)

	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("KB_KEY", "test-kb-key")

	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"unresolvable key", []string{
			"--fleet-knowledge-provider", "billet",
			"--fleet-knowledge-endpoint", "http://127.0.0.1:1/",
			"--fleet-knowledge-key-ref", "secret://CHIRON_TEST_UNSET_KB_KEY",
		}, "CHIRON_TEST_UNSET_KB_KEY"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			_, _, err := execute(t, workerArgs(modelSrv, searchSrv, append([]string{"-o", "none"}, tt.args...)...)...)
			must.ErrorContains(err, tt.want)
			must.NotContains(err.Error(), "test-kb-key", "the resolved knowledge key leaked into the error")
		})
	}
	must.Zero(modelSrv.CallCount(), "a worker that cannot build its knowledge store must not call the model endpoint")
	must.Zero(searchSrv.CallCount(), "a worker that cannot build its knowledge store must not call the search endpoint")
}

// TestInProcessAgents_UnresolvedKeyRef_StopsRun: for either in-process agent,
// a model or search key ref naming an unset variable stops the run with an
// error naming it, before any endpoint is dialled or an id minted, and never
// leaks the key that did resolve.
func TestInProcessAgents_UnresolvedKeyRef_StopsRun(t *testing.T) {
	t.Setenv("MODEL_KEY", "test-model-key")
	for _, agent := range []string{"worker", "fleet"} {
		for _, tt := range []struct {
			name string
			flag string
			env  string
		}{
			{"model key", "--fleet-model-key-ref", "CHIRON_TEST_UNSET_MODEL_KEY"},
			{"search key", "--fleet-search-key-ref", "CHIRON_TEST_UNSET_SEARCH_KEY"},
		} {
			t.Run(agent+" "+tt.name, func(t *testing.T) {
				must := require.New(t)

				modelSrv := modeltest.NewFakeServer()
				defer modelSrv.Close()
				searchSrv := searchtest.NewFakeServer(nil)
				defer searchSrv.Close()
				args := workerArgs(modelSrv, searchSrv, "-o", "none")
				if agent == "fleet" {
					args = fleetArgs(modelSrv.URL(), searchSrv, "-o", "none")
				}
				args = append(withoutFlag(args, tt.flag), tt.flag, "secret://"+tt.env)

				_, stderr, err := execute(t, args...)
				must.ErrorContains(err, tt.env)
				must.NotContains(err.Error(), "test-model-key", "the resolved model key leaked into the error")
				must.NotContains(stderr, "test-model-key", "the resolved model key leaked into stderr")
				must.Zero(modelSrv.CallCount(), "an agent whose key did not resolve must not call the model endpoint")
				must.Zero(searchSrv.CallCount(), "an agent whose key did not resolve must not call the search endpoint")
				must.NotContains(stderr, "wkr_", "an agent whose key did not resolve minted a worker id")
				must.NotContains(stderr, "flt_", "an agent whose key did not resolve minted a fleet id")
			})
		}
	}
}
