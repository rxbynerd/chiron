package fleet

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/types"
)

// newWorker builds a Worker over fakes, failing the test on construction
// error. The model fake is scripted by the caller; search returns the given
// results.
func newWorker(t *testing.T, modelSrv *model.FakeServer, results []search.Result) *Worker {
	t.Helper()
	searchSrv := search.NewFakeServer(results)
	t.Cleanup(searchSrv.Close)
	w, err := NewWorker(WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w
}

// TestWorkerStartAwaitResult drives the researcher seam: Start returns an
// opaque id immediately, Await blocks until the run finishes, and Result maps
// the Finding onto an Interaction with the worker's agent, tools, query, one
// text output, and the cited source.
func TestWorkerStartAwaitResult(t *testing.T) {
	modelSrv := model.NewFakeServer(finalReply("# Answer\n\nblue sky", "https://example.org/sky"))
	defer modelSrv.Close()
	w := newWorker(t, modelSrv, nil)

	ctx := context.Background()
	id, err := w.Start(ctx, researcher.Task{Query: "why is the sky blue"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasPrefix(id, "wkr_") {
		t.Errorf("id = %q, want a local wkr_ handle", id)
	}

	if err := w.Await(ctx, id); err != nil {
		t.Fatalf("Await: %v", err)
	}
	in, err := w.Result(ctx, id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Status != types.StatusCompleted {
		t.Errorf("status = %s (%s), want completed", in.Status, in.StatusDetail)
	}
	if in.Agent != "worker" {
		t.Errorf("agent = %q, want worker", in.Agent)
	}
	if in.Query != "why is the sky blue" {
		t.Errorf("query = %q", in.Query)
	}
	if want := []string{"web_search", "web_fetch"}; strings.Join(in.Tools, ",") != strings.Join(want, ",") {
		t.Errorf("tools = %v, want %v", in.Tools, want)
	}
	if len(in.Outputs) != 1 || in.Outputs[0].Type != types.OutputText || !strings.Contains(in.Outputs[0].Text, "blue sky") {
		t.Errorf("outputs = %+v, want one text output with the answer", in.Outputs)
	}
	if len(in.Citations) != 1 || in.Citations[0].URI != "https://example.org/sky" {
		t.Errorf("citations = %+v, want the cited source", in.Citations)
	}
	if in.CreatedAt.IsZero() || in.CompletedAt.IsZero() {
		t.Errorf("timestamps = %v / %v, want both set", in.CreatedAt, in.CompletedAt)
	}
}

// TestWorkerStartReturnsImmediately: Start must not block on the run (§3). A
// model fake that would block indefinitely on its turn must not stop Start
// from returning the resume handle.
func TestWorkerStartReturnsImmediately(t *testing.T) {
	release := make(chan struct{})
	// A raw server that hangs until released, so any synchronous await inside
	// Start would deadlock the test.
	blockingModel := newBlockingModelServer(t, release)
	defer blockingModel.Close()
	defer close(release)

	mc, err := model.New(model.Options{Endpoint: blockingModel.URL, Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatalf("model.New: %v", err)
	}
	searchSrv := search.NewFakeServer(nil)
	defer searchSrv.Close()
	w, err := NewWorker(WorkerDeps{
		Model:  mc,
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	done := make(chan string, 1)
	go func() {
		id, startErr := w.Start(context.Background(), researcher.Task{Query: "q"})
		if startErr != nil {
			t.Errorf("Start: %v", startErr)
		}
		done <- id
	}()

	select {
	case id := <-done:
		if id == "" {
			t.Error("Start returned an empty id")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return promptly — it must not block on the run")
	}
}

// TestWorkerAwaitRespectsContext: Await returns when its context is cancelled,
// even though the run is still going, rather than blocking forever.
func TestWorkerAwaitRespectsContext(t *testing.T) {
	release := make(chan struct{})
	blockingModel := newBlockingModelServer(t, release)
	defer blockingModel.Close()
	defer close(release)

	mc, err := model.New(model.Options{Endpoint: blockingModel.URL, Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatalf("model.New: %v", err)
	}
	searchSrv := search.NewFakeServer(nil)
	defer searchSrv.Close()
	w, err := NewWorker(WorkerDeps{
		Model:  mc,
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   Caps{MaxTurns: 8, Timeout: 30 * time.Second},
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	id, err := w.Start(context.Background(), researcher.Task{Query: "q"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := w.Await(ctx, id); err == nil {
		t.Fatal("Await must return the context error when cancelled before the run finishes")
	}
}

// TestWorkerRejectsFollowUp: the in-process worker has no stored interaction
// chain, so a follow-up task is rejected at Start before any work.
func TestWorkerRejectsFollowUp(t *testing.T) {
	modelSrv := model.NewFakeServer(finalReply("x"))
	defer modelSrv.Close()
	w := newWorker(t, modelSrv, nil)

	if _, err := w.Start(context.Background(), researcher.Task{Query: "q", PreviousInteractionID: "wkr_prev"}); err == nil {
		t.Fatal("a follow-up must be rejected — the worker has no interaction chain")
	}
}

// TestNewWorkerValidatesDeps: NewWorker fails when a required collaborator is
// missing, so a misconfigured worker never emits a resume handle for a run
// that cannot proceed.
func TestNewWorkerValidatesDeps(t *testing.T) {
	modelSrv := model.NewFakeServer()
	defer modelSrv.Close()
	searchSrv := search.NewFakeServer(nil)
	defer searchSrv.Close()
	good := WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	}
	for _, tt := range []struct {
		name   string
		mutate func(*WorkerDeps)
	}{
		{"nil model", func(d *WorkerDeps) { d.Model = nil }},
		{"nil search", func(d *WorkerDeps) { d.Search = nil }},
		{"nil fetch", func(d *WorkerDeps) { d.Fetch = nil }},
		{"zero turn cap", func(d *WorkerDeps) { d.Caps.MaxTurns = 0 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := good
			tt.mutate(&d)
			if _, err := NewWorker(d); err == nil {
				t.Errorf("%s: NewWorker must fail", tt.name)
			}
		})
	}
}

// TestWorkerScrubsModelCredential pins the credential-scrub guarantee
// (docs/V2-RESEARCH-AGENT §8): drive a failing model call whose 401 body echoes
// the resolved key (as a real provider 401 commonly does), then assert the key
// appears nowhere in the resulting Interaction — not the StatusDetail, not any
// output, not the citations.
func TestWorkerScrubsModelCredential(t *testing.T) {
	// A high-entropy key-shaped literal that both the model client's exact
	// redaction and secret.Scrub's backstop will catch.
	const key = "sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0"
	// The fake echoes the key in its 401 body — the provider-echo path the
	// model adapter warns about.
	modelSrv := model.NewFakeServer(model.FakeReply{
		Status:     http.StatusUnauthorized,
		StatusBody: `{"error":{"message":"invalid api key ` + key + `"}}`,
	})
	defer modelSrv.Close()
	searchSrv := search.NewFakeServer(nil)
	defer searchSrv.Close()

	mc, err := model.New(model.Options{Endpoint: modelSrv.URL(), Model: "m", APIKey: key})
	if err != nil {
		t.Fatalf("model.New: %v", err)
	}
	w, err := NewWorker(WorkerDeps{
		Model:  mc,
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	ctx := context.Background()
	id, err := w.Start(ctx, researcher.Task{Query: "q"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := w.Await(ctx, id); err != nil {
		t.Fatalf("Await: %v", err)
	}
	in, err := w.Result(ctx, id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Status != types.StatusFailed {
		t.Fatalf("status = %s, want failed", in.Status)
	}
	if strings.Contains(in.StatusDetail, key) {
		t.Errorf("the model key leaked into StatusDetail:\n%s", in.StatusDetail)
	}
	for _, out := range in.Outputs {
		if strings.Contains(out.Text, key) {
			t.Errorf("the model key leaked into an output:\n%s", out.Text)
		}
	}
}

// TestWorkerScrubsSearchCredential is the search-path analogue: a search MCP
// that rejects the tools/call with a body echoing the key must not leak it
// into the failed Interaction's StatusDetail.
func TestWorkerScrubsSearchCredential(t *testing.T) {
	const key = "sk-search-Z9y8X7w6V5u4T3s2R1q0P9o8N7m6L5k4J3i2H1g0"
	// A search MCP that answers initialize but rejects tools/call, echoing the
	// key in the error — the provider-echo path on the search side.
	searchSrv := newLeakySearchServer(t, key)
	defer searchSrv.Close()

	sc, err := search.New(search.Options{Endpoint: searchSrv.URL, APIKey: key})
	if err != nil {
		t.Fatalf("search.New: %v", err)
	}
	// The model searches on turn 1, which triggers the leaky tools/call.
	modelSrv := model.NewFakeServer(
		model.FakeReply{Content: `{"action":"search","query":"x"}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()

	w, err := NewWorker(WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: sc,
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	ctx := context.Background()
	id, err := w.Start(ctx, researcher.Task{Query: "q"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := w.Await(ctx, id); err != nil {
		t.Fatalf("Await: %v", err)
	}
	in, err := w.Result(ctx, id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Status != types.StatusFailed {
		t.Fatalf("status = %s (%s), want failed", in.Status, in.StatusDetail)
	}
	if strings.Contains(in.StatusDetail, key) {
		t.Errorf("the search key leaked into StatusDetail:\n%s", in.StatusDetail)
	}
}
