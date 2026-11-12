package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa/exatest"
	"github.com/rxbynerd/chiron/internal/secret"
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

// TestWorker_ExaServesSearchAndFetch drives a worker whose searches and
// page fetches both go to Exa: the key travels only in x-api-key, the
// endpoints event names Exa for search and fetch, the report records the
// provider and the web spend, and neither the MCP search variable nor the
// direct-fetch loopback switch is read.
func TestWorker_ExaServesSearchAndFetch(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	exaSrv := exatest.NewFakeServer(
		exatest.WithResults([]web.Result{{Title: "Sky", URL: skyPage, Snippet: "scattering"}}),
		exatest.WithPages(map[string]string{skyPage: "# Sky\n\nRayleigh scattering makes the sky blue."}),
	)
	defer exaSrv.Close()
	modelSrv := searchFetchFinal()
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("EXA_KEY", exaTestKey)
	t.Setenv(config.EnvFleetSearchEndpoint, "http://search.invalid/not-loopback")
	t.Setenv("CHIRON_FETCH_ALLOW_LOOPBACK", "yes")

	stdout, stderr, err := execute(t, exaWorkerArgs(modelSrv, exaSrv, "-o", "text")...)
	must.NoError(err, "research --agent worker; stderr: %s", stderr)

	reqs := exaSrv.Requests()
	must.Len(reqs, 2, "Exa requests = %+v, want one search then one contents call", reqs)
	must.Equal("/search", reqs[0].Path)
	must.Equal("/contents", reqs[1].Path)
	for _, r := range reqs {
		check.Equal(exaTestKey, r.APIKey, "Exa request %s x-api-key", r.Path)
		check.Empty(r.Authorization, "Exa request %s Authorization header", r.Path)
	}
	check.NotContains(string(reqs[0].Body), "objective", "the objective was sent without --fleet-search-send-objective")

	events := endpointsEvents(t, stderr)
	want := `{"endpoints":{"model":"` + modelSrv.URL() + `","search":"` + exaSrv.URL() + `","fetch":"` + exaSrv.URL() + `"}}`
	check.Equal([]string{want}, events)
	for _, line := range []string{"web_cost_usd: 0.008", "tools: ['web_search:exa', 'web_fetch:exa']", "[Sky](" + skyPage + ")", "Rayleigh scattering"} {
		check.Contains(stdout, line)
	}
	check.NotContains(stdout+stderr, exaTestKey, "the Exa key reached the report or stderr")
}

// TestWorker_MCPSearchWithExaFetch: search stays on the MCP server while
// Exa fetches; the endpoints event names each role's origin.
func TestWorker_MCPSearchWithExaFetch(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "Sky", URL: skyPage, Snippet: "scattering"}})
	defer searchSrv.Close()
	exaSrv := exatest.NewFakeServer(exatest.WithPages(map[string]string{skyPage: "Rayleigh scattering."}))
	defer exaSrv.Close()
	modelSrv := searchFetchFinal()
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("EXA_KEY", exaTestKey)

	stdout, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "text",
		"--fleet-fetch-provider", "exa",
		"--fleet-exa-endpoint", exaSrv.URL(),
		"--fleet-exa-key-ref", "secret://EXA_KEY",
	)...)
	must.NoError(err, "research --agent worker; stderr: %s", stderr)

	reqs := exaSrv.Requests()
	if check.Len(reqs, 1, "Exa requests = %+v, want the one contents call", reqs) {
		check.Equal("/contents", reqs[0].Path)
	}

	events := endpointsEvents(t, stderr)
	want := `{"endpoints":{"model":"` + modelSrv.URL() + `","search":"` + searchSrv.URL() + `","fetch":"` + exaSrv.URL() + `"}}`
	check.Equal([]string{want}, events)
	check.Contains(stdout, "tools: [web_search, 'web_fetch:exa']")
	check.Contains(stdout, "web_cost_usd: 0.001")
}

// TestWorker_ExaSearchSendsObjectiveWhenAsked: --fleet-search-send-objective
// sends the research question with each Exa search.
func TestWorker_ExaSearchSendsObjectiveWhenAsked(t *testing.T) {
	must := require.New(t)

	exaSrv := exatest.NewFakeServer()
	defer exaSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		finalReply,
	)
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("EXA_KEY", exaTestKey)

	_, stderr, err := execute(t, exaWorkerArgs(modelSrv, exaSrv, "-o", "none", "--fleet-search-send-objective")...)
	must.NoError(err, "research --agent worker; stderr: %s", stderr)

	reqs := exaSrv.Requests()
	must.Len(reqs, 1, "Exa requests = %+v, want one search", reqs)
	must.Contains(string(reqs[0].Body), `"objective":"why is the sky blue"`, "the search should carry the objective")
}

// TestWorker_RejectedExaKeyEndsRunAtOnce: a 401 from Exa ends the run Failed
// after the one model turn that met it.
func TestWorker_RejectedExaKeyEndsRunAtOnce(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	exaSrv := exatest.NewFakeServer(exatest.WithStatus(401, `{"requestId":"r1","error":"Invalid API key","tag":"INVALID_API_KEY"}`))
	defer exaSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"search","query":"sky again"}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("EXA_KEY", exaTestKey)

	stdout, _, err := execute(t, exaWorkerArgs(modelSrv, exaSrv, "-o", "text")...)
	var exitErr *ExitError
	must.ErrorAs(err, &exitErr)
	must.Equal(ExitResearchFailed, exitErr.Code)
	check.Equal(1, modelSrv.CallCount(), "model calls")
	check.Contains(stdout, "INVALID_API_KEY", "report lacks the provider's reason")
}

// TestWorker_ExaMisconfigurationRefused: misconfigured Exa selections fail
// before any secret resolves or anything is dialled.
func TestWorker_ExaMisconfigurationRefused(t *testing.T) {
	check := assert.New(t)

	exaSrv := exatest.NewFakeServer()
	defer exaSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()

	for _, tt := range []struct {
		name  string
		args  []string
		stdin string
		want  []string
	}{
		{"base config naming the exa endpoint",
			withoutFlag(exaWorkerArgs(modelSrv, exaSrv, "-o", "none", "--config", "-"), "--fleet-exa-endpoint"),
			"fleet:\n  exa_endpoint: " + exaSrv.URL() + "\n",
			[]string{"fleet.exa_endpoint: must not come from a base config", config.EnvFleetExaEndpoint}},
		{"exa key without an exa role",
			append(workerArgs(modelSrv, searchSrv, "-o", "none"), "--fleet-exa-key-ref", "secret://EXA_KEY"), "",
			[]string{"fleet.exa_key_ref: is set but neither"}},
		{"mcp search endpoint with exa search",
			append(exaWorkerArgs(modelSrv, exaSrv, "-o", "none"), "--fleet-search-endpoint", searchSrv.URL()), "",
			[]string{"fleet.search_endpoint: applies to search provider"}},
		{"missing exa endpoint",
			withoutFlag(exaWorkerArgs(modelSrv, exaSrv, "-o", "none"), "--fleet-exa-endpoint"), "",
			[]string{"fleet.exa_endpoint is required", "--fleet-exa-endpoint https://api.exa.ai", config.EnvFleetExaEndpoint}},
		{"missing exa key",
			withoutFlag(exaWorkerArgs(modelSrv, exaSrv, "-o", "none"), "--fleet-exa-key-ref"), "",
			[]string{"fleet.exa_key_ref is required"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			resolver := &countingResolver{}
			var stderr syncBuffer
			_, err := executeWith(t, resolver, strings.NewReader(tt.stdin), &stderr, tt.args...)
			must.Error(err, "the run was accepted")
			for _, want := range tt.want {
				must.ErrorContains(err, want)
			}
			must.Zero(resolver.calls.Load(), "the resolver ran before the refusal")
		})
	}

	check.Empty(exaSrv.Requests(), "a refused configuration must not dial Exa")
	check.Zero(searchSrv.CallCount(), "a refused configuration must not dial the search server")
	check.Zero(modelSrv.CallCount(), "a refused configuration must not dial the model server")
}

// TestResearchConfig_RefusesSendObjective: the objective switch cannot
// travel through a pipeline, since the next stage refuses it in a base.
func TestResearchConfig_RefusesSendObjective(t *testing.T) {
	must := require.New(t)

	stdout, _, err := execute(t, "research-config", "--agent", "worker", "--fleet-search-provider", "exa", "--fleet-search-send-objective")
	must.ErrorContains(err, "--fleet-search-send-objective")
	must.ErrorContains(err, "final chiron research stage")
	must.Empty(stdout, "a refused flag should emit nothing on stdout")
}

// TestWorker_ExaSearchWithDirectFetch: Exa searches while Chiron fetches
// pages itself. A page bound above Exa's character limit is valid, since Exa
// fetches nothing; the endpoints event has no fetch entry; the tool list
// qualifies only search; and the loopback switch is still read.
func TestWorker_ExaSearchWithDirectFetch(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("Rayleigh scattering."))
	}))
	defer page.Close()
	exaSrv := exatest.NewFakeServer(exatest.WithResults([]web.Result{{Title: "Sky", URL: page.URL, Snippet: "scattering"}}))
	defer exaSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"fetch","url":"` + page.URL + `"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"final","answer":"# Answer\n\nRayleigh scattering.","citations":[{"url":"` + page.URL + `","title":"Sky"}]}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("EXA_KEY", exaTestKey)
	t.Setenv("CHIRON_FETCH_ALLOW_LOOPBACK", "1")

	args := []string{
		"research", "--query", "why is the sky blue", "--agent", "worker", "-o", "text",
		"--fleet-model-endpoint", modelSrv.URL(), "--fleet-model-name", "test-model", "--fleet-model-key-ref", "secret://MODEL_KEY",
		"--fleet-search-provider", "exa", "--fleet-exa-endpoint", exaSrv.URL(), "--fleet-exa-key-ref", "secret://EXA_KEY",
		"--fleet-max-page-bytes", "2000000",
	}
	stdout, stderr, err := execute(t, args...)
	must.NoError(err, "research --agent worker; stderr: %s", stderr)

	reqs := exaSrv.Requests()
	if check.Len(reqs, 1, "Exa requests = %+v, want the one search", reqs) {
		check.Equal("/search", reqs[0].Path)
	}

	events := endpointsEvents(t, stderr)
	want := `{"endpoints":{"model":"` + modelSrv.URL() + `","search":"` + exaSrv.URL() + `"}}`
	check.Equal([]string{want}, events)
	check.Contains(stdout, "tools: ['web_search:exa', web_fetch]")
	check.Contains(stdout, "[Sky]("+page.URL+")")

	t.Setenv("CHIRON_FETCH_ALLOW_LOOPBACK", "yes")
	_, _, err = execute(t, args...)
	check.ErrorContains(err, "CHIRON_FETCH_ALLOW_LOOPBACK", "the loopback switch should be read and refused for the direct fetch")
}

// TestExaEndpoint_FromEnvironment: CHIRON_FLEET_EXA_ENDPOINT supplies the Exa
// endpoint when its flag is unset, a set flag wins, and the variable is
// neither read nor validated when no role uses Exa.
func TestExaEndpoint_FromEnvironment(t *testing.T) {
	searchOnce := func() *modeltest.FakeServer {
		return modeltest.NewFakeServer(
			modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
			finalReply,
		)
	}

	t.Run("env supplies the endpoint", func(t *testing.T) {
		must := require.New(t)
		exaSrv := exatest.NewFakeServer()
		defer exaSrv.Close()
		modelSrv := searchOnce()
		defer modelSrv.Close()
		t.Setenv(config.EnvFleetExaEndpoint, exaSrv.URL())
		t.Setenv("MODEL_KEY", "test-model-key")
		t.Setenv("EXA_KEY", exaTestKey)

		_, stderr, err := execute(t, withoutFlag(exaWorkerArgs(modelSrv, exaSrv, "-o", "none"), "--fleet-exa-endpoint")...)
		must.NoError(err, "research --agent worker; stderr: %s", stderr)
		must.Len(exaSrv.Requests(), 1, "want the search through the env-named endpoint")
	})

	t.Run("flag wins over env", func(t *testing.T) {
		must := require.New(t)
		flagSrv := exatest.NewFakeServer()
		defer flagSrv.Close()
		envSrv := exatest.NewFakeServer()
		defer envSrv.Close()
		modelSrv := searchOnce()
		defer modelSrv.Close()
		t.Setenv(config.EnvFleetExaEndpoint, envSrv.URL())
		t.Setenv("MODEL_KEY", "test-model-key")
		t.Setenv("EXA_KEY", exaTestKey)

		_, stderr, err := execute(t, exaWorkerArgs(modelSrv, flagSrv, "-o", "none")...)
		must.NoError(err, "research --agent worker; stderr: %s", stderr)
		must.Len(flagSrv.Requests(), 1, "want the flag endpoint to serve the search")
		must.Empty(envSrv.Requests(), "want the env endpoint unused, so the flag wins")
	})

	t.Run("env unread without an exa role", func(t *testing.T) {
		must := require.New(t)
		modelSrv := modeltest.NewFakeServer(finalReply)
		defer modelSrv.Close()
		searchSrv := searchtest.NewFakeServer(nil)
		defer searchSrv.Close()
		t.Setenv(config.EnvFleetExaEndpoint, "http://exa.invalid/not-loopback")
		t.Setenv("MODEL_KEY", "test-model-key")

		_, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "none")...)
		must.NoError(err, "an unused Exa variable failed the run; stderr: %s", stderr)
	})
}

// TestEndpointsEvent_PrecedesFirstExaRequest: the run names Exa's origin on
// stderr before its first request reaches Exa.
func TestEndpointsEvent_PrecedesFirstExaRequest(t *testing.T) {
	must := require.New(t)

	exaSrv := exatest.NewFakeServer()
	defer exaSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		finalReply,
	)
	defer modelSrv.Close()
	stderr := &syncBuffer{}
	exaProbe := newFirstRequestProbe(t, "exa", stderr, exaSrv.URL())
	defer exaProbe.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("EXA_KEY", exaTestKey)

	args := []string{
		"research", "--query", "why is the sky blue", "--agent", "worker", "-o", "none",
		"--fleet-model-endpoint", modelSrv.URL(), "--fleet-model-name", "test-model", "--fleet-model-key-ref", "secret://MODEL_KEY",
		"--fleet-search-provider", "exa", "--fleet-exa-endpoint", exaProbe.URL(), "--fleet-exa-key-ref", "secret://EXA_KEY",
	}
	_, err := executeWith(t, secret.Default(), strings.NewReader(""), stderr, args...)
	must.NoError(err, "research --agent worker; stderr: %s", stderr.String())

	at, ok := exaProbe.stderrAtFirstRequest()
	must.True(ok, "Exa was never called")
	want := `{"endpoints":{"model":"` + modelSrv.URL() + `","search":"` + exaProbe.URL() + `"}}`
	must.Equal([]string{want}, endpointsEvents(t, at), "endpoints events when Exa was first called")
}

// TestFleet_ExaServesEveryWorkerSearch: --agent fleet with Exa in both roles
// sends every worker's search to Exa, records the provider on the report,
// and rolls each worker's web spend into the run's.
func TestFleet_ExaServesEveryWorkerSearch(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fm := newCLIFleetModel(t, 3)
	modelSrv := httptest.NewServer(fm)
	defer modelSrv.Close()
	exaSrv := exatest.NewFakeServer(exatest.WithResults(fleetSources), exatest.WithCostDollars(0.007))
	defer exaSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("EXA_KEY", exaTestKey)

	stdout, stderr, err := execute(t,
		"research", "--query", "How do heat pumps compare with gas boilers?", "-o", "text",
		"--agent", "fleet", "--fleet-memory", "inmemory", "--fleet-max-workers", "3", "--fleet-concurrency", "2",
		"--fleet-model-endpoint", modelSrv.URL, "--fleet-model-name", "test-model", "--fleet-model-key-ref", "secret://MODEL_KEY",
		"--fleet-search-provider", "exa", "--fleet-fetch-provider", "exa",
		"--fleet-exa-endpoint", exaSrv.URL(), "--fleet-exa-key-ref", "secret://EXA_KEY",
	)
	must.NoError(err, "research --agent fleet; stderr: %s", stderr)

	for _, want := range []string{"agent: fleet", "status: completed", "tools: ['web_search:exa', 'web_fetch:exa']", "web_cost_usd: 0.021"} {
		check.Contains(stdout, want)
	}

	reqs := exaSrv.Requests()
	must.Len(reqs, 3, "want one search per worker")
	for _, r := range reqs {
		check.Equal("/search", r.Path)
		check.Equal(exaTestKey, r.APIKey, "Exa search key")
	}
}

// failingResolver refuses every reference.
type failingResolver struct{}

func (failingResolver) Resolve(context.Context, string) (string, error) {
	return "", errors.New("secret: not available")
}

// TestBuildWeb_FailsBeforeDialing: a key that does not resolve or a client
// that cannot be built fails buildWeb before anything is dialled. A direct
// fetch client cannot fail here, since buildWeb passes it no custom
// transport.
func TestBuildWeb_FailsBeforeDialing(t *testing.T) {
	must := require.New(t)

	exaSrv := exatest.NewFakeServer()
	defer exaSrv.Close()
	for _, tt := range []struct {
		name     string
		fc       config.FleetConfig
		resolver secret.Resolver
		want     string
	}{
		{"unresolvable exa key", config.FleetConfig{SearchProvider: config.SearchExa, ExaEndpoint: exaSrv.URL(), ExaKeyRef: "secret://EXA_KEY"},
			failingResolver{}, "secret: not available"},
		{"unresolvable search key", config.FleetConfig{SearchEndpoint: "https://search.example", SearchKeyRef: "secret://SEARCH_KEY"},
			failingResolver{}, "secret: not available"},
		{"exa client refused", config.FleetConfig{FetchProvider: config.FetchExa, SearchEndpoint: "https://search.example", ExaEndpoint: "http://exa.example", ExaKeyRef: "secret://EXA_KEY"},
			&countingResolver{}, "exa: endpoint"},
		{"search client refused", config.FleetConfig{SearchEndpoint: "http://search.example"},
			&countingResolver{}, "search:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			searcher, fetcher, closer, err := buildWeb(context.Background(), tt.fc, tt.resolver, time.Second, time.Second)
			must.ErrorContains(err, tt.want)
			must.Nil(searcher, "buildWeb returned a searcher beside its error")
			must.Nil(fetcher, "buildWeb returned a fetcher beside its error")
			must.Nil(closer, "buildWeb returned a closer beside its error")
		})
	}

	must.Empty(exaSrv.Requests(), "no Exa request should be sent")
}
