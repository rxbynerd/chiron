package memory

import (
	"context"
	"io"
)

// Noop is the v1 ContextStore: it stores nothing and finds nothing.
// Writes succeed silently (the body is discarded), reads report
// ErrNotFound, and recall returns no results — so the core can call the
// seam unconditionally while a single Gemini task holds its own context
// server-side.
type Noop struct{}

var _ ContextStore = Noop{}

// OpenSession returns a session whose Close does nothing.
func (Noop) OpenSession(_ context.Context, ref SessionRef) (Session, error) {
	return noopSession{ns: Namespace(ref.ID)}, nil
}

// Put discards the body and returns an empty Reference.
func (Noop) Put(_ context.Context, ns Namespace, body io.Reader, _ ArtifactMeta) (Reference, error) {
	if _, err := io.Copy(io.Discard, body); err != nil {
		return Reference{}, err
	}
	return Reference{Namespace: ns}, nil
}

// Get always reports ErrNotFound: nothing is ever stored.
func (Noop) Get(_ context.Context, _ Reference) (io.ReadCloser, ArtifactMeta, error) {
	return nil, ArtifactMeta{}, ErrNotFound
}

// Remember discards the memory and returns an empty Reference.
func (Noop) Remember(_ context.Context, ns Namespace, _ Memory) (Reference, error) {
	return Reference{Namespace: ns}, nil
}

// Recall returns no results.
func (Noop) Recall(_ context.Context, _ Namespace, _ Query) ([]Recalled, error) {
	return nil, nil
}

type noopSession struct{ ns Namespace }

func (s noopSession) Namespace() Namespace        { return s.ns }
func (noopSession) Close(_ context.Context) error { return nil }
