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

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/memory/billet/billettest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
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

// TestBaseConfigEndpointRefusedBeforeAnySecretResolves: a base naming a fleet
// endpoint and its key reference is refused however it arrives, even with the
// same endpoint on a flag, before the resolver runs or anything is dialled.
// The flags alone make a complete worker run.
func TestBaseConfigEndpointRefusedBeforeAnySecretResolves(t *testing.T) {
	modelSrv := modeltest.NewFakeServer(finalReply)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	kbSrv := billettest.NewFakeServer(nil)
	defer kbSrv.Close()
	clearEndpointEnv(t)

	endpoints := []struct {
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
	}
	sources := []struct {
		name  string
		stdin func(t *testing.T, base string) io.Reader
		args  func(t *testing.T, base string) []string
	}{
		{"config file",
			func(*testing.T, string) io.Reader { return strings.NewReader("") },
			func(t *testing.T, base string) []string {
				path := filepath.Join(t.TempDir(), "base.yaml")
				if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
					t.Fatal(err)
				}
				return []string{"--config", path}
			}},
		{"config dash", func(_ *testing.T, base string) io.Reader { return strings.NewReader(base) },
			func(*testing.T, string) []string { return []string{"--config", "-"} }},
		{"piped stdin", func(t *testing.T, base string) io.Reader { return pipedStdin(t, base) },
			func(*testing.T, string) []string { return nil }},
	}

	for _, ep := range endpoints {
		for _, src := range sources {
			t.Run(ep.name+"/"+src.name, func(t *testing.T) {
				resolver := &countingResolver{}
				args := append(workerArgs(modelSrv, searchSrv, "-o", "none",
					"--fleet-knowledge-provider", "billet",
					"--fleet-knowledge-endpoint", kbSrv.URL(),
				), src.args(t, ep.base)...)
				var stderr bytes.Buffer
				_, err := executeWith(t, resolver, src.stdin(t, ep.base), &stderr, args...)
				if err == nil {
					t.Fatal("a base config naming an endpoint was accepted")
				}
				for _, want := range []string{ep.field + ":", "credentials are sent", ep.env} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q lacks %q", err, want)
					}
				}
				if n := resolver.calls.Load(); n != 0 {
					t.Errorf("resolver invoked %d times; a refused base must stop the run before any secret resolves", n)
				}
			})
		}
	}
	if modelSrv.CallCount() != 0 || searchSrv.CallCount() != 0 || len(kbSrv.Requests()) != 0 {
		t.Error("a refused base config must not dial any endpoint")
	}
}

// TestEveryCommandRefusesBaseEndpointBeforeResolving: research on either
// Gemini tier or the worker, get and follow-up all refuse a base naming a
// fleet endpoint beside a key reference, before any secret resolves or any
// endpoint is dialled, without echoing the endpoint.
func TestEveryCommandRefusesBaseEndpointBeforeResolving(t *testing.T) {
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
			resolver := &countingResolver{}
			var stderr bytes.Buffer
			_, err := executeWith(t, resolver, strings.NewReader(base), &stderr, append(tt.args, "--config", "-")...)
			if err == nil {
				t.Fatal("a base config naming an endpoint was accepted")
			}
			if !strings.Contains(err.Error(), "fleet.model_endpoint: must not come from a base config") {
				t.Errorf("err = %v, want the base-config endpoint refusal", err)
			}
			if n := resolver.calls.Load(); n != 0 {
				t.Errorf("resolver invoked %d times; a refused base must stop the command before any secret resolves", n)
			}
			if strings.Contains(err.Error()+stderr.String(), "attacker.example") {
				t.Errorf("the refused endpoint was echoed: %v\nstderr: %s", err, stderr.String())
			}
		})
	}
	if geminiCalls.Load() != 0 || modelSrv.CallCount() != 0 || searchSrv.CallCount() != 0 {
		t.Error("a refused base config must not dial any endpoint")
	}
}

// TestFlagEndpointsOverBaseKeyRefsResolve is the control for the refusal
// test: the same worker run with the key references in the base and the
// endpoints on flags resolves every reference, knowledge included, through
// the injected resolver and sends its key to the flag-named endpoint.
func TestFlagEndpointsOverBaseKeyRefsResolve(t *testing.T) {
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
	if err != nil {
		t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr.String())
	}
	if n := resolver.calls.Load(); n != 3 {
		t.Errorf("resolver calls = %d, want 3 (model, search and knowledge key)", n)
	}
	reqs := modelSrv.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model requests = %d, want the recall turn and the final turn", len(reqs))
	}
	for i, req := range reqs {
		if req.Authorization != "Bearer test-resolved-key" {
			t.Errorf("model request %d Authorization = %q, want the resolved key", i, req.Authorization)
		}
	}
	if kbSrv.ToolCallCount() != 1 {
		t.Fatalf("knowledge tool calls = %d, want the one recall", kbSrv.ToolCallCount())
	}
	for i, req := range kbSrv.Requests() {
		if req.Authorization != "Bearer test-resolved-key" {
			t.Errorf("knowledge request %d Authorization = %q, want the resolved key", i, req.Authorization)
		}
	}
}

// TestFleetEndpointsFromEnvironment: CHIRON_FLEET_*_ENDPOINT supplies an
// endpoint whose flag is unset, an explicitly set flag wins over it, and the
// knowledge variable is consulted only with a knowledge provider.
func TestFleetEndpointsFromEnvironment(t *testing.T) {
	t.Run("env supplies every endpoint", func(t *testing.T) {
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
		if err != nil {
			t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr.String())
		}
		if modelSrv.CallCount() != 2 {
			t.Errorf("model calls = %d, want 2 through the env-named endpoint", modelSrv.CallCount())
		}
		if kbSrv.ToolCallCount() != 1 {
			t.Errorf("knowledge tool calls = %d, want the recall through the env-named endpoint", kbSrv.ToolCallCount())
		}
	})

	t.Run("flag wins over env", func(t *testing.T) {
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
		if err != nil {
			t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr.String())
		}
		if flagSrv.CallCount() != 1 || envSrv.CallCount() != 0 {
			t.Errorf("calls: flag endpoint %d, env endpoint %d; want the flag to win", flagSrv.CallCount(), envSrv.CallCount())
		}
	})

	t.Run("knowledge env unread without a provider", func(t *testing.T) {
		modelSrv := modeltest.NewFakeServer(finalReply)
		defer modelSrv.Close()
		searchSrv := searchtest.NewFakeServer(nil)
		defer searchSrv.Close()
		clearEndpointEnv(t)
		t.Setenv(config.EnvFleetKnowledgeEndpoint, "http://kb.example/")

		var stderr bytes.Buffer
		_, err := executeWith(t, &countingResolver{}, strings.NewReader(""), &stderr,
			workerArgs(modelSrv, searchSrv, "-o", "none")...)
		if err != nil {
			t.Fatalf("an unused knowledge variable failed the run: %v", err)
		}
	})
}

// TestFleetEndpointEnvRejectedWithoutEcho: an invalid CHIRON_FLEET_*_ENDPOINT
// fails the run before any secret resolves or endpoint is dialled, naming
// the variable but never echoing its path, query or credentials.
func TestFleetEndpointEnvRejectedWithoutEcho(t *testing.T) {
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
	} {
		t.Run(tt.name, func(t *testing.T) {
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
			if err == nil || !strings.Contains(err.Error(), tt.env) {
				t.Fatalf("err = %v, want it to name %s", err, tt.env)
			}
			if strings.Contains(err.Error()+stderr.String(), token) {
				t.Errorf("the rejected value leaked: %v\nstderr: %s", err, stderr.String())
			}
			if n := resolver.calls.Load(); n != 0 {
				t.Errorf("resolver invoked %d times before the variable was validated", n)
			}
		})
	}
	if modelSrv.CallCount() != 0 || searchSrv.CallCount() != 0 {
		t.Error("an invalid endpoint variable must not let the run dial anything")
	}
}

// TestMissingEndpointErrorNamesFlagAndVariable: a worker run missing any
// fleet endpoint fails before any secret resolves, and the error names the
// flag and the environment variable that can supply it.
func TestMissingEndpointErrorNamesFlagAndVariable(t *testing.T) {
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	kbSrv := billettest.NewFakeServer(nil)
	defer kbSrv.Close()

	for _, tt := range config.FleetEndpoints(&config.FleetConfig{}) {
		t.Run(tt.Flag, func(t *testing.T) {
			clearEndpointEnv(t)
			full := workerArgs(modelSrv, searchSrv, "-o", "none",
				"--fleet-knowledge-provider", "billet",
				"--fleet-knowledge-endpoint", kbSrv.URL(),
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
			if err == nil {
				t.Fatalf("a worker run without --%s succeeded", tt.Flag)
			}
			for _, want := range []string{tt.Field + " is required", "--" + tt.Flag, tt.Env} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if n := resolver.calls.Load(); n != 0 {
				t.Errorf("resolver invoked %d times before the missing endpoint was reported", n)
			}
		})
	}
	if modelSrv.CallCount() != 0 || searchSrv.CallCount() != 0 || len(kbSrv.Requests()) != 0 {
		t.Error("a worker run missing an endpoint must not dial any endpoint")
	}
}

// TestResearchConfigRefusesEndpointFlags: research-config never emits a
// fleet endpoint, since its output is the next stage's base config; the
// error points at the final-stage flag and the environment variable.
func TestResearchConfigRefusesEndpointFlags(t *testing.T) {
	for _, e := range config.FleetEndpoints(&config.FleetConfig{}) {
		t.Run(e.Flag, func(t *testing.T) {
			stdout, _, err := execute(t, "research-config", "--agent", "worker", "--"+e.Flag, "https://gateway.example/v1")
			if err == nil {
				t.Fatalf("research-config emitted an endpoint:\n%s", stdout)
			}
			for _, want := range []string{"--" + e.Flag, e.Env, "final chiron research stage"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing emitted", stdout)
			}
		})
	}
}

// TestWorkerPipelineTakesEndpointsAtTheFinalStage: a research-config stage
// carries the worker's key references and caps but no endpoint, and the final
// research stage takes the endpoints from its environment or its own flags.
func TestWorkerPipelineTakesEndpointsAtTheFinalStage(t *testing.T) {
	clearEndpointEnv(t)
	first, _, err := execute(t, "research-config", "--agent", "worker",
		"--fleet-model-name", "test-model",
		"--fleet-model-key-ref", "secret://MODEL_KEY",
		"--fleet-max-turns", "3",
	)
	if err != nil {
		t.Fatalf("first stage: %v", err)
	}
	if strings.Contains(first, "_endpoint") {
		t.Fatalf("the emitted config names an endpoint:\n%s", first)
	}

	for _, tt := range []struct {
		name string
		env  bool
	}{
		{"environment", true},
		{"final-stage flags", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
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
			if _, err := executeWith(t, resolver, pipedStdin(t, first), &stderr, args...); err != nil {
				t.Fatalf("final stage: %v\nstderr: %s", err, stderr.String())
			}
			if modelSrv.CallCount() != 1 || resolver.calls.Load() != 1 {
				t.Errorf("model calls %d, resolver calls %d; want the piped worker run to reach the model once", modelSrv.CallCount(), resolver.calls.Load())
			}
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
	u, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse %s fake URL: %v", name, err)
	}
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

// TestWorkerEndpointsEventPrecedesEveryEndpointRequest: a worker run emits
// exactly one delta event naming its model, search and (with a provider)
// knowledge destinations by scheme and host, before the first request
// reaches any of them; a token in an endpoint path never reaches stderr.
func TestWorkerEndpointsEventPrecedesEveryEndpointRequest(t *testing.T) {
	const token = "sk-live-0123456789abcdefghijklmn"
	for _, tt := range []struct {
		name      string
		knowledge bool
	}{
		{"with a knowledge provider", true},
		{"without a knowledge provider", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
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

			if _, err := executeWith(t, &countingResolver{}, strings.NewReader(""), stderr, args...); err != nil {
				t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr.String())
			}
			got := endpointsEvents(t, stderr.String())
			if len(got) != 1 || got[0] != want {
				t.Fatalf("endpoints events = %q, want exactly [%s]", got, want)
			}
			for _, p := range probes {
				at, ok := p.stderrAtFirstRequest()
				if !ok {
					t.Errorf("the %s endpoint was never called", p.name)
					continue
				}
				if len(endpointsEvents(t, at)) != 1 {
					t.Errorf("the endpoints event was not on stderr when the first %s request arrived:\n%s", p.name, at)
				}
			}
			if strings.Contains(stderr.String(), token) {
				t.Errorf("an endpoint path reached stderr:\n%s", stderr.String())
			}
		})
	}
}

// TestEndpointsEventCannotBeSuppressed: neither --quiet, a base turning
// streaming off, nor JSON output stops a worker run naming its endpoints on
// stderr.
func TestEndpointsEventCannotBeSuppressed(t *testing.T) {
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
			modelSrv := modeltest.NewFakeServer(finalReply)
			defer modelSrv.Close()
			searchSrv := searchtest.NewFakeServer(nil)
			defer searchSrv.Close()
			clearEndpointEnv(t)

			var stderr bytes.Buffer
			_, err := executeWith(t, &countingResolver{}, strings.NewReader(tt.base), &stderr, workerArgs(modelSrv, searchSrv, tt.extra...)...)
			if err != nil {
				t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr.String())
			}
			if n := len(endpointsEvents(t, stderr.String())); n != 1 {
				t.Errorf("endpoints events = %d, want 1\nstderr: %s", n, stderr.String())
			}
		})
	}
}
