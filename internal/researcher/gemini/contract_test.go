package gemini

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/interactions"
	"github.com/rxbynerd/chiron/internal/researcher"
)

// sparseFile creates a file of exactly size bytes (sparse, so the test
// does not write 50 MiB to disk).
func sparseFile(t *testing.T, name string, size int64) string {
	t.Helper()
	must := require.New(t)
	in := filepath.Join(t.TempDir(), name)
	f, err := os.Create(in)
	must.NoError(err, "creating %s", name)
	must.NoError(f.Truncate(size), "sizing %s", name)
	must.NoError(f.Close(), "closing %s", name)
	return in
}

// TestInputFileOverBound: an --input file one byte over the cap is
// rejected with the path and the limit, not read whole into memory.
func TestInputFileOverBound(t *testing.T) {
	must := require.New(t)
	in := sparseFile(t, "big.txt", maxInputBytes+1)
	_, err := inputPart(in)
	must.Error(err)
	must.ErrorContains(err, in)
	must.ErrorContains(err, fmt.Sprintf("%d-byte limit", int64(maxInputBytes)))
}

// TestInputFileAtBound: the bound is inclusive — exactly maxInputBytes
// is accepted.
func TestInputFileAtBound(t *testing.T) {
	must := require.New(t)
	in := sparseFile(t, "exact.txt", maxInputBytes)
	part, err := inputPart(in)
	must.NoError(err, "a file exactly at the bound must be accepted")
	must.Len(part.Data, maxInputBytes)
}

// TestStartRejectsConcurrentRun: the one-run-at-a-time contract is
// enforced at runtime, not just documented.
func TestStartRejectsConcurrentRun(t *testing.T) {
	must := require.New(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(entered) // the first Start is now in flight
		<-release
		w.Write([]byte(`{"id":"v1_first","status":"in_progress"}`))
	}))
	defer server.Close()
	// Runs before server.Close so a failed assertion cannot leave the first request blocked.
	defer releaseFirst()

	r := newResearcher(t, server)
	firstErr := make(chan error, 1)
	go func() {
		_, err := r.Start(context.Background(), researcher.Task{Query: "first"})
		firstErr <- err
	}()
	<-entered

	_, secondErr := r.Start(context.Background(), researcher.Task{Query: "second"})
	must.ErrorContains(secondErr, "already has a run in flight")

	releaseFirst()
	must.NoError(<-firstErr, "first Start must be unaffected by the refused second")
}

// TestPollCountResetsOnFreshStart: after a completed run, a fresh Start
// on the same instance reports a zero poll count, not the previous
// run's.
func TestPollCountResetsOnFreshStart(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err, "Start")
	must.NoError(r.Await(ctx, id), "Await")
	in, err := r.Result(ctx, id)
	must.NoError(err, "Result")
	must.NotZero(in.Usage.PollCount, "the first run must have polled at least once")

	_, err = r.Start(ctx, researcher.Task{Query: "second"})
	must.NoError(err, "second Start")
	in, err = r.Result(ctx, id)
	must.NoError(err, "Result after fresh Start")
	must.Zero(in.Usage.PollCount, "poll count after a fresh Start")
}

// TestInputPartMIMETypes: the MIME type on every grounding part reaches
// the API verbatim. Each recognised extension, and the content-sniff
// fallback for unknown ones, is asserted in the decoded create-request
// body.
func TestInputPartMIMETypes(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content []byte
		want    string
	}{
		{"jpg extension", "photo.jpg", []byte("jpeg-data"), "image/jpeg"},
		{"jpeg extension", "photo.jpeg", []byte("jpeg-data"), "image/jpeg"},
		{"gif extension", "anim.gif", []byte("gif-data"), "image/gif"},
		{"txt extension", "notes.txt", []byte("plain words"), "text/plain"},
		// Unknown extension and unprintable bytes: the table returns
		// nothing and http.DetectContentType sniffs binary.
		{"unknown extension sniffs binary", "blob.bin", []byte{0x00, 0x01, 0xfe, 0xff}, "application/octet-stream"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			in := filepath.Join(t.TempDir(), tt.file)
			must.NoError(os.WriteFile(in, tt.content, 0o600))

			var body struct {
				Input []interactions.Content `json:"input"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				decodeJSONBody(t, req, &body)
				w.Write([]byte(`{"id":"v1_mime","status":"in_progress"}`))
			}))
			defer server.Close()

			r := newResearcher(t, server, func(o *Options) { o.Inputs = []string{in} })
			_, err := r.Start(context.Background(), researcher.Task{Query: "q"})
			must.NoError(err, "Start")
			must.Len(body.Input, 2, "input parts: want prompt + 1 grounding part")
			must.Equal(tt.want, body.Input[1].MIMEType)
		})
	}
}
