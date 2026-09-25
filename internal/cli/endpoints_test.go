package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/memory/billet/billettest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/secret"
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

// TestBaseConfigEndpointRefusedBeforeAnySecretResolves: a base config naming
// a fleet endpoint beside its key reference is refused however the base
// arrives, even when a flag sets the same endpoint, and the resolver is never
// invoked nor any endpoint dialled. The flags alone would make a complete
// worker run, so only the provenance check stops it.
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

// TestFlagEndpointsOverBaseKeyRefsResolve is the control for the refusal
// test: the same worker run with the key references in the base and the
// endpoints on flags resolves every reference through the injected resolver
// and sends its key to the flag-named endpoint.
func TestFlagEndpointsOverBaseKeyRefsResolve(t *testing.T) {
	modelSrv := modeltest.NewFakeServer(finalReply)
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	clearEndpointEnv(t)

	const base = "agent: worker\nfleet:\n  model_name: test-model\n  model_key_ref: secret://MODEL_KEY\n  search_key_ref: secret://SEARCH_KEY\n"
	resolver := &countingResolver{}
	var stderr bytes.Buffer
	_, err := executeWith(t, resolver, strings.NewReader(base), &stderr,
		"research", "--query", "why is the sky blue", "--config", "-", "-o", "none",
		"--fleet-model-endpoint", modelSrv.URL(),
		"--fleet-search-endpoint", searchSrv.URL(),
	)
	if err != nil {
		t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr.String())
	}
	if n := resolver.calls.Load(); n != 2 {
		t.Errorf("resolver calls = %d, want 2 (model and search key)", n)
	}
	reqs := modelSrv.Requests()
	if len(reqs) != 1 || reqs[0].Authorization != "Bearer test-resolved-key" {
		t.Errorf("model requests = %+v, want one carrying the resolved key", reqs)
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
