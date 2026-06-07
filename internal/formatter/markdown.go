// The markdown Formatter renders one Interaction as a single portable
// document (PROPOSAL.md §4.4): YAML front matter carrying the run's
// identity and cost signals, the final text output as the body, chart
// images as relative links backed by returned assets, and a numbered
// sources section at the foot.
package formatter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rxbynerd/chiron/internal/types"
)

// Markdown is the v1 Formatter. It performs no IO: chart images are
// returned as Report.Assets and referenced from the document by
// relative links, leaving placement to the ReportSink (see
// docs/DECISIONS.md).
type Markdown struct{}

// NewMarkdown returns the markdown Formatter.
func NewMarkdown() *Markdown { return &Markdown{} }

var _ Formatter = (*Markdown)(nil)

// frontMatter is the YAML document at the head of the report. Field
// names are part of Chiron's output contract; keep them stable. The
// domain model deliberately carries no tool set (it is a wire-level
// detail), so the cost signals here are the search count, token
// totals, and estimated cost.
type frontMatter struct {
	Query            string       `yaml:"query,omitempty"`
	Agent            string       `yaml:"agent,omitempty"`
	Interaction      string       `yaml:"interaction,omitempty"`
	Status           types.Status `yaml:"status,omitempty"`
	StatusDetail     string       `yaml:"status_detail,omitempty"`
	Started          string       `yaml:"started,omitempty"`
	Completed        string       `yaml:"completed,omitempty"`
	Searches         int          `yaml:"searches,omitempty"`
	Tokens           *tokens      `yaml:"tokens,omitempty"`
	EstimatedCostGBP float64      `yaml:"estimated_cost_gbp,omitempty"`
	Sources          []source     `yaml:"sources,omitempty"`
}

// tokens mirrors the usage block's token counts — per-run cost signals
// alongside the search count. Poll and reconnect counts are run
// telemetry, reported via RunResult and traces, not the report.
type tokens struct {
	Input   int `yaml:"input,omitempty"`
	Cached  int `yaml:"cached,omitempty"`
	Output  int `yaml:"output,omitempty"`
	ToolUse int `yaml:"tool_use,omitempty"`
	Thought int `yaml:"thought,omitempty"`
}

// source is one cited source. Citations arrive already deduplicated
// from the adapter; the slice order here is the numbering used by the
// sources section.
type source struct {
	URI   string `yaml:"uri"`
	Title string `yaml:"title,omitempty"`
}

// markdown renders the source as a numbered-list entry body.
func (s source) markdown() string {
	title := s.Title
	if title == "" {
		title = s.URI
	}
	return fmt.Sprintf("[%s](%s)", title, s.URI)
}

// Format renders the interaction. The body is the last text output —
// the final report; earlier text outputs are interim and thought
// summaries are excluded. Every image output becomes a chart asset,
// linked under a Charts heading. An interaction with no report text
// (failed, cancelled, still in progress) still yields a document: the
// front matter carries the status and a placeholder body says so.
func (m *Markdown) Format(_ context.Context, in *types.Interaction) (*types.Report, error) {
	if in == nil {
		return nil, errors.New("formatter: nil interaction")
	}

	body := finalText(in.Outputs)
	if body == "" {
		body = fmt.Sprintf("_No report content was produced (interaction status: %s)._", statusLabel(in))
	}

	var (
		assets []types.Asset
		charts []string
	)
	for _, out := range in.Outputs {
		if out.Type != types.OutputImage {
			continue
		}
		name := fmt.Sprintf("chart-%d%s", len(assets)+1, extensionFor(out.MIMEType))
		assets = append(assets, types.Asset{Name: name, MIMEType: out.MIMEType, Data: out.Data})
		charts = append(charts, fmt.Sprintf("![Chart %d](%s)", len(assets), name))
	}

	sources := collectSources(in.Citations)

	var b strings.Builder
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(buildFrontMatter(in, sources)); err != nil {
		return nil, fmt.Errorf("formatter: encoding front matter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("formatter: encoding front matter: %w", err)
	}
	b.WriteString("---\n\n")
	b.WriteString(body)
	b.WriteString("\n")
	if len(charts) > 0 {
		b.WriteString("\n## Charts\n\n")
		b.WriteString(strings.Join(charts, "\n\n"))
		b.WriteString("\n")
	}
	if len(sources) > 0 {
		b.WriteString("\n## Sources\n\n")
		for i, s := range sources {
			fmt.Fprintf(&b, "%d. %s\n", i+1, s.markdown())
		}
	}

	return &types.Report{Markdown: []byte(b.String()), Assets: assets}, nil
}

// finalText is the last text output — the final report — or empty when
// the agent produced none.
func finalText(outputs []types.Output) string {
	for i := len(outputs) - 1; i >= 0; i-- {
		if outputs[i].Type == types.OutputText {
			return strings.TrimSpace(outputs[i].Text)
		}
	}
	return ""
}

// statusLabel renders the status for the placeholder body, with the
// adapter's human-readable detail when there is one.
func statusLabel(in *types.Interaction) string {
	status := string(in.Status)
	if status == "" {
		status = "unknown"
	}
	if in.StatusDetail != "" {
		status += ": " + in.StatusDetail
	}
	return status
}

// buildFrontMatter assembles the report's YAML header from the
// interaction's identity, timestamps, and cost signals.
func buildFrontMatter(in *types.Interaction, sources []source) frontMatter {
	return frontMatter{
		Query:            in.Query,
		Agent:            in.Agent,
		Interaction:      in.ID,
		Status:           in.Status,
		StatusDetail:     in.StatusDetail,
		Started:          stamp(in.CreatedAt),
		Completed:        stamp(in.CompletedAt),
		Searches:         in.Usage.SearchCount,
		Tokens:           tokensOf(in.Usage),
		EstimatedCostGBP: in.Usage.EstimatedCostGBP,
		Sources:          sources,
	}
}

// collectSources maps citations into front-matter shape, skipping any
// without a URI (nothing to verify against).
func collectSources(citations []types.Citation) []source {
	var out []source
	for _, c := range citations {
		if c.URI == "" {
			continue
		}
		out = append(out, source{URI: c.URI, Title: c.Title})
	}
	return out
}

// tokensOf maps the usage token counts into front-matter shape, or nil
// when none were reported so the block is omitted entirely.
func tokensOf(u types.Usage) *tokens {
	t := tokens{
		Input:   u.InputTokens,
		Cached:  u.CachedTokens,
		Output:  u.OutputTokens,
		ToolUse: u.ToolUseTokens,
		Thought: u.ThoughtTokens,
	}
	if t == (tokens{}) {
		return nil
	}
	return &t
}

// stamp renders a timestamp as RFC 3339 UTC, or empty when unset.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// extensionFor picks a file extension for a chart asset. A hand-rolled
// table rather than mime.ExtensionsByType, whose results vary by
// platform and would make asset names non-deterministic. Unrecognised
// types get a neutral extension rather than a guessed one.
func extensionFor(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/svg+xml":
		return ".svg"
	case "image/webp":
		return ".webp"
	default:
		return ".img"
	}
}
