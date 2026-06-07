package gemini

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/researcher"
)

// sparseFile creates a file of exactly size bytes (sparse, so the test
// does not write 50 MiB to disk).
func sparseFile(t *testing.T, name string, size int64) string {
	t.Helper()
	in := filepath.Join(t.TempDir(), name)
	f, err := os.Create(in)
	if err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatalf("sizing %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing %s: %v", name, err)
	}
	return in
}

// TestInputFileOverBound pins C1-CODE-1: an --input file one byte over
// the cap is rejected with the path and the limit, not read whole into
// memory.
func TestInputFileOverBound(t *testing.T) {
	in := sparseFile(t, "big.txt", maxInputBytes+1)
	_, err := inputPart(in)
	if err == nil {
		t.Fatal("a file over the input bound must be rejected")
	}
	if !strings.Contains(err.Error(), in) {
		t.Errorf("error %q must name the offending file", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d-byte limit", int64(maxInputBytes))) {
		t.Errorf("error %q must name the limit", err)
	}
}

// TestInputFileAtBound: the bound is inclusive — exactly maxInputBytes
// is accepted.
func TestInputFileAtBound(t *testing.T) {
	in := sparseFile(t, "exact.txt", maxInputBytes)
	part, err := inputPart(in)
	if err != nil {
		t.Fatalf("a file exactly at the bound must be accepted: %v", err)
	}
	if int64(len(part.Data)) != int64(maxInputBytes) {
		t.Errorf("read %d bytes, want %d", len(part.Data), int64(maxInputBytes))
	}
}

// TestStartRejectsConcurrentRun pins C1-CODE-2: the one-run-at-a-time
// contract is enforced at runtime, not just documented.
func TestStartRejectsConcurrentRun(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(entered) // the first Start is now in flight
		<-release
		w.Write([]byte(`{"id":"v1_first","status":"in_progress"}`))
	}))
	defer server.Close()

	r := newResearcher(t, server)
	firstErr := make(chan error, 1)
	go func() {
		_, err := r.Start(context.Background(), researcher.Task{Query: "first"})
		firstErr <- err
	}()
	<-entered

	if _, err := r.Start(context.Background(), researcher.Task{Query: "second"}); err == nil ||
		!strings.Contains(err.Error(), "already has a run in flight") {
		t.Errorf("second Start = %v, want the in-flight refusal", err)
	}

	close(release)
	if err := <-firstErr; err != nil {
		t.Errorf("first Start must be unaffected by the refused second: %v", err)
	}
}

// TestPollCountResetsOnFreshStart: after a completed run, a fresh Start
// on the same instance reports a zero poll count, not the previous
// run's.
func TestPollCountResetsOnFreshStart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			w.Write([]byte(`{"id":"v1_reuse","status":"in_progress"}`))
			return
		}
		w.Write([]byte(`{"id":"v1_reuse","status":"completed"}`))
	}))
	defer server.Close()

	r := newResearcher(t, server)
	ctx := context.Background()
	id, err := r.Start(ctx, researcher.Task{Query: "first"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Await(ctx, id); err != nil {
		t.Fatalf("Await: %v", err)
	}
	in, err := r.Result(ctx, id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if in.Usage.PollCount == 0 {
		t.Fatal("the first run must have polled at least once")
	}

	if _, err := r.Start(ctx, researcher.Task{Query: "second"}); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	in, err = r.Result(ctx, id)
	if err != nil {
		t.Fatalf("Result after fresh Start: %v", err)
	}
	if in.Usage.PollCount != 0 {
		t.Errorf("poll count = %d after a fresh Start, want 0", in.Usage.PollCount)
	}
}
