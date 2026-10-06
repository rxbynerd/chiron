package fleet

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/types"
)

// TestLiveRoutesHasOnlyExternalWeb: the live table has exactly one row, and
// that row is the in-process worker loop.
func TestLiveRoutesHasOnlyExternalWeb(t *testing.T) {
	routes := liveRoutes()
	if got := routes.targets(); !slices.Equal(got, []Target{TargetExternalWeb}) {
		t.Fatalf("live targets = %v, want [%s]", got, TargetExternalWeb)
	}
	dispatch, err := routes.route(TargetExternalWeb)
	if err != nil {
		t.Fatalf("route(%s): %v", TargetExternalWeb, err)
	}
	if reflect.ValueOf(dispatch).Pointer() != reflect.ValueOf(RunWorker).Pointer() {
		t.Error("the external_web row is not RunWorker")
	}
}

// TestRouteExternalWebRunsWorker: the external_web dispatch runs a real
// worker loop against the scripted model and returns its Finding.
func TestRouteExternalWebRunsWorker(t *testing.T) {
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(finalReply("routed answer"))
	defer modelSrv.Close()

	dispatch, err := liveRoutes().route(TargetExternalWeb)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	finding := dispatch(context.Background(), WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	}, Brief{Objective: "routed objective"})

	if finding.Status != types.StatusCompleted || finding.Text != "routed answer" {
		t.Fatalf("finding = %+v, want a completed routed answer", finding)
	}
	if n := modelSrv.CallCount(); n != 1 {
		t.Errorf("model calls = %d, want 1", n)
	}
	reqs := modelSrv.Requests()
	if len(reqs) == 0 || !strings.Contains(reqs[0].Messages[0].Content, "routed objective") {
		t.Error("the worker's system prompt lacks the routed brief's objective")
	}
}

// TestRouteRefusesEveryOtherTarget: anything without a row is refused with
// a typed error, including every name the managed deep-research agent goes
// by, the other agents, near-misses of external_web, and the empty target.
func TestRouteRefusesEveryOtherTarget(t *testing.T) {
	for _, tt := range []struct {
		name   string
		target Target
	}{
		{"gemini", "gemini"},
		{"deep-research", "deep-research"},
		{"deep_research", "deep_research"},
		{"deep-research-max", "deep-research-max"},
		{"gemini-deep-research", "gemini-deep-research"},
		{"managed", "managed"},
		{"worker agent", "worker"},
		{"fleet agent", "fleet"},
		{"internal source", "internal_source"},
		{"repo mcp", "repo_mcp"},
		{"file search", "file_search"},
		{"hyphenated", "external-web"},
		{"upper case", "EXTERNAL_WEB"},
		{"padded", " external_web"},
		{"empty", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dispatch, err := liveRoutes().route(tt.target)
			if dispatch != nil {
				t.Error("route returned a dispatch for a refused target")
			}
			if !errors.Is(err, ErrUnroutableTarget) {
				t.Fatalf("err = %v, want ErrUnroutableTarget", err)
			}
			var unroutable *UnroutableTargetError
			if !errors.As(err, &unroutable) {
				t.Fatalf("err = %T, want *UnroutableTargetError", err)
			}
			if unroutable.Target != tt.target {
				t.Errorf("error target = %q, want %q", unroutable.Target, tt.target)
			}
			if !slices.Equal(unroutable.Routable, []Target{TargetExternalWeb}) {
				t.Errorf("error routable = %v, want [%s]", unroutable.Routable, TargetExternalWeb)
			}
			if !strings.Contains(err.Error(), string(TargetExternalWeb)) {
				t.Errorf("error %q does not name the routable target", err)
			}
		})
	}
}

// TestUnroutableTargetErrorBoundsEcho: a long target is cut in the message
// at a rune boundary.
func TestUnroutableTargetErrorBoundsEcho(t *testing.T) {
	long := Target(strings.Repeat("é", 10*maxTargetEchoRunes))
	_, err := liveRoutes().route(long)
	if err == nil {
		t.Fatal("route accepted an unknown target")
	}
	msg := err.Error()
	if strings.Contains(msg, strings.Repeat("é", maxTargetEchoRunes+1)) {
		t.Errorf("the message echoes more than %d runes of the target: %q", maxTargetEchoRunes, msg)
	}
	if !utf8.ValidString(msg) {
		t.Errorf("the message is not valid UTF-8: %q", msg)
	}
}

// TestRouterAcceptsAnAddedRow: a new target is one row in the same table,
// routed by the same lookup, with no change to the existing row.
func TestRouterAcceptsAnAddedRow(t *testing.T) {
	const internal Target = "internal_source"
	var called Brief
	routes := liveRoutes()
	routes[internal] = func(_ context.Context, _ WorkerDeps, b Brief) Finding {
		called = b
		return Finding{Status: types.StatusCompleted}
	}

	dispatch, err := routes.route(internal)
	if err != nil {
		t.Fatalf("route(%s): %v", internal, err)
	}
	dispatch(context.Background(), WorkerDeps{}, Brief{Objective: "internal"})
	if called.Objective != "internal" {
		t.Errorf("the added row did not receive the brief: %+v", called)
	}
	if got := routes.targets(); !slices.Equal(got, []Target{TargetExternalWeb, internal}) {
		t.Errorf("targets = %v", got)
	}
	if _, err := liveRoutes().route(internal); !errors.Is(err, ErrUnroutableTarget) {
		t.Errorf("a fresh live table routes the added target: %v", err)
	}
}
