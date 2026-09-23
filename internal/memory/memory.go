// Package memory defines the agentic-memory seam for research runs. It has
// two halves. Recaller and Rememberer are long-term, cross-session memory:
// an external knowledge store (Billet, Alexandria) satisfies them through the
// adapters in this package's subpackages, and the research worker consults
// them as its recall action (docs/KNOWLEDGE.md). The session and artifact
// plane (OpenSession, Put, Get) remains bound to Noop pending the Wave 4
// in-memory store or Paddock (PROPOSAL §5).
//
// The interfaces are declared locally rather than imported from any store's
// own module, per PADDOCK §8.4: a store satisfies them structurally, so
// neither project hard-depends on the other and Chiron stays buildable
// without any of them.
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

// Recaller retrieves long-term memory semantically related to a query. The
// namespace scopes the lookup where the store supports it (Alexandria: a
// space); a store that binds its namespace server-side (Billet) copies the
// value into each Reference and otherwise ignores it.
type Recaller interface {
	Recall(ctx context.Context, ns Namespace, q Query) ([]Recalled, error)
}

// Rememberer stores one item of durable long-term memory and returns a
// Reference to it.
type Rememberer interface {
	Remember(ctx context.Context, ns Namespace, m Memory) (Reference, error)
}

// ContextStore is the full agentic-memory seam: short-term (within-session)
// artifacts written by reference, plus the long-term Rememberer and Recaller
// halves. Chiron stores and recalls; what to remember and when to forget are
// policy, and policy stays with the caller.
type ContextStore interface {
	// OpenSession opens the session-scoped, TTL'd namespace for one run.
	OpenSession(ctx context.Context, ref SessionRef) (Session, error)

	// Put writes an artifact content-addressed and returns its Reference.
	Put(ctx context.Context, ns Namespace, body io.Reader, meta ArtifactMeta) (Reference, error)

	// Get fetches an artifact by Reference.
	Get(ctx context.Context, ref Reference) (io.ReadCloser, ArtifactMeta, error)

	Rememberer
	Recaller
}
