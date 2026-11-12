package cli

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

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

// TestWorker_Run_EndsMCPSessions: a worker run holds one search session and one
// Billet session across all its calls, save-back included, and ends each with
// one DELETE when the command shuts down.
func TestWorker_Run_EndsMCPSessions(t *testing.T) {
	must := require.New(t)

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
	must.NoError(err, "research --agent worker, stderr: %s", stderr)

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
			must := require.New(t)
			must.Equal(1, tt.fake.InitializeCount(), "initialize count across the run")
			must.Equal(2, tt.fake.ToolCallCount(), "tools/call count")
			must.Equal(1, tt.fake.DeleteCount(), "DELETE count at shutdown")

			reqs := tt.fake.Requests()
			last := reqs[len(reqs)-1]
			must.Equal(http.MethodDelete, last.HTTPMethod, "last request method")
			must.Equal(tt.session, last.SessionID, "last request session")
		})
	}
}

func TestClosers_Close_ReturnsMemberErrorAndClosesLastFirst(t *testing.T) {
	must := require.New(t)

	var order []string
	failing := errors.New("second failed")
	cs := closers{
		closeFunc(func() error { order = append(order, "first"); return nil }),
		closeFunc(func() error { order = append(order, "second"); return failing }),
	}
	must.ErrorIs(cs.Close(), failing, "Close should return the member's error")
	must.Equal([]string{"second", "first"}, order, "close order")
}

// closeFunc adapts a function to io.Closer.
type closeFunc func() error

func (f closeFunc) Close() error { return f() }
