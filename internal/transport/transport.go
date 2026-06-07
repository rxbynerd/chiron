// Package transport defines the Transport seam: carrying run events out of
// the core. v1 binds stdio (NDJSON); v2 adds an outbound gRPC stream to a
// control plane — the same pattern Stirrup uses — without touching the core.
package transport

import (
	"context"
	"encoding/json"
	"time"
)

// Event is a single run event. Payload is left as raw JSON so the core can
// emit typed payloads without the transport knowing their shapes.
type Event struct {
	Time    time.Time       `json:"time"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Transport carries run events to wherever they are observed: stdout in
// v1, a control plane in v2.
type Transport interface {
	// Emit sends one event. Implementations must not block the run
	// indefinitely; respect ctx cancellation.
	Emit(ctx context.Context, ev Event) error

	// Close flushes and releases the transport.
	Close() error
}
