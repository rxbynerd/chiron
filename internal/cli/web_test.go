package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa/exatest"
)

const (
	exaTestKey = "exa-test-key-3f2b9c1e"
	skyPage    = "https://example.com/sky"
)

// exaWorkerArgs returns the flags that run --agent worker with Exa serving
// both web roles.
func exaWorkerArgs(modelSrv *modeltest.FakeServer, exaSrv *exatest.FakeServer, extra ...string) []string {
	args := []string{
		"research", "--query", "why is the sky blue",
		"--agent", "worker",
		"--fleet-model-endpoint", modelSrv.URL(),
		"--fleet-model-name", "test-model",
		"--fleet-model-key-ref", "secret://MODEL_KEY",
		"--fleet-search-provider", "exa",
		"--fleet-fetch-provider", "exa",
		"--fleet-exa-endpoint", exaSrv.URL(),
		"--fleet-exa-key-ref", "secret://EXA_KEY",
	}
	return append(args, extra...)
}

// searchFetchFinal scripts a worker that searches, fetches skyPage and
// answers citing it.
func searchFetchFinal() *modeltest.FakeServer {
	return modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"fetch","url":"` + skyPage + `"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"final","answer":"# Answer\n\nRayleigh scattering.","citations":[{"url":"` + skyPage + `","title":"Sky"}]}`, FinishReason: "stop"},
	)
}

// TestWorkerExaForBothRolesThroughCLI drives a worker whose searches and
// page fetches both go to Exa: the key travels only in x-api-key, the
// endpoints event names Exa for search and fetch, the report records the
// provider and the web spend, and neither the MCP search variable nor the
// direct-fetch loopback switch is read.
func TestWorkerExaForBothRolesThroughCLI(t *testing.T) {
	exaSrv := exatest.NewFakeServer(
		exatest.WithResults([]web.Result{{Title: "Sky", URL: skyPage, Snippet: "scattering"}}),
		exatest.WithPages(map[string]string{skyPage: "# Sky\n\nRayleigh scattering makes the sky blue."}),
	)
	defer exaSrv.Close()
	modelSrv := searchFetchFinal()
	defer modelSrv.Close()
	clearEndpointEnv(t)
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("EXA_KEY", exaTestKey)
	t.Setenv(config.EnvFleetSearchEndpoint, "http://search.invalid/not-loopback")
	t.Setenv("CHIRON_FETCH_ALLOW_LOOPBACK", "yes")

	stdout, stderr, err := execute(t, exaWorkerArgs(modelSrv, exaSrv, "-o", "text")...)
	if err != nil {
		t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr)
	}
	reqs := exaSrv.Requests()
	if len(reqs) != 2 || reqs[0].Path != "/search" || reqs[1].Path != "/contents" {
		t.Fatalf("Exa requests = %+v, want one search then one contents call", reqs)
	}
	for _, r := range reqs {
		if r.APIKey != exaTestKey || r.Authorization != "" {
			t.Errorf("Exa request %s carried x-api-key %q and Authorization %q, want only the resolved key in x-api-key", r.Path, r.APIKey, r.Authorization)
		}
	}
	if strings.Contains(string(reqs[0].Body), "objective") {
		t.Errorf("the objective was sent without --fleet-search-send-objective: %s", reqs[0].Body)
	}

	events := endpointsEvents(t, stderr)
	want := `{"endpoints":{"model":"` + modelSrv.URL() + `","search":"` + exaSrv.URL() + `","fetch":"` + exaSrv.URL() + `"}}`
	if len(events) != 1 || events[0] != want {
		t.Errorf("endpoints events = %q, want %s", events, want)
	}
	for _, line := range []string{"web_cost_usd: 0.008", "tools: ['web_search:exa', 'web_fetch:exa']", "[Sky](" + skyPage + ")", "Rayleigh scattering"} {
		if !strings.Contains(stdout, line) {
			t.Errorf("report lacks %q:\n%s", line, stdout)
		}
	}
	if strings.Contains(stdout+stderr, exaTestKey) {
		t.Error("the Exa key reached the report or stderr")
	}
}

// TestWorkerMCPSearchWithExaFetchThroughCLI: search stays on the MCP server
// while Exa fetches; the endpoints event names each role's origin.
func TestWorkerMCPSearchWithExaFetchThroughCLI(t *testing.T) {
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "Sky", URL: skyPage, Snippet: "scattering"}})
	defer searchSrv.Close()
	exaSrv := exatest.NewFakeServer(exatest.WithPages(map[string]string{skyPage: "Rayleigh scattering."}))
	defer exaSrv.Close()
	modelSrv := searchFetchFinal()
	defer modelSrv.Close()
	clearEndpointEnv(t)
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("EXA_KEY", exaTestKey)

	stdout, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "text",
		"--fleet-fetch-provider", "exa",
		"--fleet-exa-endpoint", exaSrv.URL(),
		"--fleet-exa-key-ref", "secret://EXA_KEY",
	)...)
	if err != nil {
		t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr)
	}
	if reqs := exaSrv.Requests(); len(reqs) != 1 || reqs[0].Path != "/contents" {
		t.Errorf("Exa requests = %+v, want the one contents call", reqs)
	}
	events := endpointsEvents(t, stderr)
	want := `{"endpoints":{"model":"` + modelSrv.URL() + `","search":"` + searchSrv.URL() + `","fetch":"` + exaSrv.URL() + `"}}`
	if len(events) != 1 || events[0] != want {
		t.Errorf("endpoints events = %q, want %s", events, want)
	}
	if !strings.Contains(stdout, "tools: [web_search, 'web_fetch:exa']") || !strings.Contains(stdout, "web_cost_usd: 0.001") {
		t.Errorf("report lacks the tool list or the fetch cost:\n%s", stdout)
	}
}

// TestWorkerSendsObjectiveWhenAsked: --fleet-search-send-objective sends the
// research question with each Exa search.
func TestWorkerSendsObjectiveWhenAsked(t *testing.T) {
	exaSrv := exatest.NewFakeServer()
	defer exaSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		finalReply,
	)
	defer modelSrv.Close()
	clearEndpointEnv(t)
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("EXA_KEY", exaTestKey)

	if _, stderr, err := execute(t, exaWorkerArgs(modelSrv, exaSrv, "-o", "none", "--fleet-search-send-objective")...); err != nil {
		t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr)
	}
	if reqs := exaSrv.Requests(); len(reqs) != 1 || !strings.Contains(string(reqs[0].Body), `"objective":"why is the sky blue"`) {
		t.Errorf("Exa requests = %+v, want the search to carry the objective", reqs)
	}
}

// TestWorkerEndsAtOnceOnRejectedExaKey: a 401 from Exa ends the run Failed
// after the one model turn that met it.
func TestWorkerEndsAtOnceOnRejectedExaKey(t *testing.T) {
	exaSrv := exatest.NewFakeServer(exatest.WithStatus(401, `{"requestId":"r1","error":"Invalid API key","tag":"INVALID_API_KEY"}`))
	defer exaSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"search","query":"sky again"}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()
	clearEndpointEnv(t)
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("EXA_KEY", exaTestKey)

	stdout, _, err := execute(t, exaWorkerArgs(modelSrv, exaSrv, "-o", "text")...)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != ExitResearchFailed {
		t.Fatalf("err = %v, want ExitError code %d", err, ExitResearchFailed)
	}
	if got := modelSrv.CallCount(); got != 1 {
		t.Errorf("model calls = %d, want 1", got)
	}
	if !strings.Contains(stdout, "INVALID_API_KEY") {
		t.Errorf("report lacks the provider's reason:\n%s", stdout)
	}
}

// TestWorkerExaRefusals: misconfigured Exa selections fail before any
// secret resolves or anything is dialled.
func TestWorkerExaRefusals(t *testing.T) {
	exaSrv := exatest.NewFakeServer()
	defer exaSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()

	without := func(args []string, flag string) []string {
		var out []string
		for i := 0; i < len(args); i++ {
			if args[i] == flag {
				i++
				continue
			}
			out = append(out, args[i])
		}
		return out
	}
	for _, tt := range []struct {
		name  string
		args  []string
		stdin string
		want  []string
	}{
		{"base config naming the exa endpoint",
			without(exaWorkerArgs(modelSrv, exaSrv, "-o", "none", "--config", "-"), "--fleet-exa-endpoint"),
			"fleet:\n  exa_endpoint: " + exaSrv.URL() + "\n",
			[]string{"fleet.exa_endpoint: must not come from a base config", config.EnvFleetExaEndpoint}},
		{"exa key without an exa role",
			append(workerArgs(modelSrv, searchSrv, "-o", "none"), "--fleet-exa-key-ref", "secret://EXA_KEY"), "",
			[]string{"fleet.exa_key_ref: is set but neither"}},
		{"mcp search endpoint with exa search",
			append(exaWorkerArgs(modelSrv, exaSrv, "-o", "none"), "--fleet-search-endpoint", searchSrv.URL()), "",
			[]string{"fleet.search_endpoint: applies to search provider"}},
		{"missing exa endpoint",
			without(exaWorkerArgs(modelSrv, exaSrv, "-o", "none"), "--fleet-exa-endpoint"), "",
			[]string{"fleet.exa_endpoint is required", "--fleet-exa-endpoint https://api.exa.ai", config.EnvFleetExaEndpoint}},
		{"missing exa key",
			without(exaWorkerArgs(modelSrv, exaSrv, "-o", "none"), "--fleet-exa-key-ref"), "",
			[]string{"fleet.exa_key_ref is required"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clearEndpointEnv(t)
			resolver := &countingResolver{}
			var stderr syncBuffer
			_, err := executeWith(t, resolver, strings.NewReader(tt.stdin), &stderr, tt.args...)
			if err == nil {
				t.Fatal("the run was accepted")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if n := resolver.calls.Load(); n != 0 {
				t.Errorf("resolver invoked %d times before the refusal", n)
			}
		})
	}
	if len(exaSrv.Requests()) != 0 || searchSrv.CallCount() != 0 || modelSrv.CallCount() != 0 {
		t.Error("a refused configuration must not dial anything")
	}
}
