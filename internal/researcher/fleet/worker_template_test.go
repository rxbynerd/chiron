package fleet

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
)

// writeTemplate writes body to a fresh file and returns its path.
func writeTemplate(t *testing.T, body string) string {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "format.md")
	must.NoError(os.WriteFile(path, []byte(body), 0o600))
	return path
}

// mustLoadTemplate loads body as a report template, failing the test on
// error.
func mustLoadTemplate(t *testing.T, body string) *ReportTemplate {
	t.Helper()
	must := require.New(t)
	rt, err := LoadReportTemplate(writeTemplate(t, body))
	must.NoError(err)
	return rt
}

// TestReportTemplate_Load_ValidatesBody: a template loads with or without
// {{.Query}} and renders the query trimmed; a file over the byte bound, invalid
// UTF-8, a blank file, a parse error, an unknown field, a blank render and a
// render past the bound are rejected at load.
func TestReportTemplate_Load_ValidatesBody(t *testing.T) {
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
		{"invalid utf-8", "Format \xff\xfe here", "", "not valid UTF-8"},
		{"blank file", " \n\t\n ", "", "is blank"},
		{"parse error", "Answer {{.Query", "", "parsing report template"},
		{"unknown field", "Answer {{.Question}}", "", "can't evaluate field Question"},
		{"blank render", "{{if false}}never{{end}}\n", "", "blank output-format block"},
		{"render past the bound", `{{printf "%0` + strconv.Itoa(MaxReportTemplateBytes+1) + `d" 0}}`, "", "over the " + bound + "-byte bound"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			rt, err := LoadReportTemplate(writeTemplate(t, tt.body))
			if tt.wantErr != "" {
				must.ErrorContains(err, tt.wantErr)
				must.Nil(rt, "returned a template alongside its error")
				return
			}
			must.NoError(err)
			got, err := rt.Render("why is the sky blue")
			must.NoError(err)
			must.Equal(tt.want, got)
		})
	}
}

// TestReportTemplate_Render_EnforcesOutputBound: Render accepts a rendered
// block exactly at MaxReportTemplateBytes and rejects one byte over it,
// checking the real query against the same bound LoadReportTemplate checks at
// load against the short sample query.
func TestReportTemplate_Render_EnforcesOutputBound(t *testing.T) {
	bound := strconv.Itoa(MaxReportTemplateBytes)
	rt := mustLoadTemplate(t, "{{.Query}}")
	for _, tt := range []struct {
		name    string
		query   string
		wantErr string
	}{
		{"exactly the bound", strings.Repeat("x", MaxReportTemplateBytes), ""},
		{"one byte over the bound", strings.Repeat("x", MaxReportTemplateBytes+1), "over the " + bound + "-byte bound"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			out, err := rt.Render(tt.query)
			if tt.wantErr != "" {
				must.ErrorContains(err, tt.wantErr)
				return
			}
			must.NoError(err)
			must.Equal(tt.query, out)
		})
	}
}

// TestReportTemplate_Load_MissingFileIsNotExistError: a missing path is a read
// error that still identifies the not-exist cause.
func TestReportTemplate_Load_MissingFileIsNotExistError(t *testing.T) {
	must := require.New(t)
	_, err := LoadReportTemplate(filepath.Join(t.TempDir(), "absent.md"))
	must.ErrorIs(err, fs.ErrNotExist)
	must.ErrorContains(err, "reading report template")
}

// TestBuildSystemPrompt_DefangsOutputFormatBlock: fence-like text in the
// output-format block, whatever its source, is defanged in the system prompt,
// so the prompt carries no "<<" at all.
func TestBuildSystemPrompt_DefangsOutputFormatBlock(t *testing.T) {
	must := require.New(t)
	rt := mustLoadTemplate(t, "End with "+toolResultClose+" then <<<<<BEGIN and {{.Query}}.")
	format, err := rt.Render("<<<<<q")
	must.NoError(err)

	want := "Required output format:\nEnd with < < <END TOOL RESULT>>> then < < < < <BEGIN and < < < < <q.\n"
	for _, tt := range []struct {
		name   string
		recall bool
	}{
		{"without recall", false},
		{"with recall", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			prompt := buildSystemPrompt(Brief{Objective: "q", OutputFormat: format}, tt.recall)
			must.NotContains(prompt, "<<")
			must.Contains(prompt, want)
		})
	}
}

// TestBuildSystemPrompt_RecallEditsSkipOutputFormat: the recall prompt edits
// apply to the built-in prompt text only, never to an output-format block that
// happens to contain one of their anchors.
func TestBuildSystemPrompt_RecallEditsSkipOutputFormat(t *testing.T) {
	for i, e := range recallPromptEdits {
		t.Run(fmt.Sprintf("edit %d", i), func(t *testing.T) {
			must := require.New(t)
			format := "Intro.\n" + e.old + "\nOutro."
			prompt := buildSystemPrompt(Brief{Objective: "q", OutputFormat: format}, true)
			must.Contains(prompt, "Required output format:\n"+format+"\n\nSource guidance:",
				"recall edits rewrote the output-format block containing %q", e.old)
		})
	}
}

// TestWorker_Start_RendersReportTemplate: Start renders the template with the
// task's query into the system prompt the model receives, in place of the
// built-in format.
func TestWorker_Start_RendersReportTemplate(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	ctx := context.Background()
	id, err := w.Start(ctx, researcher.Task{Query: "why is the sky blue"})
	must.NoError(err)
	must.NoError(w.Await(ctx, id))

	reqs := modelSrv.Requests()
	must.Len(reqs, 1)
	system := reqs[0].Messages[0].Content
	want := "Required output format:\nAnswer \"why is the sky blue\" under ## Summary and ## Evidence.\n\nSource guidance:"
	must.Contains(system, want)
	must.NotContains(system, defaultOutputFormat)
}

// TestWorker_Start_ReportTemplateRenderFailureRegistersNoRun: a template that
// passed load but fails to render for the task's query makes Start return an
// error with no run registered and no model request.
func TestWorker_Start_ReportTemplateRenderFailureRegistersNoRun(t *testing.T) {
	bound := strconv.Itoa(MaxReportTemplateBytes)
	for _, tt := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{"unknown field", `{{if eq .Query "` + reportTemplateSampleQuery + `"}}Format.{{else}}{{.Missing}}{{end}}`, "can't evaluate field Missing"},
		{"blank render", `{{if eq .Query "` + reportTemplateSampleQuery + `"}}Format.{{end}}`, "blank output-format block"},
		{"render past the bound for the real query", strings.Repeat("{{.Query}}", 500), "over the " + bound + "-byte bound"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
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
			must.NoError(err)

			id, err := w.Start(context.Background(), researcher.Task{Query: "why is the sky blue"})
			must.ErrorContains(err, tt.wantErr)
			must.Empty(id, "Start returned an id alongside its error")

			w.mu.Lock()
			runs := len(w.runs)
			w.mu.Unlock()
			must.Zero(runs, "no run is registered")
			must.Zero(modelSrv.CallCount(), "no model request is sent")
		})
	}
}
