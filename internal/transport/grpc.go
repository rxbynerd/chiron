package transport

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotImplemented is returned by seams declared for v2 but not bound
// in v1.
var ErrNotImplemented = errors.New("not implemented in v1 (a v2 seam)")

// GRPC is the v2 Transport seam: the run dials outbound to a control
// plane and streams events over the contract in proto/chiron/v1 — the
// same pattern Stirrup uses (`stirrup job` → CONTROL_PLANE_ADDR), per
// PROPOSAL.md §6. v1 declares the seam only; Emit always fails with
// ErrNotImplemented.
type GRPC struct {
	addr string
}

var _ Transport = (*GRPC)(nil)

// NewGRPC returns the gRPC transport stub for the control plane at
// addr. The stub compiles and satisfies Transport so v2 can bind it
// without touching the core, but it carries no connection.
func NewGRPC(addr string) *GRPC {
	return &GRPC{addr: addr}
}

// Emit always fails: the gRPC transport is a v2 seam.
func (g *GRPC) Emit(_ context.Context, ev Event) error {
	return fmt.Errorf("transport: grpc emit %q to %q: %w", ev.Kind, g.addr, ErrNotImplemented)
}

// Close is a no-op: the stub holds no connection to release.
func (*GRPC) Close() error { return nil }
