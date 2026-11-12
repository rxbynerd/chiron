package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testClock is an injectable clock the tests advance by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestStore(opts InMemoryOptions) (*InMemory, *testClock) {
	clock := newTestClock()
	opts.Now = clock.now
	return NewInMemory(opts), clock
}

func mustOpen(t *testing.T, s *InMemory, ref SessionRef) Session {
	t.Helper()
	must := require.New(t)
	sess, err := s.OpenSession(context.Background(), ref)
	must.NoError(err, "OpenSession(%q)", ref.ID)
	return sess
}

func mustPut(t *testing.T, s *InMemory, ns Namespace, body string, meta ArtifactMeta) Reference {
	t.Helper()
	must := require.New(t)
	ref, err := s.Put(context.Background(), ns, strings.NewReader(body), meta)
	must.NoError(err, "Put(%q)", ns)
	return ref
}

func mustGet(t *testing.T, s *InMemory, ref Reference) (string, ArtifactMeta) {
	t.Helper()
	must := require.New(t)
	rc, meta, err := s.Get(context.Background(), ref)
	must.NoError(err, "Get(%+v)", ref)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	must.NoError(err, "reading artifact")
	return string(b), meta
}

func wantNotFound(t *testing.T, s *InMemory, ref Reference) {
	t.Helper()
	must := require.New(t)
	_, _, err := s.Get(context.Background(), ref)
	must.ErrorIs(err, ErrNotFound, "Get(%+v)", ref)
}

func sha256Digest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// countingReader records how many bytes were read from it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// scribbler overwrites every slice it is asked to write, breaking the
// io.Writer contract the way a faulty caller could.
type scribbler struct{}

func (scribbler) Write(p []byte) (int, error) {
	for i := range p {
		p[i] = 'X' //nolint:staticcheck // Deliberate: proves Get's reader does not alias stored state.
	}
	return len(p), nil
}

// endless yields 'x' forever.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestInMemory_New_AppliesDefaults(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts InMemoryOptions
	}{
		{"zero", InMemoryOptions{}},
		{"negative", InMemoryOptions{MaxArtifactBytes: -1, SessionTTL: -time.Second}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			s := NewInMemory(tt.opts)
			must.EqualValues(DefaultMaxArtifactBytes, s.maxBytes, "max artifact bytes")
			must.Equal(DefaultSessionTTL, s.ttl, "session ttl")
			must.NotNil(s.now, "clock")
		})
	}
}

func TestInMemory_Put_ContentAddressing(t *testing.T) {
	must := require.New(t)
	s, _ := newTestStore(InMemoryOptions{})
	sess := mustOpen(t, s, SessionRef{ID: "run-1"})
	must.Equal(Namespace("session/run-1"), sess.Namespace())

	for _, tt := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"text", "the finding body"},
		{"multibyte", "Zürich · 東京"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			meta := ArtifactMeta{Name: tt.name, MediaType: "text/markdown", Labels: map[string]string{"worker": "1"}}
			ref := mustPut(t, s, sess.Namespace(), tt.body, meta)
			must.Equal(sess.Namespace(), ref.Namespace, "namespace")
			must.Equal(sha256Digest(tt.body), ref.Digest, "digest")
			got, gotMeta := mustGet(t, s, ref)
			must.Equal(tt.body, got, "body")
			must.Equal(meta.Name, gotMeta.Name, "name")
			must.Equal(meta.MediaType, gotMeta.MediaType, "media type")
			must.Equal("1", gotMeta.Labels["worker"], "label worker")
		})
	}
}

// TestInMemory_Put_IsIdempotent: identical content yields the same Reference
// and the first write's meta survives a second Put.
func TestInMemory_Put_IsIdempotent(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	s, _ := newTestStore(InMemoryOptions{})
	ns := mustOpen(t, s, SessionRef{ID: "run-1"}).Namespace()

	first := mustPut(t, s, ns, "same content", ArtifactMeta{Name: "first"})
	second := mustPut(t, s, ns, "same content", ArtifactMeta{Name: "second"})
	must.Equal(first, second, "re-put returns the first reference")

	_, meta := mustGet(t, s, first)
	check.Equal("first", meta.Name, "meta name after re-put")

	other := mustPut(t, s, ns, "other content", ArtifactMeta{})
	check.NotEqual(first.Digest, other.Digest, "digest of different content")
}

func TestInMemory_Get_MissingReferenceNotFound(t *testing.T) {
	s, _ := newTestStore(InMemoryOptions{})
	a := mustOpen(t, s, SessionRef{ID: "a"}).Namespace()
	b := mustOpen(t, s, SessionRef{ID: "b"}).Namespace()
	stored := mustPut(t, s, a, "only in a", ArtifactMeta{})

	for _, tt := range []struct {
		name string
		ref  Reference
	}{
		{"unknown digest", Reference{Namespace: a, Digest: sha256Digest("never stored")}},
		{"unknown namespace", Reference{Namespace: "session/nobody", Digest: stored.Digest}},
		{"empty reference", Reference{}},
		{"digest under another live namespace", Reference{Namespace: b, Digest: stored.Digest}},
		{"bare namespace id", Reference{Namespace: "a", Digest: stored.Digest}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wantNotFound(t, s, tt.ref)
		})
	}
}

// TestInMemory_Put_RejectsOversizedArtifact: the bound is inclusive, a larger
// body is a typed error with nothing stored, and at most bound+1 bytes are read.
func TestInMemory_Put_RejectsOversizedArtifact(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const bound = 16
	s, _ := newTestStore(InMemoryOptions{MaxArtifactBytes: bound})
	ns := mustOpen(t, s, SessionRef{ID: "run-1"}).Namespace()

	atBound := strings.Repeat("a", bound)
	mustPut(t, s, ns, atBound, ArtifactMeta{})

	over := strings.Repeat("b", bound+1)
	_, err := s.Put(context.Background(), ns, strings.NewReader(over), ArtifactMeta{})
	must.ErrorIs(err, ErrArtifactTooLarge, "Put of an oversized body")
	wantNotFound(t, s, Reference{Namespace: ns, Digest: sha256Digest(over)})
	check.Len(s.sessions[ns].artifacts, 1, "artifacts after a rejected Put")

	src := &countingReader{r: endless{}}
	_, err = s.Put(context.Background(), ns, src, ArtifactMeta{})
	must.ErrorIs(err, ErrArtifactTooLarge, "Put of an endless body")
	check.LessOrEqual(src.n, bound+1, "bytes read from an endless body")
}

// errReader's Read always fails, simulating a body that errors mid-read.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// TestInMemory_Put_BodyReadErrorStoresNothing: a body read failure is wrapped,
// not swallowed, and nothing is stored.
func TestInMemory_Put_BodyReadErrorStoresNothing(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	s, _ := newTestStore(InMemoryOptions{})
	ns := mustOpen(t, s, SessionRef{ID: "run-1"}).Namespace()

	wantErr := errors.New("body read boom")
	_, err := s.Put(context.Background(), ns, errReader{err: wantErr}, ArtifactMeta{})
	must.ErrorIs(err, wantErr, "Put err")
	check.Empty(s.sessions[ns].artifacts, "artifacts after a failed read")
}

// TestInMemory_Put_RequiresLiveSession: a namespace that is not an open
// session is rejected before the body is read.
func TestInMemory_Put_RequiresLiveSession(t *testing.T) {
	must := require.New(t)
	s, _ := newTestStore(InMemoryOptions{})
	closed := mustOpen(t, s, SessionRef{ID: "closed"})
	must.NoError(closed.Close(context.Background()), "Close")

	for _, tt := range []struct {
		name string
		ns   Namespace
	}{
		{"never opened", "session/nobody"},
		{"closed", closed.Namespace()},
		{"empty", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			src := &countingReader{r: strings.NewReader("body")}
			_, err := s.Put(context.Background(), tt.ns, src, ArtifactMeta{})
			must.ErrorIs(err, ErrSessionNotOpen)
			must.Zero(src.n, "bytes read before rejecting the namespace")
		})
	}

	_, err := s.Put(context.Background(), "session/x", nil, ArtifactMeta{})
	must.Error(err, "Put accepted a nil body")
}

// closingReader closes sess on its first Read, simulating a session
// closed while a Put targeting it has already passed the initial
// liveSession check and is mid-way through reading its body.
type closingReader struct {
	r    io.Reader
	sess Session
}

func (c *closingReader) Read(p []byte) (int, error) {
	if c.sess != nil {
		sess := c.sess
		c.sess = nil
		if err := sess.Close(context.Background()); err != nil {
			return 0, err
		}
	}
	return c.r.Read(p)
}

// TestInMemory_Put_SessionClosedMidRead: a session closed while Put's body
// read is in flight is caught by Put's re-check under the lock, so the
// write lands ErrSessionNotOpen instead of writing into (or panicking on)
// the now-nil artifacts map.
func TestInMemory_Put_SessionClosedMidRead(t *testing.T) {
	must := require.New(t)
	s, _ := newTestStore(InMemoryOptions{})
	sess := mustOpen(t, s, SessionRef{ID: "run-1"})
	ns := sess.Namespace()

	src := &closingReader{r: strings.NewReader("body"), sess: sess}
	_, err := s.Put(context.Background(), ns, src, ArtifactMeta{})
	must.ErrorIs(err, ErrSessionNotOpen, "Put err")
}

// TestInMemory_Sessions_AreIsolated: the same content in two sessions is two
// independent artifacts, and closing one session leaves the other intact.
func TestInMemory_Sessions_AreIsolated(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	s, _ := newTestStore(InMemoryOptions{})
	a := mustOpen(t, s, SessionRef{ID: "a"})
	b := mustOpen(t, s, SessionRef{ID: "b"})

	refA := mustPut(t, s, a.Namespace(), "shared finding", ArtifactMeta{Name: "from a"})
	refB := mustPut(t, s, b.Namespace(), "shared finding", ArtifactMeta{Name: "from b"})
	must.Equal(refA.Digest, refB.Digest, "digest of the same content")
	must.NotEqual(refA.Namespace, refB.Namespace, "namespace of each session")
	_, meta := mustGet(t, s, refB)
	check.Equal("from b", meta.Name, "b's meta")

	must.NoError(a.Close(context.Background()), "Close a")
	wantNotFound(t, s, refA)
	got, meta := mustGet(t, s, refB)
	check.Equal("shared finding", got, "b's body after closing a")
	check.Equal("from b", meta.Name, "b's meta after closing a")
	mustPut(t, s, b.Namespace(), "still writable", ArtifactMeta{})
}

func TestInMemory_Session_CloseReleasesArtifacts(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	s, _ := newTestStore(InMemoryOptions{})
	sess := mustOpen(t, s, SessionRef{ID: "run-1"})
	ref := mustPut(t, s, sess.Namespace(), "finding", ArtifactMeta{})

	must.NoError(sess.Close(context.Background()), "Close")
	must.NoError(sess.Close(context.Background()), "second Close")
	wantNotFound(t, s, ref)
	check.Empty(s.sessions, "sessions held after Close")

	// The ID is free again; the reopened session starts empty, and the
	// stale handle's Close does not reach it.
	reopened := mustOpen(t, s, SessionRef{ID: "run-1"})
	wantNotFound(t, s, ref)
	fresh := mustPut(t, s, reopened.Namespace(), "second run", ArtifactMeta{})
	must.NoError(sess.Close(context.Background()), "stale Close")
	got, _ := mustGet(t, s, fresh)
	check.Equal("second run", got, "reopened session after a stale Close")

	// A cancelled context still releases the session.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	must.NoError(reopened.Close(ctx), "Close with a cancelled context")
	wantNotFound(t, s, fresh)
}

// TestInMemory_Session_TTLExpiry: expiry is absolute from open time, an expired
// session and its artifacts are swept, and its ID can be opened again.
func TestInMemory_Session_TTLExpiry(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	s, clock := newTestStore(InMemoryOptions{SessionTTL: time.Hour})
	short := mustOpen(t, s, SessionRef{ID: "short", TTL: 10 * time.Minute})
	long := mustOpen(t, s, SessionRef{ID: "long"})
	shortRef := mustPut(t, s, short.Namespace(), "short-lived", ArtifactMeta{})
	longRef := mustPut(t, s, long.Namespace(), "long-lived", ArtifactMeta{})

	clock.advance(10*time.Minute - time.Second)
	// Activity just before expiry does not extend the session.
	mustPut(t, s, short.Namespace(), "late write", ArtifactMeta{})
	mustGet(t, s, shortRef)

	clock.advance(time.Second)
	wantNotFound(t, s, shortRef)
	_, err := s.Put(context.Background(), short.Namespace(), strings.NewReader("x"), ArtifactMeta{})
	must.ErrorIs(err, ErrSessionNotOpen, "Put to an expired session")
	_, live := s.sessions[short.Namespace()]
	check.False(live, "expired session was swept")
	mustGet(t, s, longRef)

	clock.advance(50 * time.Minute)
	wantNotFound(t, s, longRef)
	check.Empty(s.sessions, "sessions held after both expired")

	again := mustOpen(t, s, SessionRef{ID: "short"})
	wantNotFound(t, s, shortRef)
	must.NoError(short.Close(context.Background()), "Close of the expired handle")
	mustPut(t, s, again.Namespace(), "new run", ArtifactMeta{})
}

// TestInMemory_Session_DefaultTTL: a zero SessionRef.TTL takes the store default.
func TestInMemory_Session_DefaultTTL(t *testing.T) {
	s, clock := newTestStore(InMemoryOptions{})
	ref := mustPut(t, s, mustOpen(t, s, SessionRef{ID: "run-1"}).Namespace(), "finding", ArtifactMeta{})

	clock.advance(DefaultSessionTTL - time.Nanosecond)
	mustGet(t, s, ref)
	clock.advance(time.Nanosecond)
	wantNotFound(t, s, ref)
}

func TestInMemory_OpenSession_Rejects(t *testing.T) {
	s, _ := newTestStore(InMemoryOptions{})
	mustOpen(t, s, SessionRef{ID: "live"})
	mustOpen(t, s, SessionRef{ID: strings.Repeat("i", MaxSessionIDBytes)})

	for _, tt := range []struct {
		name string
		ref  SessionRef
		is   error
	}{
		{"empty id", SessionRef{}, nil},
		{"id over the bound", SessionRef{ID: strings.Repeat("i", MaxSessionIDBytes+1)}, nil},
		{"negative ttl", SessionRef{ID: "neg", TTL: -time.Second}, nil},
		{"duplicate live id", SessionRef{ID: "live"}, ErrSessionExists},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			_, err := s.OpenSession(context.Background(), tt.ref)
			must.Error(err, "OpenSession(%+v) should be rejected", tt.ref)
			if tt.is != nil {
				must.ErrorIs(err, tt.is)
			}
		})
	}
}

// TestInMemory_State_IsNotAliased: neither the caller's input nor anything Get
// returns aliases stored state.
func TestInMemory_State_IsNotAliased(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	s, _ := newTestStore(InMemoryOptions{})
	ns := mustOpen(t, s, SessionRef{ID: "run-1"}).Namespace()

	labels := map[string]string{"worker": "1"}
	ref := mustPut(t, s, ns, "original", ArtifactMeta{Labels: labels})
	labels["worker"] = "mutated by caller"

	rc, meta, err := s.Get(context.Background(), ref)
	must.NoError(err, "Get")
	// io.Copy hands the reader's own slice to Write via WriterTo.
	_, err = io.Copy(scribbler{}, rc)
	must.NoError(err, "copy")
	meta.Labels["worker"] = "mutated by reader"
	meta.Labels["extra"] = "added"

	got, again := mustGet(t, s, ref)
	check.Equal("original", got, "stored body after mutating a read")
	check.Equal(map[string]string{"worker": "1"}, again.Labels, "stored labels")
}

func TestInMemory_LongTermMemory_NotImplemented(t *testing.T) {
	must := require.New(t)
	s, _ := newTestStore(InMemoryOptions{})
	_, err := s.Remember(context.Background(), "session/x", Memory{Text: "fact"})
	must.ErrorIs(err, ErrNotImplemented, "Remember")
	hits, err := s.Recall(context.Background(), "session/x", Query{Text: "fact"})
	must.ErrorIs(err, ErrNotImplemented, "Recall")
	must.Nil(hits, "Recall hits")
}

func TestInMemory_Operations_HonourCancelledContext(t *testing.T) {
	must := require.New(t)
	s, _ := newTestStore(InMemoryOptions{})
	ns := mustOpen(t, s, SessionRef{ID: "run-1"}).Namespace()
	ref := mustPut(t, s, ns, "finding", ArtifactMeta{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.OpenSession(ctx, SessionRef{ID: "run-2"})
	must.ErrorIs(err, context.Canceled, "OpenSession")
	_, err = s.Put(ctx, ns, strings.NewReader("x"), ArtifactMeta{})
	must.ErrorIs(err, context.Canceled, "Put")
	_, _, err = s.Get(ctx, ref)
	must.ErrorIs(err, context.Canceled, "Get")
}

// TestInMemory_Concurrent_ParallelUse drives parallel writers and readers across
// sessions while other sessions open and close, for the race detector.
func TestInMemory_Concurrent_ParallelUse(t *testing.T) {
	check := assert.New(t)
	s := NewInMemory(InMemoryOptions{})
	const (
		sessions = 4
		writers  = 8
		puts     = 25
	)
	var wg sync.WaitGroup
	for i := range sessions {
		ns := mustOpen(t, s, SessionRef{ID: fmt.Sprintf("run-%d", i)}).Namespace()
		for w := range writers {
			wg.Go(func() {
				for p := range puts {
					body := fmt.Sprintf("worker %d finding %d", w, p)
					if p%5 == 0 {
						body = "shared finding"
					}
					ref, err := s.Put(context.Background(), ns, strings.NewReader(body), ArtifactMeta{Labels: map[string]string{"w": fmt.Sprint(w)}})
					if err != nil {
						check.NoError(err, "Put")
						return
					}
					rc, _, err := s.Get(context.Background(), ref)
					if err != nil {
						check.NoError(err, "Get")
						return
					}
					got, _ := io.ReadAll(rc)
					rc.Close()
					check.Equal(body, string(got), "Get body")
				}
			})
		}
	}
	wg.Go(func() {
		for i := range puts {
			sess, err := s.OpenSession(context.Background(), SessionRef{ID: fmt.Sprintf("churn-%d", i)})
			if err != nil {
				check.NoError(err, "OpenSession")
				return
			}
			_, err = s.Put(context.Background(), sess.Namespace(), strings.NewReader("churn"), ArtifactMeta{})
			check.NoError(err, "churn Put")
			check.NoError(sess.Close(context.Background()), "Close")
		}
	})
	wg.Wait()

	for i := range sessions {
		t.Run(fmt.Sprintf("run-%d", i), func(t *testing.T) {
			must := require.New(t)
			rec := s.sessions[Namespace(fmt.Sprintf("session/run-%d", i))]
			// Each worker writes puts-puts/5 distinct findings, and every worker
			// writes the same shared finding, which is stored once.
			want := writers*(puts-puts/5) + 1
			must.Len(rec.artifacts, want)
		})
	}
}
