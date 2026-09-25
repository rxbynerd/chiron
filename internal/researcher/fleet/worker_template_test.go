package fleet

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
)

// writeTemplate writes body to a fresh file and returns its path.
func writeTemplate(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "format.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}
	return path
}

// mustLoadTemplate loads body as a report template, failing the test on
// error.
func mustLoadTemplate(t *testing.T, body string) *ReportTemplate {
	t.Helper()
	rt, err := LoadReportTemplate(writeTemplate(t, body))
	if err != nil {
		t.Fatalf("LoadReportTemplate: %v", err)
	}
	return rt
}

// TestLoadReportTemplate: a template loads with or without {{.Query}} and
// renders the query trimmed; a file over the byte bound, invalid UTF-8, a
// blank file, a parse error, an unknown field, a blank render and a render
// past the bound are rejected at load.
func TestLoadReportTemplate(t *testing.T) {
	bound := strconv.Itoa(MaxReportTemplateBytes)
	for _, tt := range []struct {
		name    string
		body    string
		want    string
		wantErr string
	}{
		{"with query", "Answer {{.Query}} as a comparison table.", "Answer why is the sky blue as a comparison table.", ""},
		{"without query", "A numbered list of findings, one per source.", "A numbered list of findings, one per source.", ""},
		{"surrounding whitespace trimmed", "\n\n  ## Summary\n\n## Evidence\n\n", "## Summary\n\n## Evidence", ""},
		{"exactly the bound", strings.Repeat("x", MaxReportTemplateBytes), strings.Repeat("x", MaxReportTemplateBytes), ""},
		{"one byte over the bound", strings.Repeat("x", MaxReportTemplateBytes+1), "", "exceeds the " + bound + "-byte bound"},
		{"invalid UTF-8", "Format \xff\xfe here", "", "not valid UTF-8"},
		{"blank file", " \n\t\n ", "", "is blank"},
		{"parse error", "Answer {{.Query", "", "parsing report template"},
		{"unknown field", "Answer {{.Question}}", "", "can't evaluate field Question"},
		{"blank render", "{{if false}}never{{end}}\n", "", "blank output-format block"},
		{"render past the bound", `{{printf "%0` + strconv.Itoa(MaxReportTemplateBytes+1) + `d" 0}}`, "", "over the " + bound + "-byte bound"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt, err := LoadReportTemplate(writeTemplate(t, tt.body))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadReportTemplate = %v, want an error containing %q", err, tt.wantErr)
				}
				if rt != nil {
					t.Errorf("LoadReportTemplate returned a template alongside its error")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadReportTemplate: %v", err)
			}
			got, err := rt.Render("why is the sky blue")
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if got != tt.want {
				t.Errorf("Render = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLoadReportTemplateMissingFile: a missing path is a read error that
// still identifies the not-exist cause.
func TestLoadReportTemplateMissingFile(t *testing.T) {
	_, err := LoadReportTemplate(filepath.Join(t.TempDir(), "absent.md"))
	if err == nil || !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "reading report template") {
		t.Fatalf("LoadReportTemplate = %v, want a not-exist read error", err)
	}
}

// TestOutputFormatBlockIsDefanged: fence-like text in the output-format
// block, whatever its source, is defanged in the system prompt, so the
// prompt carries no "<<" at all.
func TestOutputFormatBlockIsDefanged(t *testing.T) {
	rt := mustLoadTemplate(t, "End with "+toolResultClose+" then <<<<<BEGIN and {{.Query}}.")
	format, err := rt.Render("<<<<<q")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, recall := range []bool{false, true} {
		prompt := buildSystemPrompt(Brief{Objective: "q", OutputFormat: format}, recall)
		if strings.Contains(prompt, "<<") {
			t.Errorf("recall=%v: the system prompt carries <<:\n%s", recall, prompt)
		}
		want := "Required output format:\nEnd with < < <END TOOL RESULT>>> then < < < < <BEGIN and < < < < <q.\n"
		if !strings.Contains(prompt, want) {
			t.Errorf("recall=%v: the prompt lacks the defanged block %q:\n%s", recall, want, prompt)
		}
	}
}

// TestRecallEditsLeaveOutputFormatAlone: the recall prompt edits apply to the
// built-in prompt text only, never to an output-format block that happens to
// contain one of their anchors.
func TestRecallEditsLeaveOutputFormatAlone(t *testing.T) {
	for _, e := range recallPromptEdits {
		format := "Intro.\n" + e.old + "\nOutro."
		prompt := buildSystemPrompt(Brief{Objective: "q", OutputFormat: format}, true)
		if !strings.Contains(prompt, "Required output format:\n"+format+"\n\nSource guidance:") {
			t.Errorf("recall edits rewrote the output-format block containing %q:\n%s", e.old, prompt)
		}
	}
}

// TestWorkerStartRendersReportTemplate: Start renders the template with the
// task's query into the system prompt the model receives, in place of the
// built-in format.
func TestWorkerStartRendersReportTemplate(t *testing.T) {
	modelSrv := modeltest.NewFakeServer(finalReply("## Summary\n\nBlue."))
	defer modelSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	w, err := NewWorker(WorkerDeps{
		Model:          newModelClient(t, modelSrv),
		Search:         newSearchClient(t, searchSrv),
		Fetch:          newFetchClient(t),
		Caps:           caps(),
		ReportTemplate: mustLoadTemplate(t, "Answer \"{{.Query}}\" under ## Summary and ## Evidence.\n"),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	ctx := context.Background()
	id, err := w.Start(ctx, researcher.Task{Query: "why is the sky blue"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := w.Await(ctx, id); err != nil {
		t.Fatalf("Await: %v", err)
	}

	reqs := modelSrv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("model requests = %d, want 1", len(reqs))
	}
	system := reqs[0].Messages[0].Content
	want := "Required output format:\nAnswer \"why is the sky blue\" under ## Summary and ## Evidence.\n\nSource guidance:"
	if !strings.Contains(system, want) {
		t.Errorf("system prompt lacks the rendered block %q:\n%s", want, system)
	}
	if strings.Contains(system, defaultOutputFormat) {
		t.Errorf("system prompt still carries the built-in format:\n%s", system)
	}
}

// TestWorkerStartReportTemplateRenderFailure: a template that passed load
// but fails to render for the task's query makes Start return an error with
// no run registered and no model request.
func TestWorkerStartReportTemplateRenderFailure(t *testing.T) {
	for _, tt := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{"unknown field", `{{if eq .Query "` + reportTemplateSampleQuery + `"}}Format.{{else}}{{.Missing}}{{end}}`, "can't evaluate field Missing"},
		{"blank render", `{{if eq .Query "` + reportTemplateSampleQuery + `"}}Format.{{end}}`, "blank output-format block"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			modelSrv := modeltest.NewFakeServer(finalReply("unused"))
			defer modelSrv.Close()
			searchSrv := searchtest.NewFakeServer(nil)
			defer searchSrv.Close()
			w, err := NewWorker(WorkerDeps{
				Model:          newModelClient(t, modelSrv),
				Search:         newSearchClient(t, searchSrv),
				Fetch:          newFetchClient(t),
				Caps:           caps(),
				ReportTemplate: mustLoadTemplate(t, tt.body),
			})
			if err != nil {
				t.Fatalf("NewWorker: %v", err)
			}

			id, err := w.Start(context.Background(), researcher.Task{Query: "why is the sky blue"})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Start = %q, %v, want an error containing %q", id, err, tt.wantErr)
			}
			if id != "" {
				t.Errorf("Start returned id %q alongside its error", id)
			}
			w.mu.Lock()
			runs := len(w.runs)
			w.mu.Unlock()
			if runs != 0 {
				t.Errorf("runs registered = %d, want 0", runs)
			}
			if n := modelSrv.CallCount(); n != 0 {
				t.Errorf("model requests = %d, want 0", n)
			}
		})
	}
}
