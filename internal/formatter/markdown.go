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

// Markdown is the v1 Formatter. It renders one Interaction as a single
// portable document (PROPOSAL.md §4.4): YAML front matter carrying the
// run's identity and cost signals, the final text output as the body,
// chart images as relative links backed by returned assets, and a
// numbered sources section at the foot. It performs no IO: chart
// images are returned as Report.Assets and referenced from the
// document by relative links, leaving placement to the ReportSink
// (see docs/DECISIONS.md).
type Markdown struct{}

// NewMarkdown returns the markdown Formatter.
func NewMarkdown() *Markdown { return &Markdown{} }

var _ Formatter = (*Markdown)(nil)

// frontMatter is the YAML document at the head of the report. Field
// names are part of Chiron's output contract; keep them stable. The
// cost signals are the search and recall counts, token totals, and
// estimated cost;
// the tool set is the one the adapter resolved onto the create request
// (PROPOSAL §4.4), recorded on the domain Interaction since the API
// does not echo it back.
type frontMatter struct {
	Query            string       `yaml:"query,omitempty"`
	Agent            string       `yaml:"agent,omitempty"`
	Interaction      string       `yaml:"interaction,omitempty"`
	Status           types.Status `yaml:"status,omitempty"`
	StatusDetail     string       `yaml:"status_detail,omitempty"`
	Started          string       `yaml:"started,omitempty"`
	Completed        string       `yaml:"completed,omitempty"`
	Searches         int          `yaml:"searches,omitempty"`
	Recalls          int          `yaml:"recalls,omitempty"`
	Tokens           *tokens      `yaml:"tokens,omitempty"`
	EstimatedCostGBP float64      `yaml:"estimated_cost_gbp,omitempty"`
	Tools            []string     `yaml:"tools,omitempty,flow"`
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

// markdown renders the source as a numbered-list entry body. Title and
// URI are untrusted API input: link text goes through escapeInline, and
// the destination is percent-encoded separately (see webLinkDestination).
// A knowledge store locator renders as escaped title plus locator in code.
func (s source) markdown() string {
	if isKnowledgeURI(s.URI) {
		uri := strings.ReplaceAll(strings.Join(strings.Fields(s.URI), "%20"), "`", "%60")
		if s.Title == "" {
			return "`" + uri + "`"
		}
		return escapeInline(s.Title) + " `" + uri + "`"
	}
	title := s.Title
	if title == "" {
		title = s.URI
	}
	return fmt.Sprintf("[%s](%s)", escapeInline(title), webLinkDestination.Replace(s.URI))
}

// webLinkDestination percent-encodes bytes that would break CommonMark's
// inline-link destination grammar or force a fallback to literal text,
// where raw HTML in the URI would then render live: "(", ")", ASCII
// whitespace, "<", ">", and every ASCII control byte (0x00-0x1F, 0x7F).
var webLinkDestination = func() *strings.Replacer {
	pairs := []string{
		"(", "%28",
		")", "%29",
		" ", "%20",
		"<", "%3C",
		">", "%3E",
	}
	for b := 0; b <= 0x1F; b++ {
		pairs = append(pairs, string(rune(b)), fmt.Sprintf("%%%02X", b))
	}
	pairs = append(pairs, string(rune(0x7F)), fmt.Sprintf("%%%02X", 0x7F))
	return strings.NewReplacer(pairs...)
}()

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
		Recalls:          in.Usage.RecallCount,
		Tokens:           tokensOf(in.Usage),
		EstimatedCostGBP: in.Usage.EstimatedCostGBP,
		Tools:            in.Tools,
		Sources:          sources,
	}
}

// knowledgeSchemes are the non-web citation schemes a report keeps: a Billet
// memory locator and an Alexandria ref. Neither resolves in a viewer, so
// both render unlinked.
var knowledgeSchemes = []string{"billet://", "kb://"}

// isKnowledgeURI reports whether uri carries one of knowledgeSchemes.
func isKnowledgeURI(uri string) bool {
	for _, scheme := range knowledgeSchemes {
		if strings.HasPrefix(uri, scheme) {
			return true
		}
	}
	return false
}

// collectSources maps citations into front-matter shape, skipping any
// without a URI (nothing to verify against) and any with a non-web
// scheme other than the knowledge schemes: citation URIs are API-provided, and a
// javascript:, data: or file: URI is not a verifiable source — some
// renderers would pass it through to live HTML.
func collectSources(citations []types.Citation) []source {
	var out []source
	for _, c := range citations {
		if !strings.HasPrefix(c.URI, "https://") && !strings.HasPrefix(c.URI, "http://") &&
			!isKnowledgeURI(c.URI) {
			continue
		}
		out = append(out, source{URI: c.URI, Title: c.Title})
	}
	return out
}

// inlineMarkdown matches the characters that would give plain text
// Markdown meaning: emphasis, code, links, images and raw HTML.
var inlineMarkdown = strings.NewReplacer(
	`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`,
	"[", `\[`, "]", `\]`, "<", `\<`, ">", `\>`, "!", `\!`,
)

// escapeInline renders untrusted text as literal Markdown on one line.
func escapeInline(s string) string {
	return inlineMarkdown.Replace(strings.Join(strings.Fields(s), " "))
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
