package fleet

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Target names where the lead routes a brief. The decomposition schema's
// target enum lists exactly the targets in liveRoutes.
type Target string

// TargetExternalWeb routes a brief to the in-process research worker, which
// searches and reads the public web and, when configured, recalls from a
// knowledge store.
const TargetExternalWeb Target = "external_web"

// ErrUnroutableTarget matches every UnroutableTargetError under errors.Is.
var ErrUnroutableTarget = errors.New("fleet: target is not routable")

// maxTargetEchoRunes bounds the refused target an error message repeats,
// since the target can come from a model reply.
const maxTargetEchoRunes = 64

// UnroutableTargetError reports a target with no row in the routing table.
type UnroutableTargetError struct {
	// Target is the refused target, as supplied.
	Target Target
	// Routable lists the targets the table does route, sorted.
	Routable []Target
}

func (e *UnroutableTargetError) Error() string {
	routable := make([]string, len(e.Routable))
	for i, t := range e.Routable {
		routable[i] = string(t)
	}
	return fmt.Sprintf("fleet: target %q is not routable; routable targets: %s",
		boundRunes(string(e.Target), maxTargetEchoRunes), strings.Join(routable, ", "))
}

// Is reports whether target is ErrUnroutableTarget.
func (e *UnroutableTargetError) Is(target error) bool { return target == ErrUnroutableTarget }

// dispatchFunc runs one brief to a Finding. Like RunWorker it never returns
// an error: every outcome is expressed on the Finding.
type dispatchFunc func(ctx context.Context, deps WorkerDeps, brief Brief) Finding

// router is the routing table, one row per live target. A target with no row
// is refused. The managed deep-research agent is a top-level --agent, never a
// row: a fleet run only ever dispatches Chiron's own workers.
type router map[Target]dispatchFunc

// liveRoutes returns the production routing table. Only external_web is
// live; an internal-source target is a new constant plus one row here.
func liveRoutes() router {
	return router{
		TargetExternalWeb: RunWorker,
	}
}

// route returns the dispatch for t, or an *UnroutableTargetError.
func (r router) route(t Target) (dispatchFunc, error) {
	if dispatch, ok := r[t]; ok {
		return dispatch, nil
	}
	return nil, &UnroutableTargetError{Target: t, Routable: r.targets()}
}

// targets returns the routable targets, sorted.
func (r router) targets() []Target {
	return slices.Sorted(maps.Keys(r))
}
