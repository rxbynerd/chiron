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
	sess, err := s.OpenSession(context.Background(), ref)
	if err != nil {
		t.Fatalf("OpenSession(%q): %v", ref.ID, err)
	}
	return sess
}

func mustPut(t *testing.T, s *InMemory, ns Namespace, body string, meta ArtifactMeta) Reference {
	t.Helper()
	ref, err := s.Put(context.Background(), ns, strings.NewReader(body), meta)
	if err != nil {
		t.Fatalf("Put(%q): %v", ns, err)
	}
	return ref
}

func mustGet(t *testing.T, s *InMemory, ref Reference) (string, ArtifactMeta) {
	t.Helper()
	rc, meta, err := s.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("Get(%+v): %v", ref, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading artifact: %v", err)
	}
	return string(b), meta
}

func wantNotFound(t *testing.T, s *InMemory, ref Reference) {
	t.Helper()
	if _, _, err := s.Get(context.Background(), ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(%+v) err = %v, want ErrNotFound", ref, err)
	}
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

func TestNewInMemoryDefaults(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts InMemoryOptions
	}{
		{"zero", InMemoryOptions{}},
		{"negative", InMemoryOptions{MaxArtifactBytes: -1, SessionTTL: -time.Second}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := NewInMemory(tt.opts)
			if s.maxBytes != DefaultMaxArtifactBytes || s.ttl != DefaultSessionTTL || s.now == nil {
				t.Errorf("defaults not applied: maxBytes=%d ttl=%s now=%v", s.maxBytes, s.ttl, s.now != nil)
			}
		})
	}
}

func TestInMemoryContentAddressing(t *testing.T) {
	s, _ := newTestStore(InMemoryOptions{})
	sess := mustOpen(t, s, SessionRef{ID: "run-1"})
	if sess.Namespace() != "session/run-1" {
		t.Fatalf("Namespace = %q, want it derived from the id", sess.Namespace())
	}

	for _, tt := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"text", "the finding body"},
		{"multibyte", "Zürich · 東京"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			meta := ArtifactMeta{Name: tt.name, MediaType: "text/markdown", Labels: map[string]string{"worker": "1"}}
			ref := mustPut(t, s, sess.Namespace(), tt.body, meta)
			if ref.Namespace != sess.Namespace() {
				t.Errorf("ref.Namespace = %q, want %q", ref.Namespace, sess.Namespace())
			}
			if ref.Digest != sha256Digest(tt.body) {
				t.Errorf("ref.Digest = %q, want %q", ref.Digest, sha256Digest(tt.body))
			}
			got, gotMeta := mustGet(t, s, ref)
			if got != tt.body {
				t.Errorf("Get body = %q, want %q", got, tt.body)
			}
			if gotMeta.Name != meta.Name || gotMeta.MediaType != meta.MediaType || gotMeta.Labels["worker"] != "1" {
				t.Errorf("Get meta = %+v, want %+v", gotMeta, meta)
			}
		})
	}
}

// TestInMemoryPutIsIdempotent: identical content yields the same Reference
// and the first write's meta survives a second Put.
func TestInMemoryPutIsIdempotent(t *testing.T) {
	s, _ := newTestStore(InMemoryOptions{})
	ns := mustOpen(t, s, SessionRef{ID: "run-1"}).Namespace()

	first := mustPut(t, s, ns, "same content", ArtifactMeta{Name: "first"})
	second := mustPut(t, s, ns, "same content", ArtifactMeta{Name: "second"})
	if first != second {
		t.Fatalf("re-put returned %+v, want %+v", second, first)
	}
	if _, meta := mustGet(t, s, first); meta.Name != "first" {
		t.Errorf("meta.Name = %q after re-put, want the first write's %q", meta.Name, "first")
	}
	if other := mustPut(t, s, ns, "other content", ArtifactMeta{}); other.Digest == first.Digest {
		t.Error("different content produced the same digest")
	}
}

func TestInMemoryGetMissing(t *testing.T) {
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

// TestInMemoryOversizedArtifact: the bound is inclusive, a larger body is a
// typed error with nothing stored, and at most bound+1 bytes are read.
func TestInMemoryOversizedArtifact(t *testing.T) {
	const bound = 16
	s, _ := newTestStore(InMemoryOptions{MaxArtifactBytes: bound})
	ns := mustOpen(t, s, SessionRef{ID: "run-1"}).Namespace()

	atBound := strings.Repeat("a", bound)
	mustPut(t, s, ns, atBound, ArtifactMeta{})

	over := strings.Repeat("b", bound+1)
	if _, err := s.Put(context.Background(), ns, strings.NewReader(over), ArtifactMeta{}); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("oversized Put err = %v, want ErrArtifactTooLarge", err)
	}
	wantNotFound(t, s, Reference{Namespace: ns, Digest: sha256Digest(over)})
	if n := len(s.sessions[ns].artifacts); n != 1 {
		t.Errorf("session holds %d artifacts after a rejected Put, want 1", n)
	}

	src := &countingReader{r: endless{}}
	if _, err := s.Put(context.Background(), ns, src, ArtifactMeta{}); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("endless Put err = %v, want ErrArtifactTooLarge", err)
	}
	if src.n > bound+1 {
		t.Errorf("Put read %d bytes from an endless body, want at most %d", src.n, bound+1)
	}
}

// errReader's Read always fails, simulating a body that errors mid-read.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// TestInMemoryPutBodyReadError: a body read failure is wrapped, not
// swallowed, and nothing is stored.
func TestInMemoryPutBodyReadError(t *testing.T) {
	s, _ := newTestStore(InMemoryOptions{})
	ns := mustOpen(t, s, SessionRef{ID: "run-1"}).Namespace()

	wantErr := errors.New("body read boom")
	if _, err := s.Put(context.Background(), ns, errReader{err: wantErr}, ArtifactMeta{}); !errors.Is(err, wantErr) {
		t.Fatalf("Put err = %v, want it wrapping %v", err, wantErr)
	}
	if n := len(s.sessions[ns].artifacts); n != 0 {
		t.Errorf("session holds %d artifacts after a failed read, want 0", n)
	}
}

// TestInMemoryPutRequiresLiveSession: a namespace that is not an open
// session is rejected before the body is read.
func TestInMemoryPutRequiresLiveSession(t *testing.T) {
	s, _ := newTestStore(InMemoryOptions{})
	closed := mustOpen(t, s, SessionRef{ID: "closed"})
	if err := closed.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, tt := range []struct {
		name string
		ns   Namespace
	}{
		{"never opened", "session/nobody"},
		{"closed", closed.Namespace()},
		{"empty", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := &countingReader{r: strings.NewReader("body")}
			if _, err := s.Put(context.Background(), tt.ns, src, ArtifactMeta{}); !errors.Is(err, ErrSessionNotOpen) {
				t.Fatalf("Put err = %v, want ErrSessionNotOpen", err)
			}
			if src.n != 0 {
				t.Errorf("Put read %d bytes before rejecting the namespace", src.n)
			}
		})
	}

	if _, err := s.Put(context.Background(), "session/x", nil, ArtifactMeta{}); err == nil {
		t.Error("Put accepted a nil body")
	}
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

// TestInMemoryPutRaceWithSessionClose: a session closed while Put's body
// read is in flight is caught by Put's re-check under the lock, so the
// write lands ErrSessionNotOpen instead of writing into (or panicking on)
// the now-nil artifacts map.
func TestInMemoryPutRaceWithSessionClose(t *testing.T) {
	s, _ := newTestStore(InMemoryOptions{})
	sess := mustOpen(t, s, SessionRef{ID: "run-1"})
	ns := sess.Namespace()

	src := &closingReader{r: strings.NewReader("body"), sess: sess}
	if _, err := s.Put(context.Background(), ns, src, ArtifactMeta{}); !errors.Is(err, ErrSessionNotOpen) {
		t.Fatalf("Put err = %v, want ErrSessionNotOpen", err)
	}
}

// TestInMemoryNamespaceIsolation: the same content in two sessions is two
// independent artifacts, and closing one session leaves the other intact.
func TestInMemoryNamespaceIsolation(t *testing.T) {
	s, _ := newTestStore(InMemoryOptions{})
	a := mustOpen(t, s, SessionRef{ID: "a"})
	b := mustOpen(t, s, SessionRef{ID: "b"})

	refA := mustPut(t, s, a.Namespace(), "shared finding", ArtifactMeta{Name: "from a"})
	refB := mustPut(t, s, b.Namespace(), "shared finding", ArtifactMeta{Name: "from b"})
	if refA.Digest != refB.Digest {
		t.Fatalf("same content, different digests: %q vs %q", refA.Digest, refB.Digest)
	}
	if refA.Namespace == refB.Namespace {
		t.Fatalf("two sessions share namespace %q", refA.Namespace)
	}
	if _, meta := mustGet(t, s, refB); meta.Name != "from b" {
		t.Errorf("b's meta = %q, want its own write", meta.Name)
	}

	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close a: %v", err)
	}
	wantNotFound(t, s, refA)
	if got, meta := mustGet(t, s, refB); got != "shared finding" || meta.Name != "from b" {
		t.Errorf("b after closing a = %q, %+v", got, meta)
	}
	mustPut(t, s, b.Namespace(), "still writable", ArtifactMeta{})
}

func TestInMemorySessionClose(t *testing.T) {
	s, _ := newTestStore(InMemoryOptions{})
	sess := mustOpen(t, s, SessionRef{ID: "run-1"})
	ref := mustPut(t, s, sess.Namespace(), "finding", ArtifactMeta{})

	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	wantNotFound(t, s, ref)
	if len(s.sessions) != 0 {
		t.Errorf("store holds %d sessions after Close, want 0", len(s.sessions))
	}

	// The ID is free again; the reopened session starts empty, and the
	// stale handle's Close does not reach it.
	reopened := mustOpen(t, s, SessionRef{ID: "run-1"})
	wantNotFound(t, s, ref)
	fresh := mustPut(t, s, reopened.Namespace(), "second run", ArtifactMeta{})
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("stale Close: %v", err)
	}
	if got, _ := mustGet(t, s, fresh); got != "second run" {
		t.Errorf("reopened session after a stale Close = %q", got)
	}

	// A cancelled context still releases the session.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := reopened.Close(ctx); err != nil {
		t.Fatalf("Close with a cancelled context: %v", err)
	}
	wantNotFound(t, s, fresh)
}

// TestInMemoryTTLExpiry: expiry is absolute from open time, an expired
// session and its artifacts are swept, and its ID can be opened again.
func TestInMemoryTTLExpiry(t *testing.T) {
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
	if _, err := s.Put(context.Background(), short.Namespace(), strings.NewReader("x"), ArtifactMeta{}); !errors.Is(err, ErrSessionNotOpen) {
		t.Errorf("Put to an expired session err = %v, want ErrSessionNotOpen", err)
	}
	if _, live := s.sessions[short.Namespace()]; live {
		t.Error("expired session was not swept")
	}
	mustGet(t, s, longRef)

	clock.advance(50 * time.Minute)
	wantNotFound(t, s, longRef)
	if len(s.sessions) != 0 {
		t.Errorf("store holds %d sessions after both expired, want 0", len(s.sessions))
	}

	again := mustOpen(t, s, SessionRef{ID: "short"})
	wantNotFound(t, s, shortRef)
	if err := short.Close(context.Background()); err != nil {
		t.Fatalf("Close of the expired handle: %v", err)
	}
	mustPut(t, s, again.Namespace(), "new run", ArtifactMeta{})
}

// TestInMemoryDefaultTTL: a zero SessionRef.TTL takes the store default.
func TestInMemoryDefaultTTL(t *testing.T) {
	s, clock := newTestStore(InMemoryOptions{})
	ref := mustPut(t, s, mustOpen(t, s, SessionRef{ID: "run-1"}).Namespace(), "finding", ArtifactMeta{})

	clock.advance(DefaultSessionTTL - time.Nanosecond)
	mustGet(t, s, ref)
	clock.advance(time.Nanosecond)
	wantNotFound(t, s, ref)
}

func TestInMemoryOpenSessionRejects(t *testing.T) {
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
			sess, err := s.OpenSession(context.Background(), tt.ref)
			if err == nil {
				t.Fatalf("OpenSession(%+v) opened %q", tt.ref, sess.Namespace())
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("err = %v, want %v", err, tt.is)
			}
		})
	}
}

// TestInMemoryCopiesState: neither the caller's input nor anything Get
// returns aliases stored state.
func TestInMemoryCopiesState(t *testing.T) {
	s, _ := newTestStore(InMemoryOptions{})
	ns := mustOpen(t, s, SessionRef{ID: "run-1"}).Namespace()

	labels := map[string]string{"worker": "1"}
	ref := mustPut(t, s, ns, "original", ArtifactMeta{Labels: labels})
	labels["worker"] = "mutated by caller"

	rc, meta, err := s.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// io.Copy hands the reader's own slice to Write via WriterTo.
	if _, err := io.Copy(scribbler{}, rc); err != nil {
		t.Fatalf("copy: %v", err)
	}
	meta.Labels["worker"] = "mutated by reader"
	meta.Labels["extra"] = "added"

	got, again := mustGet(t, s, ref)
	if got != "original" {
		t.Errorf("stored body = %q after mutating a read", got)
	}
	if len(again.Labels) != 1 || again.Labels["worker"] != "1" {
		t.Errorf("stored labels = %v, want the Put-time copy", again.Labels)
	}
}

func TestInMemoryLongTermMemoryNotImplemented(t *testing.T) {
	s, _ := newTestStore(InMemoryOptions{})
	if _, err := s.Remember(context.Background(), "session/x", Memory{Text: "fact"}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Remember err = %v, want ErrNotImplemented", err)
	}
	hits, err := s.Recall(context.Background(), "session/x", Query{Text: "fact"})
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Recall err = %v, want ErrNotImplemented", err)
	}
	if hits != nil {
		t.Errorf("Recall hits = %v, want none", hits)
	}
}

func TestInMemoryHonoursCancelledContext(t *testing.T) {
	s, _ := newTestStore(InMemoryOptions{})
	ns := mustOpen(t, s, SessionRef{ID: "run-1"}).Namespace()
	ref := mustPut(t, s, ns, "finding", ArtifactMeta{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.OpenSession(ctx, SessionRef{ID: "run-2"}); !errors.Is(err, context.Canceled) {
		t.Errorf("OpenSession err = %v, want context.Canceled", err)
	}
	if _, err := s.Put(ctx, ns, strings.NewReader("x"), ArtifactMeta{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Put err = %v, want context.Canceled", err)
	}
	if _, _, err := s.Get(ctx, ref); !errors.Is(err, context.Canceled) {
		t.Errorf("Get err = %v, want context.Canceled", err)
	}
}

// TestInMemoryConcurrentUse drives parallel writers and readers across
// sessions while other sessions open and close, for the race detector.
func TestInMemoryConcurrentUse(t *testing.T) {
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
						t.Errorf("Put: %v", err)
						return
					}
					rc, _, err := s.Get(context.Background(), ref)
					if err != nil {
						t.Errorf("Get: %v", err)
						return
					}
					got, _ := io.ReadAll(rc)
					rc.Close()
					if string(got) != body {
						t.Errorf("Get = %q, want %q", got, body)
					}
				}
			})
		}
	}
	wg.Go(func() {
		for i := range puts {
			sess, err := s.OpenSession(context.Background(), SessionRef{ID: fmt.Sprintf("churn-%d", i)})
			if err != nil {
				t.Errorf("OpenSession: %v", err)
				return
			}
			if _, err := s.Put(context.Background(), sess.Namespace(), strings.NewReader("churn"), ArtifactMeta{}); err != nil {
				t.Errorf("churn Put: %v", err)
			}
			if err := sess.Close(context.Background()); err != nil {
				t.Errorf("Close: %v", err)
			}
		}
	})
	wg.Wait()

	for i := range sessions {
		rec := s.sessions[Namespace(fmt.Sprintf("session/run-%d", i))]
		if want := writers*(puts-puts/5) + 1; len(rec.artifacts) != want {
			t.Errorf("run-%d holds %d artifacts, want %d", i, len(rec.artifacts), want)
		}
	}
}
