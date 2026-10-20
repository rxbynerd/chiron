package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestResearchConfigRefusesSendObjective: the objective switch cannot
// travel through a pipeline, since the next stage refuses it in a base.
func TestResearchConfigRefusesSendObjective(t *testing.T) {
	stdout, _, err := execute(t, "research-config", "--agent", "worker", "--fleet-search-provider", "exa", "--fleet-search-send-objective")
	if err == nil || !strings.Contains(err.Error(), "--fleet-search-send-objective") || !strings.Contains(err.Error(), "final chiron research stage") {
		t.Fatalf("err = %v, want the flag refused", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing emitted", stdout)
	}
}

// TestWorkerExaSearchWithDirectFetchThroughCLI: Exa searches while Chiron
// fetches pages itself. A page bound above Exa's character limit is valid,
// since Exa fetches nothing; the endpoints event has no fetch entry; the
// tool list qualifies only search; and the loopback switch is still read.
func TestWorkerExaSearchWithDirectFetchThroughCLI(t *testing.T) {
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
	clearEndpointEnv(t)
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
	if err != nil {
		t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr)
	}
	if reqs := exaSrv.Requests(); len(reqs) != 1 || reqs[0].Path != "/search" {
		t.Errorf("Exa requests = %+v, want the one search", reqs)
	}
	events := endpointsEvents(t, stderr)
	want := `{"endpoints":{"model":"` + modelSrv.URL() + `","search":"` + exaSrv.URL() + `"}}`
	if len(events) != 1 || events[0] != want {
		t.Errorf("endpoints events = %q, want %s", events, want)
	}
	if !strings.Contains(stdout, "tools: ['web_search:exa', web_fetch]") || !strings.Contains(stdout, "["+"Sky]("+page.URL+")") {
		t.Errorf("report lacks the tool list or the fetched source:\n%s", stdout)
	}

	t.Setenv("CHIRON_FETCH_ALLOW_LOOPBACK", "yes")
	if _, _, err := execute(t, args...); err == nil || !strings.Contains(err.Error(), "CHIRON_FETCH_ALLOW_LOOPBACK") {
		t.Errorf("err = %v, want the loopback switch read and refused for the direct fetch", err)
	}
}

// TestExaEndpointFromEnvironment: CHIRON_FLEET_EXA_ENDPOINT supplies the Exa
// endpoint when its flag is unset, a set flag wins, and the variable is
// neither read nor validated when no role uses Exa.
func TestExaEndpointFromEnvironment(t *testing.T) {
	withoutExaEndpoint := func(args []string) []string {
		var out []string
		for i := 0; i < len(args); i++ {
			if args[i] == "--fleet-exa-endpoint" {
				i++
				continue
			}
			out = append(out, args[i])
		}
		return out
	}
	searchOnce := func() *modeltest.FakeServer {
		return modeltest.NewFakeServer(
			modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
			finalReply,
		)
	}

	t.Run("env supplies the endpoint", func(t *testing.T) {
		exaSrv := exatest.NewFakeServer()
		defer exaSrv.Close()
		modelSrv := searchOnce()
		defer modelSrv.Close()
		clearEndpointEnv(t)
		t.Setenv(config.EnvFleetExaEndpoint, exaSrv.URL())
		t.Setenv("MODEL_KEY", "test-model-key")
		t.Setenv("EXA_KEY", exaTestKey)

		_, stderr, err := execute(t, withoutExaEndpoint(exaWorkerArgs(modelSrv, exaSrv, "-o", "none"))...)
		if err != nil {
			t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr)
		}
		if n := len(exaSrv.Requests()); n != 1 {
			t.Errorf("Exa requests = %d, want the search through the env-named endpoint", n)
		}
	})

	t.Run("flag wins over env", func(t *testing.T) {
		flagSrv := exatest.NewFakeServer()
		defer flagSrv.Close()
		envSrv := exatest.NewFakeServer()
		defer envSrv.Close()
		modelSrv := searchOnce()
		defer modelSrv.Close()
		clearEndpointEnv(t)
		t.Setenv(config.EnvFleetExaEndpoint, envSrv.URL())
		t.Setenv("MODEL_KEY", "test-model-key")
		t.Setenv("EXA_KEY", exaTestKey)

		if _, stderr, err := execute(t, exaWorkerArgs(modelSrv, flagSrv, "-o", "none")...); err != nil {
			t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr)
		}
		if len(flagSrv.Requests()) != 1 || len(envSrv.Requests()) != 0 {
			t.Errorf("requests: flag endpoint %d, env endpoint %d; want the flag to win", len(flagSrv.Requests()), len(envSrv.Requests()))
		}
	})

	t.Run("env unread without an exa role", func(t *testing.T) {
		modelSrv := modeltest.NewFakeServer(finalReply)
		defer modelSrv.Close()
		searchSrv := searchtest.NewFakeServer(nil)
		defer searchSrv.Close()
		clearEndpointEnv(t)
		t.Setenv(config.EnvFleetExaEndpoint, "http://exa.invalid/not-loopback")
		t.Setenv("MODEL_KEY", "test-model-key")

		if _, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "none")...); err != nil {
			t.Fatalf("an unused Exa variable failed the run: %v\nstderr: %s", err, stderr)
		}
	})
}

// TestEndpointsEventPrecedesTheFirstExaRequest: the run names Exa's origin
// on stderr before its first request reaches Exa.
func TestEndpointsEventPrecedesTheFirstExaRequest(t *testing.T) {
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
	clearEndpointEnv(t)
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("EXA_KEY", exaTestKey)

	args := []string{
		"research", "--query", "why is the sky blue", "--agent", "worker", "-o", "none",
		"--fleet-model-endpoint", modelSrv.URL(), "--fleet-model-name", "test-model", "--fleet-model-key-ref", "secret://MODEL_KEY",
		"--fleet-search-provider", "exa", "--fleet-exa-endpoint", exaProbe.URL(), "--fleet-exa-key-ref", "secret://EXA_KEY",
	}
	if _, err := executeWith(t, secret.Default(), strings.NewReader(""), stderr, args...); err != nil {
		t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr.String())
	}
	at, ok := exaProbe.stderrAtFirstRequest()
	if !ok {
		t.Fatal("Exa was never called")
	}
	want := `{"endpoints":{"model":"` + modelSrv.URL() + `","search":"` + exaProbe.URL() + `"}}`
	if got := endpointsEvents(t, at); len(got) != 1 || got[0] != want {
		t.Errorf("endpoints events when Exa was first called = %q, want [%s]", got, want)
	}
}

// TestFleetAgentWithExaThroughCLI: --agent fleet with Exa in both roles
// sends every worker's search to Exa, records the provider on the report,
// and rolls each worker's web spend into the run's.
func TestFleetAgentWithExaThroughCLI(t *testing.T) {
	fm := newCLIFleetModel(t, 3)
	modelSrv := httptest.NewServer(fm)
	defer modelSrv.Close()
	exaSrv := exatest.NewFakeServer(exatest.WithResults(fleetSources), exatest.WithCostDollars(0.007))
	defer exaSrv.Close()
	clearEndpointEnv(t)
	t.Setenv("MODEL_KEY", "test-model-key")
	t.Setenv("EXA_KEY", exaTestKey)

	stdout, stderr, err := execute(t,
		"research", "--query", "How do heat pumps compare with gas boilers?", "-o", "text",
		"--agent", "fleet", "--fleet-memory", "inmemory", "--fleet-max-workers", "3", "--fleet-concurrency", "2",
		"--fleet-model-endpoint", modelSrv.URL, "--fleet-model-name", "test-model", "--fleet-model-key-ref", "secret://MODEL_KEY",
		"--fleet-search-provider", "exa", "--fleet-fetch-provider", "exa",
		"--fleet-exa-endpoint", exaSrv.URL(), "--fleet-exa-key-ref", "secret://EXA_KEY",
	)
	if err != nil {
		t.Fatalf("research --agent fleet: %v\nstderr: %s", err, stderr)
	}
	for _, want := range []string{"agent: fleet", "status: completed", "tools: ['web_search:exa', 'web_fetch:exa']", "web_cost_usd: 0.021"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report lacks %q:\n%s", want, stdout)
		}
	}
	reqs := exaSrv.Requests()
	if len(reqs) != 3 {
		t.Fatalf("Exa requests = %d, want one search per worker", len(reqs))
	}
	for _, r := range reqs {
		if r.Path != "/search" || r.APIKey != exaTestKey {
			t.Errorf("Exa request = %s with key %q, want a keyed search", r.Path, r.APIKey)
		}
	}
}

// failingResolver refuses every reference.
type failingResolver struct{}

func (failingResolver) Resolve(context.Context, string) (string, error) {
	return "", errors.New("secret: not available")
}

// TestBuildWebFailures: a key that does not resolve or a client that cannot
// be built fails buildWeb before anything is dialled. A direct fetch client
// cannot fail here, since buildWeb passes it no custom transport.
func TestBuildWebFailures(t *testing.T) {
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
			searcher, fetcher, closer, err := buildWeb(context.Background(), tt.fc, tt.resolver, time.Second, time.Second)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if searcher != nil || fetcher != nil || closer != nil {
				t.Errorf("buildWeb returned providers beside its error: %v %v %v", searcher, fetcher, closer)
			}
		})
	}
	if n := len(exaSrv.Requests()); n != 0 {
		t.Errorf("Exa requests = %d, want none", n)
	}
}
