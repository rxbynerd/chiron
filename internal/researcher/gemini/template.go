package gemini

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/template"
)

// The built-in output-format template. Steerability is prompt-driven
// (PROPOSAL §3): the agent shapes its report to whatever structure,
// tone and sectioning the prompt asks for, so the template is a
// first-class, swappable input (--template).
//
//go:embed template.md
var builtinFS embed.FS

// visualiseNudge is appended to the rendered prompt when --visualise is
// set: visualization "auto" permits charts, the nudge asks for them
// (PROPOSAL §4.3).
const visualiseNudge = "\nInclude charts or other visualisations wherever they make the findings easier to grasp.\n"

// queryProbe is rendered through every template at construction. It is
// chosen so that no template's own text can contain it.
const queryProbe = "chiron-query-probe-7f3a91"

// promptTemplate renders the research prompt from the query.
type promptTemplate struct {
	tmpl      *template.Template
	visualise bool
}

// templateData is the contract a template renders against: {{.Query}}
// is the research question.
type templateData struct {
	Query string
}

// loadTemplate parses the template at path, or the built-in one when
// path is empty, and renders it once with queryProbe. Both happen at
// construction so a broken template, or one that never prints the query,
// fails before any request is built whatever query is passed later.
func loadTemplate(path string, visualise bool) (*promptTemplate, error) {
	var (
		text string
		name string
	)
	if path == "" {
		data, err := builtinFS.ReadFile("template.md")
		if err != nil {
			return nil, fmt.Errorf("gemini: reading built-in template: %w", err)
		}
		text, name = string(data), "built-in"
	} else {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("gemini: reading template: %w", err)
		}
		text, name = string(data), path
	}

	tmpl, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("gemini: parsing template %s: %w", name, err)
	}
	p := &promptTemplate{tmpl: tmpl, visualise: visualise}
	if _, err := p.render(queryProbe); err != nil {
		return nil, err
	}
	return p, nil
}

// render produces the prompt for a query. A rendered prompt that does
// not contain the query verbatim is rejected: a template that drops
// {{.Query}} would silently spend £1–7 researching its own boilerplate.
func (p *promptTemplate) render(query string) (string, error) {
	var b strings.Builder
	if err := p.tmpl.Execute(&b, templateData{Query: query}); err != nil {
		return "", fmt.Errorf("gemini: rendering template: %w", err)
	}
	out := b.String()
	if !strings.Contains(out, query) {
		return "", errors.New("gemini: the rendered template does not contain the query — the template must reference {{.Query}}")
	}
	if p.visualise {
		out += visualiseNudge
	}
	return out, nil
}
