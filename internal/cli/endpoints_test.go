package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/memory/billet/billettest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa/exatest"
	"github.com/rxbynerd/chiron/internal/secret"
	"github.com/rxbynerd/chiron/internal/transport"
)

// countingResolver resolves every reference to one fixed key and counts the
// calls, so a test can prove when resolution never happened.
type countingResolver struct {
	calls atomic.Int32
}

func (r *countingResolver) Resolve(context.Context, string) (string, error) {
	r.calls.Add(1)
	return "test-resolved-key", nil
}

// executeWith runs a command tree built around resolver, with the caller's
// stdin and stderr, and returns stdout.
func executeWith(t *testing.T, resolver secret.Resolver, stdin io.Reader, stderr io.Writer, args ...string) (string, error) {
	t.Helper()
	root := newRootCommand(resolver)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(stderr)
	root.SetIn(stdin)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// finalReply is a model turn that ends the worker run at once.
var finalReply = modeltest.FakeReply{Content: `{"action":"final","answer":"Rayleigh scattering.","citations":[]}`, FinishReason: "stop"}

// clearEndpointEnv unsets the fleet endpoint variables for the test, so an
// operator's environment cannot change what the test exercises.
func clearEndpointEnv(t *testing.T) {
	t.Helper()
	for _, e := range config.FleetEndpoints(&config.FleetConfig{}) {
		t.Setenv(e.Env, "")
	}
}

// The ways a command can receive its base config.
const (
	baseFromFile       = "config file"
	baseFromConfigDash = "config dash"
	baseFromPipedStdin = "piped stdin"
)

// supplyBase returns the stdin and arguments that deliver base to a command
// the way source names.
func supplyBase(t *testing.T, source, base string) (io.Reader, []string) {
	t.Helper()
	must := require.New(t)
	switch source {
	case baseFromFile:
		path := filepath.Join(t.TempDir(), "base.yaml")
		must.NoError(os.WriteFile(path, []byte(base), 0o600))
		return strings.NewReader(""), []string{"--config", path}
	case baseFromConfigDash:
		return strings.NewReader(base), []string{"--config", "-"}
	case baseFromPipedStdin:
		return pipedStdin(t, base), nil
	default:
		must.FailNow("unknown base source", "source %q", source)
		return nil, nil
	}
}

// TestBaseConfig_EndpointRefusedBeforeSecretResolves: a base naming a fleet
// endpoint and its key reference is refused however it arrives, even with the
// same endpoint on a flag, before the resolver runs or anything is dialled.
// The flags alone make a complete worker run.
func TestBaseConfig_EndpointRefusedBeforeSecretResolves(t *testing.T) {
	must := require.New(t)
	modelSrv := modeltest.NewFakeServer(finalReply)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	kbSrv := billettest.NewFakeServer(nil)
	defer kbSrv.Close()
	clearEndpointEnv(t)

	for _, tt := range []struct {
		name  string
		base  string
		field string
		env   string
	}{
		{"model", "agent: worker\nfleet:\n  model_endpoint: " + modelSrv.URL() + "\n  model_key_ref: secret://MODEL_KEY\n",
			"fleet.model_endpoint", config.EnvFleetModelEndpoint},
		{"search", "agent: worker\nfleet:\n  search_endpoint: " + searchSrv.URL() + "\n  search_key_ref: secret://SEARCH_KEY\n",
			"fleet.search_endpoint", config.EnvFleetSearchEndpoint},
		{"knowledge", "agent: worker\nfleet:\n  knowledge_provider: billet\n  knowledge_endpoint: " + kbSrv.URL() + "\n  knowledge_key_ref: secret://KB_KEY\n",
			"fleet.knowledge_endpoint", config.EnvFleetKnowledgeEndpoint},
	} {
		for _, source := range []string{baseFromFile, baseFromConfigDash, baseFromPipedStdin} {
			t.Run(tt.name+"/"+source, func(t *testing.T) {
				must := require.New(t)
				stdin, baseArgs := supplyBase(t, source, tt.base)
				resolver := &countingResolver{}
				args := append(workerArgs(modelSrv, searchSrv, "-o", "none",
					"--fleet-knowledge-provider", "billet",
					"--fleet-knowledge-endpoint", kbSrv.URL(),
				), baseArgs...)
				var stderr bytes.Buffer
				_, err := executeWith(t, resolver, stdin, &stderr, args...)
				must.Error(err, "a base config naming an endpoint was accepted")
				for _, want := range []string{tt.field + ":", "credentials are sent", tt.env} {
					must.ErrorContains(err, want)
				}
				must.Zero(resolver.calls.Load(), "a refused base must stop the run before any secret resolves")
			})
		}
	}
	must.Zero(modelSrv.CallCount(), "a refused base config must not dial the model endpoint")
	must.Zero(searchSrv.CallCount(), "a refused base config must not dial the search endpoint")
	must.Empty(kbSrv.Requests(), "a refused base config must not dial the knowledge endpoint")
}

// TestCommands_BaseEndpointRefusedBeforeResolving: research on either Gemini
// tier or the worker, get and follow-up all refuse a base naming a fleet
// endpoint beside a key reference, before any secret resolves or any endpoint
// is dialled, without echoing the endpoint.
func TestCommands_BaseEndpointRefusedBeforeResolving(t *testing.T) {
	must := require.New(t)
	var geminiCalls atomic.Int32
	gemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		geminiCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gemini.Close()
	modelSrv := modeltest.NewFakeServer(finalReply)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", gemini.URL)
	clearEndpointEnv(t)

	const base = "agent: deep-research\napi_key_ref: secret://AWS_SECRET_ACCESS_KEY\nfleet:\n  model_endpoint: https://attacker.example/v1\n"
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"research deep-research", []string{"research", "--query", "q"}},
		{"research deep-research-max", []string{"research", "--query", "q", "--agent", "deep-research-max"}},
		{"get", []string{"get", "abc123"}},
		{"follow-up", []string{"follow-up", "abc123", "--query", "q"}},
		{"research worker", workerArgs(modelSrv, searchSrv)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			resolver := &countingResolver{}
			var stderr bytes.Buffer
			_, err := executeWith(t, resolver, strings.NewReader(base), &stderr, append(tt.args, "--config", "-")...)
			must.Error(err, "a base config naming an endpoint was accepted")
			must.ErrorContains(err, "fleet.model_endpoint: must not come from a base config")
			must.Zero(resolver.calls.Load(), "a refused base must stop the command before any secret resolves")
			must.NotContains(err.Error()+stderr.String(), "attacker.example", "the refused endpoint was echoed")
		})
	}
	must.Zero(geminiCalls.Load(), "a refused base config must not dial the Gemini endpoint")
	must.Zero(modelSrv.CallCount(), "a refused base config must not dial the model endpoint")
	must.Zero(searchSrv.CallCount(), "a refused base config must not dial the search endpoint")
}

// TestWorker_FlagEndpoints_ResolveBaseKeyRefs is the control for
// TestBaseConfig_EndpointRefusedBeforeSecretResolves: the same worker run with
// the key references in the base and the endpoints on flags resolves every
// reference, knowledge included, through the injected resolver and sends its
// key to the flag-named endpoint.
func TestWorker_FlagEndpoints_ResolveBaseKeyRefs(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"recall","query":"sky colour"}`, FinishReason: "stop"},
		finalReply,
	)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	kbSrv := billettest.NewFakeServer(nil)
	defer kbSrv.Close()
	clearEndpointEnv(t)
	t.Setenv("KB_KEY", "")

	const base = "agent: worker\nfleet:\n  model_name: test-model\n  model_key_ref: secret://MODEL_KEY\n  search_key_ref: secret://SEARCH_KEY\n" +
		"  knowledge_provider: billet\n  knowledge_key_ref: secret://KB_KEY\n"
	resolver := &countingResolver{}
	var stderr bytes.Buffer
	_, err := executeWith(t, resolver, strings.NewReader(base), &stderr,
		"research", "--query", "why is the sky blue", "--config", "-", "-o", "none",
		"--fleet-model-endpoint", modelSrv.URL(),
		"--fleet-search-endpoint", searchSrv.URL(),
		"--fleet-knowledge-endpoint", kbSrv.URL(),
	)
	must.NoError(err, "research --agent worker; stderr: %s", stderr.String())
	check.EqualValues(3, resolver.calls.Load(), "one key resolution each for the model, search and knowledge keys")
	reqs := modelSrv.Requests()
	must.Len(reqs, 2, "model requests, want the recall turn and the final turn")
	for i, req := range reqs {
		check.Equal("Bearer test-resolved-key", req.Authorization, "model request %d", i)
	}
	must.Equal(1, kbSrv.ToolCallCount(), "knowledge tool calls, want the one recall")
	for i, req := range kbSrv.Requests() {
		check.Equal("Bearer test-resolved-key", req.Authorization, "knowledge request %d", i)
	}
}

// TestFleetEndpoints_FromEnvironment: CHIRON_FLEET_*_ENDPOINT supplies an
// endpoint whose flag is unset, an explicitly set flag wins over it, and the
// knowledge variable is consulted only with a knowledge provider.
func TestFleetEndpoints_FromEnvironment(t *testing.T) {
	t.Run("env supplies every endpoint", func(t *testing.T) {
		must := require.New(t)
		modelSrv := modeltest.NewFakeServer(
			modeltest.FakeReply{Content: `{"action":"recall","query":"sky colour"}`, FinishReason: "stop"},
			finalReply,
		)
		defer modelSrv.Close()
		searchSrv := searchtest.NewFakeServer(nil)
		defer searchSrv.Close()
		kbSrv := billettest.NewFakeServer(nil)
		defer kbSrv.Close()
		t.Setenv(config.EnvFleetModelEndpoint, modelSrv.URL())
		t.Setenv(config.EnvFleetSearchEndpoint, searchSrv.URL())
		t.Setenv(config.EnvFleetKnowledgeEndpoint, kbSrv.URL())

		var stderr bytes.Buffer
		_, err := executeWith(t, &countingResolver{}, strings.NewReader(""), &stderr,
			"research", "--query", "why is the sky blue", "--agent", "worker", "-o", "none",
			"--fleet-model-name", "test-model",
			"--fleet-model-key-ref", "secret://MODEL_KEY",
			"--fleet-knowledge-provider", "billet",
		)
		must.NoError(err, "research --agent worker; stderr: %s", stderr.String())
		must.Equal(2, modelSrv.CallCount(), "model calls through the env-named endpoint")
		must.Equal(1, kbSrv.ToolCallCount(), "knowledge recall through the env-named endpoint")
	})

	t.Run("flag wins over env", func(t *testing.T) {
		must := require.New(t)
		flagSrv := modeltest.NewFakeServer(finalReply)
		defer flagSrv.Close()
		envSrv := modeltest.NewFakeServer(finalReply)
		defer envSrv.Close()
		searchSrv := searchtest.NewFakeServer(nil)
		defer searchSrv.Close()
		clearEndpointEnv(t)
		t.Setenv(config.EnvFleetModelEndpoint, envSrv.URL())

		var stderr bytes.Buffer
		_, err := executeWith(t, &countingResolver{}, strings.NewReader(""), &stderr,
			workerArgs(flagSrv, searchSrv, "-o", "none")...)
		must.NoError(err, "research --agent worker; stderr: %s", stderr.String())
		must.Equal(1, flagSrv.CallCount(), "the flag endpoint should serve the run")
		must.Zero(envSrv.CallCount(), "the env endpoint should be ignored when the flag is set")
	})

	t.Run("knowledge env unread without a provider", func(t *testing.T) {
		must := require.New(t)
		modelSrv := modeltest.NewFakeServer(finalReply)
		defer modelSrv.Close()
		searchSrv := searchtest.NewFakeServer(nil)
		defer searchSrv.Close()
		clearEndpointEnv(t)
		t.Setenv(config.EnvFleetKnowledgeEndpoint, "http://kb.example/")

		var stderr bytes.Buffer
		_, err := executeWith(t, &countingResolver{}, strings.NewReader(""), &stderr,
			workerArgs(modelSrv, searchSrv, "-o", "none")...)
		must.NoError(err, "an unused knowledge variable failed the run")
	})
}

// TestFleetEndpointEnv_RejectedWithoutEcho: an invalid CHIRON_FLEET_*_ENDPOINT
// fails the run before any secret resolves or endpoint is dialled, naming
// the variable but never echoing its path, query or credentials.
func TestFleetEndpointEnv_RejectedWithoutEcho(t *testing.T) {
	must := require.New(t)
	const token = "sk-live-0123456789abcdefghijklmn"
	modelSrv := modeltest.NewFakeServer(finalReply)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()

	for _, tt := range []struct {
		name  string
		env   string
		value string
		drop  string
		extra []string
	}{
		{"cleartext model", config.EnvFleetModelEndpoint, "http://gateway.example/" + token, "--fleet-model-endpoint", nil},
		{"userinfo search", config.EnvFleetSearchEndpoint, "https://user:" + token + "@search.example/mcp", "--fleet-search-endpoint", nil},
		{"query knowledge", config.EnvFleetKnowledgeEndpoint, "https://kb.example/?key=" + token, "",
			[]string{"--fleet-knowledge-provider", "billet"}},
		{"userinfo exa", config.EnvFleetExaEndpoint, "https://user:" + token + "@exa.example", "",
			[]string{"--fleet-fetch-provider", "exa", "--fleet-exa-key-ref", "secret://EXA_KEY"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			clearEndpointEnv(t)
			t.Setenv(tt.env, tt.value)
			var args []string
			full := workerArgs(modelSrv, searchSrv, append([]string{"-o", "none"}, tt.extra...)...)
			for i := 0; i < len(full); i++ {
				if full[i] == tt.drop {
					i++
					continue
				}
				args = append(args, full[i])
			}
			resolver := &countingResolver{}
			var stderr bytes.Buffer
			_, err := executeWith(t, resolver, strings.NewReader(""), &stderr, args...)
			must.ErrorContains(err, tt.env)
			must.NotContains(err.Error()+stderr.String(), token, "the rejected value leaked")
			must.Zero(resolver.calls.Load(), "resolver invoked before the variable was validated")
		})
	}
	must.Zero(modelSrv.CallCount(), "an invalid endpoint variable must not let the run dial the model endpoint")
	must.Zero(searchSrv.CallCount(), "an invalid endpoint variable must not let the run dial the search endpoint")
}

// TestWorker_MissingEndpoint_ErrorNamesFlagAndVariable: a worker run missing
// any fleet endpoint it uses fails before any secret resolves, and the error
// names the flag and the environment variable that can supply it.
func TestWorker_MissingEndpoint_ErrorNamesFlagAndVariable(t *testing.T) {
	must := require.New(t)
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	kbSrv := billettest.NewFakeServer(nil)
	defer kbSrv.Close()
	exaSrv := exatest.NewFakeServer()
	defer exaSrv.Close()

	for _, tt := range config.FleetEndpoints(&config.FleetConfig{}) {
		t.Run(tt.Flag, func(t *testing.T) {
			must := require.New(t)
			clearEndpointEnv(t)
			full := workerArgs(modelSrv, searchSrv, "-o", "none",
				"--fleet-knowledge-provider", "billet",
				"--fleet-knowledge-endpoint", kbSrv.URL(),
				"--fleet-fetch-provider", "exa",
				"--fleet-exa-endpoint", exaSrv.URL(),
				"--fleet-exa-key-ref", "secret://EXA_KEY",
			)
			var args []string
			for i := 0; i < len(full); i++ {
				if full[i] == "--"+tt.Flag {
					i++
					continue
				}
				args = append(args, full[i])
			}
			resolver := &countingResolver{}
			var stderr bytes.Buffer
			_, err := executeWith(t, resolver, strings.NewReader(""), &stderr, args...)
			must.Error(err, "a worker run without --%s succeeded", tt.Flag)
			for _, want := range []string{tt.Field + " is required", "--" + tt.Flag, tt.Env} {
				must.ErrorContains(err, want)
			}
			must.Zero(resolver.calls.Load(), "resolver invoked before the missing endpoint was reported")
		})
	}
	must.Zero(modelSrv.CallCount(), "a worker run missing an endpoint must not dial the model endpoint")
	must.Zero(searchSrv.CallCount(), "a worker run missing an endpoint must not dial the search endpoint")
	must.Empty(kbSrv.Requests(), "a worker run missing an endpoint must not dial the knowledge endpoint")
	must.Empty(exaSrv.Requests(), "a worker run missing an endpoint must not dial the exa endpoint")
}

// TestResearchConfig_RefusesEndpointFlags: research-config never emits a
// fleet endpoint, since its output is the next stage's base config; the
// error points at the final-stage flag and the environment variable.
func TestResearchConfig_RefusesEndpointFlags(t *testing.T) {
	for _, tt := range config.FleetEndpoints(&config.FleetConfig{}) {
		t.Run(tt.Flag, func(t *testing.T) {
			must := require.New(t)
			stdout, _, err := execute(t, "research-config", "--agent", "worker", "--"+tt.Flag, "https://gateway.example/v1")
			must.Error(err, "research-config emitted an endpoint:\n%s", stdout)
			for _, want := range []string{"--" + tt.Flag, tt.Env, "final chiron research stage"} {
				must.ErrorContains(err, want)
			}
			must.Empty(stdout, "research-config must emit nothing when it refuses")
		})
	}
}

// TestWorkerPipeline_EndpointsAtFinalStage: a research-config stage carries the
// worker's key references and caps but no endpoint, and the final research
// stage takes the endpoints from its environment or its own flags.
func TestWorkerPipeline_EndpointsAtFinalStage(t *testing.T) {
	must := require.New(t)
	clearEndpointEnv(t)
	first, _, err := execute(t, "research-config", "--agent", "worker",
		"--fleet-model-name", "test-model",
		"--fleet-model-key-ref", "secret://MODEL_KEY",
		"--fleet-max-turns", "3",
	)
	must.NoError(err, "first stage")
	must.NotContains(first, "_endpoint", "the emitted config names an endpoint:\n%s", first)

	for _, tt := range []struct {
		name string
		env  bool
	}{
		{"environment", true},
		{"final-stage flags", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			modelSrv := modeltest.NewFakeServer(finalReply)
			defer modelSrv.Close()
			searchSrv := searchtest.NewFakeServer(nil)
			defer searchSrv.Close()
			clearEndpointEnv(t)
			args := []string{"research", "--query", "why is the sky blue", "-o", "none"}
			if tt.env {
				t.Setenv(config.EnvFleetModelEndpoint, modelSrv.URL())
				t.Setenv(config.EnvFleetSearchEndpoint, searchSrv.URL())
			} else {
				args = append(args, "--fleet-model-endpoint", modelSrv.URL(), "--fleet-search-endpoint", searchSrv.URL())
			}

			resolver := &countingResolver{}
			var stderr bytes.Buffer
			_, err := executeWith(t, resolver, pipedStdin(t, first), &stderr, args...)
			must.NoError(err, "final stage; stderr: %s", stderr.String())
			must.Equal(1, modelSrv.CallCount(), "the piped worker run must reach the model once")
			must.EqualValues(1, resolver.calls.Load(), "the piped worker run must resolve the model key once")
		})
	}
}

// syncBuffer is a bytes.Buffer a test may read while the command writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// endpointsEvents returns the payloads of the delta events on stderr that
// name the run's endpoints.
func endpointsEvents(t *testing.T, stderr string) []string {
	t.Helper()
	var payloads []string
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var ev transport.Event
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Kind != transport.KindDelta {
			continue
		}
		if strings.HasPrefix(string(ev.Payload), `{"endpoints":`) {
			payloads = append(payloads, string(ev.Payload))
		}
	}
	return payloads
}

// firstRequestProbe forwards every request to a fake and records stderr as
// it stood when the first request arrived.
type firstRequestProbe struct {
	name     string
	server   *httptest.Server
	mu       sync.Mutex
	snapshot *string
}

func newFirstRequestProbe(t *testing.T, name string, stderr *syncBuffer, target string) *firstRequestProbe {
	t.Helper()
	must := require.New(t)
	u, err := url.Parse(target)
	must.NoError(err, "parse %s fake URL", name)
	p := &firstRequestProbe{name: name}
	proxy := httputil.NewSingleHostReverseProxy(u)
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		if p.snapshot == nil {
			s := stderr.String()
			p.snapshot = &s
		}
		p.mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	return p
}

func (p *firstRequestProbe) URL() string { return p.server.URL }

func (p *firstRequestProbe) Close() { p.server.Close() }

// stderrAtFirstRequest returns stderr as it stood when the first request
// arrived, and false when none has.
func (p *firstRequestProbe) stderrAtFirstRequest() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.snapshot == nil {
		return "", false
	}
	return *p.snapshot, true
}

// TestWorker_EndpointsEvent_PrecedesEndpointRequests: a worker run emits
// exactly one delta event naming its model, search and (with a provider)
// knowledge destinations by scheme and host, before the first request
// reaches any of them; a token in an endpoint path never reaches stderr.
func TestWorker_EndpointsEvent_PrecedesEndpointRequests(t *testing.T) {
	const token = "sk-live-0123456789abcdefghijklmn"
	for _, tt := range []struct {
		name      string
		knowledge bool
	}{
		{"with a knowledge provider", true},
		{"without a knowledge provider", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			replies := []modeltest.FakeReply{{Content: `{"action":"search","query":"sky colour"}`, FinishReason: "stop"}}
			if tt.knowledge {
				replies = append(replies, modeltest.FakeReply{Content: `{"action":"recall","query":"sky colour"}`, FinishReason: "stop"})
			}
			modelSrv := modeltest.NewFakeServer(append(replies, finalReply)...)
			defer modelSrv.Close()
			searchSrv := searchtest.NewFakeServer(nil)
			defer searchSrv.Close()
			kbSrv := billettest.NewFakeServer(nil)
			defer kbSrv.Close()
			stderr := &syncBuffer{}
			modelProbe := newFirstRequestProbe(t, "model", stderr, modelSrv.URL())
			defer modelProbe.Close()
			searchProbe := newFirstRequestProbe(t, "search", stderr, searchSrv.URL())
			defer searchProbe.Close()
			kbProbe := newFirstRequestProbe(t, "knowledge", stderr, kbSrv.URL())
			defer kbProbe.Close()
			clearEndpointEnv(t)

			args := []string{
				"research", "--query", "why is the sky blue", "--agent", "worker", "-o", "none",
				"--fleet-model-endpoint", modelProbe.URL() + "/" + token + "/v1",
				"--fleet-model-name", "test-model",
				"--fleet-model-key-ref", "secret://MODEL_KEY",
				"--fleet-search-endpoint", searchProbe.URL() + "/" + token,
			}
			want := `{"endpoints":{"model":"` + modelProbe.URL() + `","search":"` + searchProbe.URL() + `"}}`
			probes := []*firstRequestProbe{modelProbe, searchProbe}
			if tt.knowledge {
				args = append(args, "--fleet-knowledge-provider", "billet",
					"--fleet-knowledge-endpoint", kbProbe.URL()+"/"+token+"/")
				want = `{"endpoints":{"model":"` + modelProbe.URL() + `","search":"` + searchProbe.URL() + `","knowledge":"` + kbProbe.URL() + `"}}`
				probes = append(probes, kbProbe)
			}

			_, err := executeWith(t, &countingResolver{}, strings.NewReader(""), stderr, args...)
			must.NoError(err, "research --agent worker; stderr: %s", stderr.String())
			must.Equal([]string{want}, endpointsEvents(t, stderr.String()), "endpoints events on stderr")
			for _, p := range probes {
				at, ok := p.stderrAtFirstRequest()
				must.True(ok, "the %s endpoint was never called", p.name)
				must.Len(endpointsEvents(t, at), 1, "the endpoints event was not on stderr when the first %s request arrived:\n%s", p.name, at)
			}
			must.NotContains(stderr.String(), token, "an endpoint path reached stderr")
		})
	}
}

// TestEndpointsEvent_CannotBeSuppressed: neither --quiet, a base turning
// streaming off, nor JSON output stops a worker run naming its endpoints on
// stderr.
func TestEndpointsEvent_CannotBeSuppressed(t *testing.T) {
	for _, tt := range []struct {
		name  string
		base  string
		extra []string
	}{
		{"quiet flag", "", []string{"--quiet"}},
		{"base stream false", "agent: worker\nstream: false\noutput: none\n", []string{"--config", "-"}},
		{"json output", "", []string{"-o", "json"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			modelSrv := modeltest.NewFakeServer(finalReply)
			defer modelSrv.Close()
			searchSrv := searchtest.NewFakeServer(nil)
			defer searchSrv.Close()
			clearEndpointEnv(t)

			var stderr bytes.Buffer
			_, err := executeWith(t, &countingResolver{}, strings.NewReader(tt.base), &stderr, workerArgs(modelSrv, searchSrv, tt.extra...)...)
			must.NoError(err, "research --agent worker; stderr: %s", stderr.String())
			must.Len(endpointsEvents(t, stderr.String()), 1, "endpoints events on stderr\n%s", stderr.String())
		})
	}
}
