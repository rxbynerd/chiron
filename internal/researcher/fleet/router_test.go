package fleet

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/types"
)

// TestLiveRoutes_HasOnlyExternalWebRow: the live table has exactly one row, and
// that row is the in-process worker loop.
func TestLiveRoutes_HasOnlyExternalWebRow(t *testing.T) {
	must := require.New(t)
	routes := liveRoutes()
	must.Equal([]Target{TargetExternalWeb}, routes.targets(), "live targets")
	dispatch, err := routes.route(TargetExternalWeb)
	must.NoError(err)
	want := reflect.ValueOf(dispatchWorker).Pointer()
	must.Equal(want, reflect.ValueOf(dispatch).Pointer(), "the external_web row is not dispatchWorker")
}

// TestRoute_ExternalWebRunsWorker: the external_web dispatch runs a real
// worker loop against the scripted model and returns its Finding.
func TestRoute_ExternalWebRunsWorker(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(finalReply("routed answer"))
	defer modelSrv.Close()

	dispatch, err := liveRoutes().route(TargetExternalWeb)
	must.NoError(err)
	finding := dispatch(context.Background(), WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	}, Brief{Objective: "routed objective"}, nil)

	must.Equal(types.StatusCompleted, finding.Status)
	must.Equal("routed answer", finding.Text)
	check.Equal(1, modelSrv.CallCount(), "model calls")
	reqs := modelSrv.Requests()
	must.NotEmpty(reqs, "the model received no requests")
	check.Contains(reqs[0].Messages[0].Content, "routed objective",
		"the worker's system prompt lacks the routed brief's objective")
}

// TestDispatchWorker_HonoursProgressGate: the external_web row hands its gate
// to the worker loop, so a released gate or no gate delivers the report, and
// a held gate drops it at the progress deadline without changing the
// Finding.
func TestDispatchWorker_HonoursProgressGate(t *testing.T) {
	released := make(chan struct{})
	close(released)
	for _, tt := range []struct {
		name  string
		gate  <-chan struct{}
		calls int
	}{
		{"released gate", released, 1},
		{"held gate", make(chan struct{}), 0},
		{"no gate", nil, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			searchSrv := searchtest.NewFakeServer(nil)
			defer searchSrv.Close()
			modelSrv := modeltest.NewFakeServer(finalReply("gated answer"))
			defer modelSrv.Close()

			var log progressLog
			finding := dispatchWorker(context.Background(), WorkerDeps{
				Model:                   newModelClient(t, modelSrv),
				Search:                  newSearchClient(t, searchSrv),
				Fetch:                   newFetchClient(t),
				Caps:                    caps(),
				Progress:                log.hook,
				progressTimeoutOverride: 20 * time.Millisecond,
			}, Brief{Objective: "gated objective"}, tt.gate)

			must.Equal(types.StatusCompleted, finding.Status)
			must.Equal("gated answer", finding.Text)
			must.Len(log.snapshot(), tt.calls, "progress deliveries")
		})
	}
}

// TestRoute_RefusesEveryOtherTarget: anything without a row is refused with
// a typed error, including every name the managed deep-research agent goes
// by, the other agents, near-misses of external_web, and the empty target.
func TestRoute_RefusesEveryOtherTarget(t *testing.T) {
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
			must := require.New(t)
			dispatch, err := liveRoutes().route(tt.target)
			must.Nil(dispatch, "route returned a dispatch for a refused target")
			must.ErrorIs(err, ErrUnroutableTarget)
			var unroutable *UnroutableTargetError
			must.ErrorAs(err, &unroutable)
			must.Equal(tt.target, unroutable.Target, "error target")
			must.Equal([]Target{TargetExternalWeb}, unroutable.Routable, "error routable")
			must.ErrorContains(err, string(TargetExternalWeb), "the error does not name the routable target")
		})
	}
}

// TestUnroutableTargetError_BoundsEcho: a long target is cut in the message
// at a rune boundary.
func TestUnroutableTargetError_BoundsEcho(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	longTarget := Target(strings.Repeat("é", 10*maxTargetEchoRunes))
	_, err := liveRoutes().route(longTarget)
	must.Error(err, "route accepted an unknown target")
	msg := err.Error()
	check.NotContains(msg, strings.Repeat("é", maxTargetEchoRunes+1),
		"the message echoes more than %d runes of the target", maxTargetEchoRunes)
	check.True(utf8.ValidString(msg), "the message is not valid UTF-8")
}

// TestRouter_AcceptsAddedRow: a new target is one row in the same table,
// routed by the same lookup, with no change to the existing row.
func TestRouter_AcceptsAddedRow(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const internal Target = "internal_source"
	var called Brief
	routes := liveRoutes()
	routes[internal] = func(_ context.Context, _ WorkerDeps, b Brief, _ <-chan struct{}) Finding {
		called = b
		return Finding{Status: types.StatusCompleted}
	}

	dispatch, err := routes.route(internal)
	must.NoError(err)
	dispatch(context.Background(), WorkerDeps{}, Brief{Objective: "internal"}, nil)
	check.Equal("internal", called.Objective, "the added row did not receive the brief")
	check.Equal([]Target{TargetExternalWeb, internal}, routes.targets(), "targets")
	_, err = liveRoutes().route(internal)
	check.ErrorIs(err, ErrUnroutableTarget, "a fresh live table routes the added target")
}
