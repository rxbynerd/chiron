package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/researcher/fleet"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
)

// writeWorkerTemplate writes body to a fresh template file and returns its
// path.
func writeWorkerTemplate(t *testing.T, body string) string {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "format.md")
	must.NoError(os.WriteFile(path, []byte(body), 0o600), "write template")
	return path
}

// TestWorkerTemplate_CLIRendersIntoSystemPrompt: --template with --agent
// worker validates, and the rendered block, query substituted, reaches the
// model's system prompt in place of the built-in format.
func TestWorkerTemplate_CLIRendersIntoSystemPrompt(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(modeltest.FakeReply{
		Content:      `{"action":"final","answer":"## Summary\n\nRayleigh scattering.","citations":[]}`,
		FinishReason: "stop",
	})
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", "test-model-key")
	path := writeWorkerTemplate(t, "Answer \"{{.Query}}\" under a single ## Summary heading.\n")

	stdout, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "text", "--template", path)...)
	must.NoError(err, "stderr: %s", stderr)
	reqs := modelSrv.Requests()
	must.Len(reqs, 1, "model requests")
	want := "Required output format:\nAnswer \"why is the sky blue\" under a single ## Summary heading.\n\nSource guidance:"
	system := reqs[0].Messages[0].Content
	check.Contains(system, want, "system prompt lacks the rendered block")
	check.Contains(stdout, "Rayleigh scattering.", "report lacks the answer")
}

// TestWorkerTemplate_RejectedBeforeAnyRequest: a template that cannot load
// fails the run at setup, with the usage exit code, before any model or
// search request and before an interaction id is emitted.
func TestWorkerTemplate_RejectedBeforeAnyRequest(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{"oversized", strings.Repeat("x", fleet.MaxReportTemplateBytes+1), "byte bound"},
		{"unparsable", "Answer {{.Query", "parsing report template"},
		{"unknown field", "Answer {{.Question}}", "can't evaluate field Question"},
		{"missing file", "", "reading report template"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			searchSrv := searchtest.NewFakeServer(nil)
			defer searchSrv.Close()
			modelSrv := modeltest.NewFakeServer()
			defer modelSrv.Close()
			t.Setenv("MODEL_KEY", "test-model-key")
			path := filepath.Join(t.TempDir(), "absent.md")
			if tt.body != "" {
				path = writeWorkerTemplate(t, tt.body)
			}

			_, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "none", "--template", path)...)
			must.ErrorContains(err, tt.want)
			_, isExitErr := errors.AsType[*ExitError](err)
			must.False(isExitErr, "a template failure is a setup error with the usage exit code: %v", err)
			must.Equal(0, modelSrv.CallCount(), "model requests")
			must.Equal(0, searchSrv.CallCount(), "search requests")
			must.NotContains(stderr, "interaction_created", "an interaction id was emitted for a run that never started")
		})
	}
}
