// Package memory defines the ContextStore seam: agentic memory for
// research runs. Defining the interface now — and *not* implementing it —
// is a deliberate decision (PROPOSAL §5): a single Gemini task needs no
// Chiron-side memory, so v1 binds Noop, and v2 binds an external
// implementation (the Paddock project) without touching the core.
//
// The interface is declared locally rather than imported from paddockapi,
// per PADDOCK §8.4: Paddock satisfies it structurally, so neither project
// hard-depends on the other and Chiron stays buildable without Paddock.
package memory

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned when a Reference resolves to nothing — always,
// in the case of the no-op store.
var ErrNotFound = errors.New("memory: reference not found")

// Namespace scopes every operation. It carries the tenant, so isolation
// is enforced on each call rather than bolted on.
type Namespace string

// Reference is a lightweight, serialisable handle to a stored artifact:
// small enough to pass between agents instead of full payloads (the
// findings-by-reference pattern).
type Reference struct {
	Namespace Namespace `json:"namespace"`
	Digest    string    `json:"digest"`
	Locator   string    `json:"locator,omitempty"`
}

// ArtifactMeta describes a stored artifact.
type ArtifactMeta struct {
	Name      string            `json:"name,omitempty"`
	MediaType string            `json:"media_type,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// SessionRef identifies the short-term namespace for one research run.
type SessionRef struct {
	ID  string        `json:"id"`
	TTL time.Duration `json:"ttl,omitempty"`
}

// Session is a session-scoped, TTL'd namespace. Closing it ends the
// session; the store decides what is garbage-collected or promoted.
type Session interface {
	Namespace() Namespace
	Close(ctx context.Context) error
}

// Memory is one durable, recallable item of long-term memory.
type Memory struct {
	Text string       `json:"text"`
	Meta ArtifactMeta `json:"meta,omitzero"`
}

// Query asks long-term memory for semantically related items.
type Query struct {
	Text  string `json:"text"`
	Limit int    `json:"limit,omitempty"`
}

// Recalled is one result of a Recall query.
type Recalled struct {
	Reference Reference `json:"reference"`
	Memory    Memory    `json:"memory"`
	Score     float64   `json:"score,omitempty"`
}

// ContextStore is the agentic-memory seam: short-term (within-session)
// artifacts written by reference, and long-term (cross-session) memory
// with semantic recall. Chiron stores and recalls; what to remember and
// when to forget are policy, and policy stays with the caller.
type ContextStore interface {
	// OpenSession opens the session-scoped, TTL'd namespace for one run.
	OpenSession(ctx context.Context, ref SessionRef) (Session, error)

	// Put writes an artifact content-addressed and returns its Reference.
	Put(ctx context.Context, ns Namespace, body io.Reader, meta ArtifactMeta) (Reference, error)

	// Get fetches an artifact by Reference.
	Get(ctx context.Context, ref Reference) (io.ReadCloser, ArtifactMeta, error)

	// Remember stores one item of durable long-term memory.
	Remember(ctx context.Context, ns Namespace, m Memory) (Reference, error)

	// Recall retrieves long-term memory semantically related to the query.
	Recall(ctx context.Context, ns Namespace, q Query) ([]Recalled, error)
}
