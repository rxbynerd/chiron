package fleet

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"unicode/utf8"
)

// MaxReportTemplateBytes bounds a report template file and the block it
// renders at load. The block is part of the system prompt, which is re-sent
// on every turn.
const MaxReportTemplateBytes = 8 << 10

// reportTemplateSampleQuery is the query a template is rendered against at
// load. It is shorter than "{{.Query}}", so substituting it never grows the
// render and the render bound measures the template's own text.
const reportTemplateSampleQuery = "sample"

// ReportTemplate is an operator-supplied text/template that replaces the
// "Required output format" block of the worker's system prompt. It renders
// against reportTemplateData; {{.Query}} is optional because the objective
// has its own section. A ReportTemplate is safe for concurrent use.
type ReportTemplate struct {
	tmpl *template.Template
}

// reportTemplateData is the contract a report template renders against:
// {{.Query}} is the research question, as in the Gemini prompt template.
type reportTemplateData struct {
	Query string
}

// LoadReportTemplate reads, parses and validates the template at path. It
// reads at most MaxReportTemplateBytes+1 bytes and rejects a larger file,
// invalid UTF-8, a blank file, a parse error, and a template that fails to
// render, renders blank or renders past MaxReportTemplateBytes for a sample
// query, so a broken template fails before any request is made.
func LoadReportTemplate(path string) (*ReportTemplate, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("fleet: reading report template: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxReportTemplateBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fleet: reading report template %s: %w", path, err)
	}
	if len(data) > MaxReportTemplateBytes {
		return nil, fmt.Errorf("fleet: report template %s exceeds the %d-byte bound", path, MaxReportTemplateBytes)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("fleet: report template %s is not valid UTF-8", path)
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, fmt.Errorf("fleet: report template %s is blank", path)
	}

	tmpl, err := template.New(filepath.Base(path)).Option("missingkey=error").Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("fleet: parsing report template %s: %w", path, err)
	}
	rt := &ReportTemplate{tmpl: tmpl}
	sample, err := rt.render(reportTemplateSampleQuery)
	if err != nil {
		return nil, fmt.Errorf("fleet: report template %s: %w", path, err)
	}
	if len(sample) > MaxReportTemplateBytes {
		return nil, fmt.Errorf("fleet: report template %s renders %d bytes, over the %d-byte bound", path, len(sample), MaxReportTemplateBytes)
	}
	return rt, nil
}

// Render returns the output-format block for query, trimmed of surrounding
// whitespace. A render error, a blank block, or a render past
// MaxReportTemplateBytes is an error, so a real query that pushes a
// {{.Query}}-referencing template over the bound fails here too.
func (t *ReportTemplate) Render(query string) (string, error) {
	out, err := t.render(query)
	if err != nil {
		return "", fmt.Errorf("fleet: report template: %w", err)
	}
	if len(out) > MaxReportTemplateBytes {
		return "", fmt.Errorf("fleet: report template renders %d bytes for this query, over the %d-byte bound", len(out), MaxReportTemplateBytes)
	}
	return out, nil
}

func (t *ReportTemplate) render(query string) (string, error) {
	var b strings.Builder
	if err := t.tmpl.Execute(&b, reportTemplateData{Query: query}); err != nil {
		return "", err
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", errors.New("renders a blank output-format block")
	}
	return out, nil
}
