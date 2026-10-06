package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/researcher/fleet"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
)

// writeWorkerTemplate writes body to a fresh template file and returns its
// path.
func writeWorkerTemplate(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "format.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}
	return path
}

// TestWorkerTemplateThroughCLI: --template with --agent worker validates, and
// the rendered block, query substituted, reaches the model's system prompt
// in place of the built-in format.
func TestWorkerTemplateThroughCLI(t *testing.T) {
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
	if err != nil {
		t.Fatalf("research --agent worker --template: %v\nstderr: %s", err, stderr)
	}
	reqs := modelSrv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("model requests = %d, want 1", len(reqs))
	}
	want := "Required output format:\nAnswer \"why is the sky blue\" under a single ## Summary heading.\n\nSource guidance:"
	if system := reqs[0].Messages[0].Content; !strings.Contains(system, want) {
		t.Errorf("system prompt lacks the rendered block %q:\n%s", want, system)
	}
	if !strings.Contains(stdout, "Rayleigh scattering.") {
		t.Errorf("report lacks the answer:\n%s", stdout)
	}
}

// TestWorkerTemplateRejectedBeforeAnyRequest: a template that cannot load
// fails the run at setup, with the usage exit code, before any model or
// search request and before an interaction id is emitted.
func TestWorkerTemplateRejectedBeforeAnyRequest(t *testing.T) {
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
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want an error containing %q", err, tt.want)
			}
			if _, ok := errors.AsType[*ExitError](err); ok {
				t.Errorf("a template failure is a setup error with the usage exit code: %v", err)
			}
			if n := modelSrv.CallCount(); n != 0 {
				t.Errorf("model requests = %d, want 0", n)
			}
			if n := searchSrv.CallCount(); n != 0 {
				t.Errorf("search requests = %d, want 0", n)
			}
			if strings.Contains(stderr, "interaction_created") {
				t.Errorf("an interaction id was emitted for a run that never started:\n%s", stderr)
			}
		})
	}
}
