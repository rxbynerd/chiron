package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"sync"
	"time"
)

const (
	// DefaultMaxArtifactBytes bounds one artifact when
	// InMemoryOptions.MaxArtifactBytes is not positive. A finding or a plan
	// is tens of KiB; the bound stops a runaway writer, not a real one.
	DefaultMaxArtifactBytes = 4 << 20
	// DefaultSessionTTL is a session's lifetime when neither SessionRef.TTL
	// nor InMemoryOptions.SessionTTL is positive. It exceeds the hard cap on
	// a run's wall clock (config.MaxTimeout), so a live run's session never
	// expires under it.
	DefaultSessionTTL = 2 * time.Hour
	// MaxSessionIDBytes bounds SessionRef.ID.
	MaxSessionIDBytes = 256

	digestPrefix           = "sha256:"
	sessionNamespacePrefix = "session/"
)

var (
	// ErrArtifactTooLarge is returned by Put when the body exceeds the
	// store's per-artifact bound. Nothing is stored.
	ErrArtifactTooLarge = errors.New("memory: artifact exceeds the size bound")
	// ErrSessionNotOpen is returned by Put when the namespace is not a live
	// session: never opened, closed, or expired.
	ErrSessionNotOpen = errors.New("memory: namespace is not an open session")
	// ErrSessionExists is returned by OpenSession when the ID is already
	// live, so two runs never share a namespace.
	ErrSessionExists = errors.New("memory: session is already open")
)

// InMemoryOptions configures an InMemory store. Non-positive sizes and
// durations, and a nil Now, take the documented defaults.
type InMemoryOptions struct {
	// MaxArtifactBytes bounds one Put body. Default DefaultMaxArtifactBytes.
	MaxArtifactBytes int64
	// SessionTTL is the lifetime of a session opened with a zero
	// SessionRef.TTL. Default DefaultSessionTTL.
	SessionTTL time.Duration
	// Now is the clock sessions expire against. Default time.Now.
	Now func() time.Time
}

// InMemory is the in-process ContextStore: the session and artifact plane
// the fleet passes findings through by reference. Artifacts are
// content-addressed by SHA-256 and immutable, scoped to a session namespace
// derived from the session ID, and dropped when the session closes or its
// absolute TTL passes. Expired sessions are swept lazily on every call.
// Remember and Recall report ErrNotImplemented: long-term memory belongs to
// the knowledge-store adapters. Safe for concurrent use; construct with
// NewInMemory.
type InMemory struct {
	maxBytes int64
	ttl      time.Duration
	now      func() time.Time

	mu       sync.Mutex
	sessions map[Namespace]*sessionRecord
}

var _ ContextStore = (*InMemory)(nil)

type sessionRecord struct {
	expires   time.Time
	artifacts map[string]storedArtifact
}

type storedArtifact struct {
	body []byte
	meta ArtifactMeta
}

// NewInMemory returns an empty store.
func NewInMemory(opts InMemoryOptions) *InMemory {
	s := &InMemory{
		maxBytes: opts.MaxArtifactBytes,
		ttl:      opts.SessionTTL,
		now:      opts.Now,
		sessions: make(map[Namespace]*sessionRecord),
	}
	if s.maxBytes <= 0 {
		s.maxBytes = DefaultMaxArtifactBytes
	}
	if s.ttl <= 0 {
		s.ttl = DefaultSessionTTL
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// OpenSession opens the namespace for ref.ID, expiring at open time plus
// ref.TTL (or the store default when zero). An ID that is already live is
// ErrSessionExists; an expired or closed one may be opened again.
func (s *InMemory) OpenSession(ctx context.Context, ref SessionRef) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ref.ID == "" {
		return nil, errors.New("memory: session id must not be empty")
	}
	if len(ref.ID) > MaxSessionIDBytes {
		return nil, fmt.Errorf("memory: session id is %d bytes, over the %d-byte bound", len(ref.ID), MaxSessionIDBytes)
	}
	if ref.TTL < 0 {
		return nil, fmt.Errorf("memory: session ttl %s is negative", ref.TTL)
	}
	ttl := ref.TTL
	if ttl == 0 {
		ttl = s.ttl
	}
	ns := Namespace(sessionNamespacePrefix + ref.ID)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)
	if _, live := s.sessions[ns]; live {
		return nil, fmt.Errorf("%w: %q", ErrSessionExists, ns)
	}
	rec := &sessionRecord{expires: now.Add(ttl), artifacts: make(map[string]storedArtifact)}
	s.sessions[ns] = rec
	return &inMemorySession{store: s, ns: ns, rec: rec}, nil
}

// Put stores body under ns and returns its Reference, whose Digest is
// "sha256:" plus the lower-case hex SHA-256 of the content. The namespace is
// checked before the body is read. Re-putting identical content returns the
// same Reference and leaves the first write's meta in place.
func (s *InMemory) Put(ctx context.Context, ns Namespace, body io.Reader, meta ArtifactMeta) (Reference, error) {
	if err := ctx.Err(); err != nil {
		return Reference{}, err
	}
	if body == nil {
		return Reference{}, errors.New("memory: put body must not be nil")
	}
	rec, err := s.liveSession(ns)
	if err != nil {
		return Reference{}, err
	}

	content, err := io.ReadAll(io.LimitReader(body, s.maxBytes+1))
	if err != nil {
		return Reference{}, fmt.Errorf("memory: reading artifact: %w", err)
	}
	if int64(len(content)) > s.maxBytes {
		return Reference{}, fmt.Errorf("%w: over %d bytes", ErrArtifactTooLarge, s.maxBytes)
	}
	sum := sha256.Sum256(content)
	ref := Reference{Namespace: ns, Digest: digestPrefix + hex.EncodeToString(sum[:])}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now())
	if s.sessions[ns] != rec {
		return Reference{}, fmt.Errorf("%w: %q", ErrSessionNotOpen, ns)
	}
	if _, stored := rec.artifacts[ref.Digest]; !stored {
		rec.artifacts[ref.Digest] = storedArtifact{body: content, meta: cloneMeta(meta)}
	}
	return ref, nil
}

// Get returns a reader over a private copy of the artifact and a deep copy
// of its meta. A reference that does not resolve in a live session,
// including one naming a different namespace from the one the content was
// stored under, is ErrNotFound.
func (s *InMemory) Get(ctx context.Context, ref Reference) (io.ReadCloser, ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, ArtifactMeta{}, err
	}
	s.mu.Lock()
	s.sweepLocked(s.now())
	var (
		art   storedArtifact
		found bool
	)
	if rec, live := s.sessions[ref.Namespace]; live {
		art, found = rec.artifacts[ref.Digest]
	}
	s.mu.Unlock()
	if !found {
		return nil, ArtifactMeta{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(art.body))), cloneMeta(art.meta), nil
}

// Remember reports ErrNotImplemented: this store holds session artifacts
// only.
func (*InMemory) Remember(context.Context, Namespace, Memory) (Reference, error) {
	return Reference{}, errNoLongTermMemory()
}

// Recall reports ErrNotImplemented: this store holds session artifacts
// only.
func (*InMemory) Recall(context.Context, Namespace, Query) ([]Recalled, error) {
	return nil, errNoLongTermMemory()
}

func errNoLongTermMemory() error {
	return fmt.Errorf("memory: the in-memory store has no long-term memory: %w", ErrNotImplemented)
}

// liveSession returns the record for ns, or ErrSessionNotOpen.
func (s *InMemory) liveSession(ns Namespace) (*sessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now())
	rec, live := s.sessions[ns]
	if !live {
		return nil, fmt.Errorf("%w: %q", ErrSessionNotOpen, ns)
	}
	return rec, nil
}

// sweepLocked drops every session whose expiry is not after now. The
// caller holds mu.
func (s *InMemory) sweepLocked(now time.Time) {
	for ns, rec := range s.sessions {
		if !now.Before(rec.expires) {
			s.dropLocked(ns, rec)
		}
	}
}

// dropLocked removes ns if rec is still its record, and releases the
// artifacts so a retained Session handle does not pin them. The caller
// holds mu.
func (s *InMemory) dropLocked(ns Namespace, rec *sessionRecord) {
	if s.sessions[ns] == rec {
		delete(s.sessions, ns)
	}
	rec.artifacts = nil
}

type inMemorySession struct {
	store *InMemory
	ns    Namespace
	rec   *sessionRecord
}

func (h *inMemorySession) Namespace() Namespace { return h.ns }

// Close drops the session and its artifacts. It is idempotent and ignores
// ctx, so a deferred Close after a cancelled run still releases the
// session; closing a handle whose ID has since been reopened leaves the
// new session alone.
func (h *inMemorySession) Close(context.Context) error {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.dropLocked(h.ns, h.rec)
	return nil
}

func cloneMeta(m ArtifactMeta) ArtifactMeta {
	m.Labels = maps.Clone(m.Labels)
	return m
}
