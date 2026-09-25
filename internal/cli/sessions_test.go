package cli

import (
	"errors"
	"net/http"
	"testing"

	"github.com/rxbynerd/chiron/internal/mcpclient/mcpclienttest"
	"github.com/rxbynerd/chiron/internal/memory/billet/billettest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
)

// mcpFake is the request-counting surface searchtest and billettest share.
type mcpFake interface {
	InitializeCount() int
	ToolCallCount() int
	DeleteCount() int
	Requests() []mcpclienttest.FakeRequest
}

// TestWorkerRunEndsMCPSessions: a worker run holds one search session and one
// Billet session across all its calls, save-back included, and ends each with
// one DELETE when the command shuts down.
func TestWorkerRunEndsMCPSessions(t *testing.T) {
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "A", URL: "https://example.org/a"}}, searchtest.WithSessionID("sess-search"))
	defer searchSrv.Close()
	kbSrv := billettest.NewFakeServer(nil, billettest.WithSessionID("sess-kb"))
	defer kbSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"recall","query":"sky colour"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"search","query":"blue sky"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"final","answer":"Rayleigh scattering.","citations":[]}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")

	_, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "none",
		"--fleet-knowledge-provider", "billet",
		"--fleet-knowledge-endpoint", kbSrv.URL(),
		"--fleet-knowledge-remember",
	)...)
	if err != nil {
		t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr)
	}

	tests := []struct {
		name    string
		fake    mcpFake
		session string
	}{
		{"search", searchSrv, "sess-search"},
		{"billet recall and save-back", kbSrv, "sess-kb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if n := tt.fake.InitializeCount(); n != 1 {
				t.Errorf("initialize count = %d, want 1 across the run", n)
			}
			if n := tt.fake.ToolCallCount(); n != 2 {
				t.Errorf("tools/call count = %d, want 2", n)
			}
			if n := tt.fake.DeleteCount(); n != 1 {
				t.Errorf("DELETE count = %d, want 1 at shutdown", n)
			}
			reqs := tt.fake.Requests()
			if last := reqs[len(reqs)-1]; last.HTTPMethod != http.MethodDelete || last.SessionID != tt.session {
				t.Errorf("last request = %s on %q, want the DELETE of %q", last.HTTPMethod, last.SessionID, tt.session)
			}
		})
	}
}

func TestClosersCloseEveryMemberLastFirst(t *testing.T) {
	var order []string
	failing := errors.New("second failed")
	cs := closers{
		closeFunc(func() error { order = append(order, "first"); return nil }),
		closeFunc(func() error { order = append(order, "second"); return failing }),
	}
	if err := cs.Close(); !errors.Is(err, failing) {
		t.Errorf("Close = %v, want the member's error", err)
	}
	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Errorf("close order = %v, want second then first", order)
	}
}

// closeFunc adapts a function to io.Closer.
type closeFunc func() error

func (f closeFunc) Close() error { return f() }
