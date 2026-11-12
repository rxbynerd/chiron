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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/memory"
)

// TestLiveBillet proves the adapter speaks to a real Billet server, not only
// to the fake. It runs only when CHIRON_BILLET_BIN names a billet binary,
// which it starts on a free loopback port with the default in-process
// backend.
func TestLiveBillet(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

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
		must.NoError(err, "starting %s", bin)
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
	must.NoError(err, "New")

	const (
		ns    = memory.Namespace("chiron-interop")
		fact1 = "The Chiron interop canary is saffron-coloured."
		fact2 = "Billet stores memories for the Equestrianism suite.\n(Chiron worker finding; interaction wkr_interop; saved 2026-09-24T00:00:00Z)"
	)
	fact := memory.ArtifactMeta{Labels: map[string]string{"kind": "fact"}}
	ref1, err := c.Remember(ctx, ns, memory.Memory{Text: fact1, Meta: fact})
	must.NoError(err, "Remember fact 1")
	ref2, err := c.Remember(ctx, ns, memory.Memory{Text: fact2, Meta: fact})
	must.NoError(err, "Remember fact 2")
	for _, ref := range []memory.Reference{ref1, ref2} {
		check.Equal(ns, ref.Namespace, "reference %+v", ref)
		check.NotEmpty(ref.Digest, "reference %+v", ref)
		check.Equal(LocatorPrefix+ref.Digest, ref.Locator, "reference %+v", ref)
	}
	check.NotEqual(ref1.Digest, ref2.Digest, "both memories got the same id")

	hits, err := c.Recall(ctx, ns, memory.Query{Text: "saffron canary", Limit: 1})
	must.NoError(err, "Recall")
	must.Len(hits, 1, "got %+v", hits)
	hit := hits[0]
	check.Equal(ref1, hit.Reference)
	check.Equal(fact1, hit.Memory.Text)
	check.Equal(fact1, hit.Memory.Meta.Name)
	check.Equal("text/plain", hit.Memory.Meta.MediaType)
	createdAt := hit.Memory.Meta.Labels["created_at"]
	_, err = time.Parse(time.RFC3339, createdAt)
	must.NoError(err, "created_at %q is not RFC 3339", createdAt)
	check.Positive(hit.Score, "score")

	// A multi-line memory recalls under its first line, which is why a saved
	// finding leads with the objective.
	hits, err = c.Recall(ctx, ns, memory.Query{Text: "Equestrianism suite", Limit: 1})
	must.NoError(err, "Recall fact 2")
	must.Len(hits, 1, "got %+v", hits)
	must.Equal(ref2, hits[0].Reference, "got %+v, want the suite fact", hits)
	first, _, _ := strings.Cut(fact2, "\n")
	check.Equal(fact2, hits[0].Memory.Text)
	check.Equal(first, hits[0].Memory.Meta.Name)
}

// freeLoopbackAddr returns a loopback address whose port was free a moment
// ago.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	must := require.New(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(err, "reserving a port")
	addr := l.Addr().String()
	must.NoError(l.Close(), "releasing %s", addr)
	return addr
}

// waitForListener polls addr until it accepts a connection, failing early if
// the server process exits first.
func waitForListener(t *testing.T, addr string, exited <-chan struct{}) {
	t.Helper()
	must := require.New(t)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			must.FailNow("billet exited before listening on " + addr)
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	must.FailNow("billet did not listen on " + addr + " within 10s")
}
