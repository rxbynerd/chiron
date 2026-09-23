package billet

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/chiron/internal/memory"
)

// TestLiveBillet proves the adapter speaks to a real Billet server, not only
// to the fake. It runs only when CHIRON_BILLET_BIN names a billet binary,
// which it starts on a free loopback port with the default in-process
// backend.
func TestLiveBillet(t *testing.T) {
	bin := os.Getenv("CHIRON_BILLET_BIN")
	if bin == "" {
		t.Skip("set CHIRON_BILLET_BIN to a billet binary to run the Billet interop test")
	}

	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	var output bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "serve", "--listen", addr)
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("starting %s: %v", bin, err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		cancel()
		<-exited
		if t.Failed() {
			t.Logf("billet output:\n%s", output.String())
		}
	})
	waitForListener(t, addr, exited)

	c, err := New(Options{Endpoint: "http://" + addr + "/", RequestTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const (
		ns    = memory.Namespace("chiron-interop")
		fact1 = "The Chiron interop canary is saffron-coloured."
		fact2 = "Billet stores memories for the Equestrianism suite.\n(Chiron worker finding; interaction wkr_interop; saved 2026-09-24T00:00:00Z)"
	)
	fact := memory.ArtifactMeta{Labels: map[string]string{"kind": "fact"}}
	ref1, err := c.Remember(ctx, ns, memory.Memory{Text: fact1, Meta: fact})
	if err != nil {
		t.Fatalf("Remember fact 1: %v", err)
	}
	ref2, err := c.Remember(ctx, ns, memory.Memory{Text: fact2, Meta: fact})
	if err != nil {
		t.Fatalf("Remember fact 2: %v", err)
	}
	for _, ref := range []memory.Reference{ref1, ref2} {
		if ref.Namespace != ns || ref.Digest == "" || ref.Locator != LocatorPrefix+ref.Digest {
			t.Errorf("reference = %+v, want namespace, id and billet:// locator", ref)
		}
	}
	if ref1.Digest == ref2.Digest {
		t.Errorf("both memories got id %q", ref1.Digest)
	}

	hits, err := c.Recall(ctx, ns, memory.Query{Text: "saffron canary", Limit: 1})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1: %+v", len(hits), hits)
	}
	hit := hits[0]
	if hit.Reference != ref1 {
		t.Errorf("hit reference = %+v, want %+v", hit.Reference, ref1)
	}
	if hit.Memory.Text != fact1 || hit.Memory.Meta.Name != fact1 {
		t.Errorf("hit memory = %+v, want the saffron fact as text and name", hit.Memory)
	}
	if hit.Memory.Meta.MediaType != "text/plain" {
		t.Errorf("media type = %q, want text/plain", hit.Memory.Meta.MediaType)
	}
	if _, err := time.Parse(time.RFC3339, hit.Memory.Meta.Labels["created_at"]); err != nil {
		t.Errorf("created_at label %q is not RFC 3339: %v", hit.Memory.Meta.Labels["created_at"], err)
	}
	if hit.Score <= 0 {
		t.Errorf("score = %v, want a positive match score", hit.Score)
	}

	// A multi-line memory recalls under its first line, which is why a saved
	// finding leads with the objective.
	hits, err = c.Recall(ctx, ns, memory.Query{Text: "Equestrianism suite", Limit: 1})
	if err != nil {
		t.Fatalf("Recall fact 2: %v", err)
	}
	if len(hits) != 1 || hits[0].Reference != ref2 {
		t.Fatalf("got %+v, want the suite fact", hits)
	}
	if first, _, _ := strings.Cut(fact2, "\n"); hits[0].Memory.Text != fact2 || hits[0].Memory.Meta.Name != first {
		t.Errorf("hit memory = %+v, want the full text and the first line as name", hits[0].Memory)
	}
}

// freeLoopbackAddr returns a loopback address whose port was free a moment
// ago.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("releasing %s: %v", addr, err)
	}
	return addr
}

// waitForListener polls addr until it accepts a connection, failing early if
// the server process exits first.
func waitForListener(t *testing.T, addr string, exited <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			t.Fatalf("billet exited before listening on %s", addr)
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("billet did not listen on %s within 10s", addr)
}
