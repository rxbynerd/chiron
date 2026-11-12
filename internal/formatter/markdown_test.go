package formatter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/testutil/golden"
	"github.com/rxbynerd/chiron/internal/types"
	"github.com/rxbynerd/chiron/internal/version"
)

var (
	started  = time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	finished = time.Date(2026, 6, 7, 12, 34, 0, 0, time.UTC)
)

func TestMarkdownFormat(t *testing.T) {
	cases := []struct {
		name       string // also the golden file: testdata/<name>.md
		in         *types.Interaction
		wantAssets []types.Asset
	}{
		{
			// The canonical happy path: thought summary and interim
			// text excluded, the last text output is the body, both
			// charts become assets linked under a Charts heading, and
			// the sources land in front matter and foot — including
			// one with no title, which falls back to its URI.
			name: "complete",
			in: &types.Interaction{
				ID:     "v1_complete",
				Agent:  "deep-research-preview-04-2026",
				Query:  "Compare the safety records of European rail operators",
				Tools:  []string{"google_search", "url_context", "code_execution", "mcp_server:docs"},
				Status: types.StatusCompleted,
				Outputs: []types.Output{
					{Type: types.OutputThoughtSummary, Text: "planning the report..."},
					{Type: types.OutputText, Text: "Interim draft: superseded by the final report."},
					{Type: types.OutputImage, MIMEType: "image/png", Data: []byte("png-bytes-1")},
					{Type: types.OutputText, Text: "# Rail safety in Europe\n\nFindings follow.\n\n## Conclusion\n\nRecords improved overall.\n"},
					{Type: types.OutputImage, MIMEType: "image/svg+xml", Data: []byte("svg-bytes-2")},
				},
				Citations: []types.Citation{
					{URI: "https://example.org/era-report", Title: "ERA Annual Safety Report"},
					{URI: "https://example.com/uic-stats"},
				},
				Usage: types.Usage{
					InputTokens:      250000,
					CachedTokens:     150000,
					OutputTokens:     60000,
					ToolUseTokens:    1000,
					ThoughtTokens:    9000,
					SearchCount:      80,
					PollCount:        14,
					ReconnectCount:   1,
					EstimatedCostGBP: 1.84,
				},
				CreatedAt:   started,
				CompletedAt: finished,
			},
			wantAssets: []types.Asset{
				{Name: "chart-1.png", MIMEType: "image/png", Data: []byte("png-bytes-1")},
				{Name: "chart-2.svg", MIMEType: "image/svg+xml", Data: []byte("svg-bytes-2")},
			},
		},
		{
			// An in-process run with web-provider spend: web_cost_usd and
			// web_cost_unknown_calls follow estimated_cost_gbp, in their
			// own currency, and the tool list names the provider.
			name: "web-cost",
			in: &types.Interaction{
				ID:      "wkr_webcost",
				Agent:   "worker",
				Query:   "Why is the sky blue",
				Tools:   []string{"web_search:exa", "web_fetch:exa"},
				Status:  types.StatusCompleted,
				Outputs: []types.Output{{Type: types.OutputText, Text: "# Sky\n\nRayleigh scattering."}},
				Citations: []types.Citation{
					{URI: "https://example.org/sky", Title: "Sky"},
				},
				Usage: types.Usage{
					InputTokens:         1200,
					OutputTokens:        300,
					SearchCount:         2,
					EstimatedCostGBP:    0.01,
					WebCostUSD:          0.015,
					WebCostUnknownCalls: 1,
				},
				CreatedAt:   started,
				CompletedAt: finished,
			},
		},
		{
			// No citations and no images: no sources front matter, no
			// Sources or Charts sections, no tokens block.
			name: "no-citations",
			in: &types.Interaction{
				ID:          "v1_nocite",
				Agent:       "deep-research-preview-04-2026",
				Query:       "Summarise the history of the Brennan torpedo",
				Status:      types.StatusCompleted,
				Outputs:     []types.Output{{Type: types.OutputText, Text: "# The Brennan torpedo\n\nA wire-guided weapon."}},
				CreatedAt:   started,
				CompletedAt: finished,
			},
		},
		{
			// Failed before producing any output: placeholder body
			// carries the status and the adapter's detail; front
			// matter still records identity, timestamps, and the
			// cost accrued before the failure.
			name: "failed",
			in: &types.Interaction{
				ID:           "v1_failed",
				Agent:        "deep-research-preview-04-2026",
				Query:        "Audit the provenance of the Codex Sinaiticus",
				Status:       types.StatusFailed,
				StatusDetail: "research quota exhausted",
				Usage:        types.Usage{InputTokens: 1200, SearchCount: 3},
				CreatedAt:    started,
				CompletedAt:  finished,
			},
		},
		{
			// Cancelled mid-run with partial output: the last text
			// output still renders as the body, citations gathered so
			// far are kept for verification.
			name: "cancelled-partial",
			in: &types.Interaction{
				ID:     "v1_cancelled",
				Agent:  "deep-research-preview-04-2026",
				Query:  "Trace the etymology of the word chiron",
				Status: types.StatusCancelled,
				Outputs: []types.Output{
					{Type: types.OutputText, Text: "Partial findings before cancellation."},
				},
				Citations:   []types.Citation{{URI: "https://example.net/etymology", Title: "Etymology Online"}},
				CreatedAt:   started,
				CompletedAt: finished,
			},
		},
		{
			// Retrieved mid-run with no outputs at all: front matter
			// only, no completed stamp, placeholder body.
			name: "in-progress-empty",
			in: &types.Interaction{
				ID:        "v1_running",
				Agent:     "deep-research-preview-04-2026",
				Query:     "Chart quarterly grain shipments through the Bosphorus",
				Status:    types.StatusInProgress,
				CreatedAt: started,
			},
		},
		{
			// Images without any text output: charts still surface
			// (with a neutral extension for an unrecognised MIME
			// type) alongside the placeholder body. A citation with
			// an empty URI is dropped — nothing to verify against.
			name: "charts-no-text",
			in: &types.Interaction{
				ID:     "v1_charts",
				Agent:  "deep-research-max-preview-04-2026",
				Query:  "Visualise Mediterranean shipping density",
				Status: types.StatusCompleted,
				Outputs: []types.Output{
					{Type: types.OutputImage, MIMEType: "image/tiff", Data: []byte("tiff-bytes")},
					{Type: types.OutputImage, MIMEType: "image/png", Data: []byte("png-bytes")},
				},
				Citations:   []types.Citation{{URI: "", Title: "orphaned title"}},
				CreatedAt:   started,
				CompletedAt: finished,
			},
			wantAssets: []types.Asset{
				{Name: "chart-1.img", MIMEType: "image/tiff", Data: []byte("tiff-bytes")},
				{Name: "chart-2.png", MIMEType: "image/png", Data: []byte("png-bytes")},
			},
		},
		{
			// A citation title containing `]` has the bracket escaped so
			// it cannot break out of the link text.
			name: "citation-bracket-title",
			in: &types.Interaction{
				ID:      "v1_brkt",
				Agent:   "deep-research-preview-04-2026",
				Query:   "Catalogue bracket usage in legal citations",
				Status:  types.StatusCompleted,
				Outputs: []types.Output{{Type: types.OutputText, Text: "# Brackets\n\nFindings."}},
				Citations: []types.Citation{
					{URI: "https://example.org/brackets", Title: "Title with ] bracket](https://evil.example)"},
				},
				CreatedAt:   started,
				CompletedAt: finished,
			},
		},
		{
			// A citation URI containing `)` has the paren percent-encoded
			// so it cannot terminate the link target early.
			name: "citation-paren-uri",
			in: &types.Interaction{
				ID:      "v1_paren",
				Agent:   "deep-research-preview-04-2026",
				Query:   "Survey URLs containing parentheses",
				Status:  types.StatusCompleted,
				Outputs: []types.Output{{Type: types.OutputText, Text: "# Parentheses\n\nFindings."}},
				Citations: []types.Citation{
					{URI: "https://example.org/wiki/Foo_(bar)", Title: "Foo (bar)"},
				},
				CreatedAt:   started,
				CompletedAt: finished,
			},
		},
		{
			// A javascript: URI is not a web-verifiable source, so no
			// Markdown link (and no front-matter source) is emitted for
			// it; only the https citation survives.
			name: "citation-javascript-uri",
			in: &types.Interaction{
				ID:      "v1_js",
				Agent:   "deep-research-preview-04-2026",
				Query:   "Audit citation hygiene",
				Status:  types.StatusCompleted,
				Outputs: []types.Output{{Type: types.OutputText, Text: "# Hygiene\n\nFindings."}},
				Citations: []types.Citation{
					{URI: "javascript:alert(1)", Title: "Hostile"},
					{URI: "https://example.org/safe", Title: "Safe source"},
				},
				CreatedAt:   started,
				CompletedAt: finished,
			},
		},
		{
			// A billet:// memory locator or a kb:// ref is kept as a
			// source but rendered as its escaped title plus the locator
			// in inline code, never a link; other non-web schemes stay
			// dropped, and the recall count lands in front matter.
			name: "citation-billet-uri",
			in: &types.Interaction{
				ID:      "wkr_billet",
				Agent:   "worker",
				Query:   "What did we decide about PHY vendors?",
				Tools:   []string{"web_search", "web_fetch", "knowledge_recall"},
				Status:  types.StatusCompleted,
				Outputs: []types.Output{{Type: types.OutputText, Text: "# PHY vendors\n\nFindings."}},
				Citations: []types.Citation{
					{URI: "billet://memory/0b1c2d", Title: "PHY vendor [decision](javascript:x) *2026*"},
					{URI: "billet://memory/untitled`id"},
					{URI: "kb://source/1234#L10-L14", Title: "Vendor notes"},
					{URI: "javascript:alert(1)", Title: "Dropped"},
					{URI: "https://example.org/phy", Title: "PHY overview"},
				},
				Usage:       types.Usage{SearchCount: 1, RecallCount: 2},
				CreatedAt:   started,
				CompletedAt: finished,
			},
		},
		{
			// A web citation title carrying raw HTML, plus emphasis and
			// code markers, must render as inert text: escapeInline
			// covers the same characters here as it does for
			// knowledge-source titles.
			name: "citation-html-title",
			in: &types.Interaction{
				ID:      "v1_html",
				Agent:   "deep-research-preview-04-2026",
				Query:   "Audit citation titles for raw HTML",
				Status:  types.StatusCompleted,
				Outputs: []types.Output{{Type: types.OutputText, Text: "# HTML titles\n\nFindings."}},
				Citations: []types.Citation{
					{URI: "https://example.org/report", Title: "Breaking <img src=https://attacker.example/x> *news* _now_ `verified`"},
				},
				CreatedAt:   started,
				CompletedAt: finished,
			},
		},
		{
			// A web citation URI carrying a space and raw HTML must stay
			// a valid CommonMark link destination: the space and angle
			// brackets are percent-encoded so the link cannot fall back
			// to literal text.
			name: "citation-uri-space-html",
			in: &types.Interaction{
				ID:      "v1_urispace",
				Agent:   "deep-research-preview-04-2026",
				Query:   "Audit citation URIs for raw HTML",
				Status:  types.StatusCompleted,
				Outputs: []types.Output{{Type: types.OutputText, Text: "# URI hygiene\n\nFindings."}},
				Citations: []types.Citation{
					{URI: "https://example.com/a <img src=x>", Title: "Suspicious URI"},
				},
				CreatedAt:   started,
				CompletedAt: finished,
			},
		},
		{
			// An empty title falls back to the URI as link text; that
			// fallback text is escaped exactly like an explicit title.
			name: "citation-empty-title-escaped-uri",
			in: &types.Interaction{
				ID:      "v1_emptytitle",
				Agent:   "deep-research-preview-04-2026",
				Query:   "Audit fallback link text",
				Status:  types.StatusCompleted,
				Outputs: []types.Output{{Type: types.OutputText, Text: "# Fallback text\n\nFindings."}},
				Citations: []types.Citation{
					{URI: "https://example.org/foo_bar*baz"},
				},
				CreatedAt:   started,
				CompletedAt: finished,
			},
		},
		{
			// An empty title's URI fallback feeds the same string
			// through escapeInline for the link text and
			// webLinkDestination for the destination: both transforms
			// must fire correctly on one shared, untrusted string.
			name: "citation-empty-title-html-uri",
			in: &types.Interaction{
				ID:      "v1_emptyhtml",
				Agent:   "deep-research-preview-04-2026",
				Query:   "Audit fallback link text against raw HTML",
				Status:  types.StatusCompleted,
				Outputs: []types.Output{{Type: types.OutputText, Text: "# Fallback text\n\nFindings."}},
				Citations: []types.Citation{
					{URI: "https://example.org/<script> report"},
				},
				CreatedAt:   started,
				CompletedAt: finished,
			},
		},
		{
			// A literal backslash immediately before a raw tag escapes
			// itself first, then the tag: the title still round-trips
			// to fully inert text rather than unmasking the tag.
			name: "citation-backslash-html-title",
			in: &types.Interaction{
				ID:      "v1_backslash",
				Agent:   "deep-research-preview-04-2026",
				Query:   "Audit backslash-adjacent HTML in citation titles",
				Status:  types.StatusCompleted,
				Outputs: []types.Output{{Type: types.OutputText, Text: "# Backslash titles\n\nFindings."}},
				Citations: []types.Citation{
					{URI: "https://example.org/backslash", Title: `\<img src=x>`},
				},
				CreatedAt:   started,
				CompletedAt: finished,
			},
		},
		{
			// A title with an embedded newline and repeated spaces
			// still collapses to single spaces, matching escapeInline's
			// existing whitespace handling at this call site.
			name: "citation-title-whitespace",
			in: &types.Interaction{
				ID:      "v1_whitespace",
				Agent:   "deep-research-preview-04-2026",
				Query:   "Audit whitespace collapse in citation titles",
				Status:  types.StatusCompleted,
				Outputs: []types.Output{{Type: types.OutputText, Text: "# Whitespace titles\n\nFindings."}},
				Citations: []types.Citation{
					{URI: "https://example.org/whitespace", Title: "Multiple   spaces\nand a newline"},
				},
				CreatedAt:   started,
				CompletedAt: finished,
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			got, err := NewMarkdown().Format(context.Background(), tt.in)
			must.NoError(err)

			// The build's own version varies with how the test binary was
			// built; the fixtures always read "dev".
			rendered := strings.Replace(string(got.Markdown),
				"chiron-version: "+version.String()+" -->", "chiron-version: dev -->", 1)
			golden.Assert(t, tt.name+".md", []byte(rendered))

			must.Equal(tt.wantAssets, got.Assets)
		})
	}
}

// TestMarkdown_Format_NoLiveHTMLInBody proves that a citation title
// carrying an <img> tag cannot reach the rendered body as live HTML.
// The YAML front matter carries the raw title as data for
// front-matter-aware viewers; this test does not scan it.
func TestMarkdown_Format_NoLiveHTMLInBody(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	in := &types.Interaction{
		ID:      "v1_livehtml",
		Agent:   "deep-research-preview-04-2026",
		Query:   "Audit citation titles for raw HTML",
		Status:  types.StatusCompleted,
		Outputs: []types.Output{{Type: types.OutputText, Text: "# HTML titles\n\nFindings."}},
		Citations: []types.Citation{
			{URI: "https://example.org/report", Title: "Breaking <img src=https://attacker.example/x> news"},
		},
		CreatedAt:   started,
		CompletedAt: finished,
	}

	got, err := NewMarkdown().Format(context.Background(), in)
	must.NoError(err)

	const frontMatterEnd = "---\n\n"
	must.Contains(string(got.Markdown), frontMatterEnd, "front-matter close delimiter")
	i := strings.Index(string(got.Markdown), frontMatterEnd)
	body := string(got.Markdown)[i+len(frontMatterEnd):]

	check.Contains(body, `\<img src=https://attacker.example/x\>`)
	for pos, r := range body {
		if r != '<' {
			continue
		}
		backslashes := 0
		for j := pos - 1; j >= 0 && body[j] == '\\'; j-- {
			backslashes++
		}
		escaped := backslashes%2 == 1
		check.True(escaped, "unescaped %q at byte %d in body (preceded by %d backslashes):\n%s", "<", pos, backslashes, body)
	}
}

func TestMarkdown_Format_NilInteractionErrors(t *testing.T) {
	must := require.New(t)

	_, err := NewMarkdown().Format(context.Background(), nil)
	must.Error(err, "Format(nil) must error, not render an empty document")
}

// TestWebLinkDestination_PercentEncoding asserts every ASCII control byte
// and DEL is percent-encoded as uppercase %XX, and that an already
// percent-encoded sequence is left untouched rather than having its "%"
// re-encoded.
func TestWebLinkDestination_PercentEncoding(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"nul", "\x00", "%00"},
		{"bel", "\x07", "%07"},
		{"tab", "\t", "%09"},
		{"lf", "\n", "%0A"},
		{"cr", "\r", "%0D"},
		{"esc", "\x1B", "%1B"},
		{"del", "\x7F", "%7F"},
		{"no-double-encode", "https://example.org/a%20b?x=1%29", "https://example.org/a%20b?x=1%29"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.want, webLinkDestination.Replace(tt.in))
		})
	}
}

// TestMarkdown_Format_VersionComment pins the provenance comment to the
// chiron build version, independent of the golden fixtures (which are
// normalised to the "dev" version): a refresh script needs it as
// the document's first line, matching what --version reports.
func TestMarkdown_Format_VersionComment(t *testing.T) {
	must := require.New(t)

	old := version.Version
	version.Version = "v9.9.9-test"
	defer func() { version.Version = old }()

	in := &types.Interaction{
		ID:      "v1_version",
		Status:  types.StatusCompleted,
		Outputs: []types.Output{{Type: types.OutputText, Text: "Body."}},
	}
	got, err := NewMarkdown().Format(context.Background(), in)
	must.NoError(err)

	want := "<!-- chiron-version: v9.9.9-test -->\n"
	must.True(strings.HasPrefix(string(got.Markdown), want),
		"markdown does not start with provenance comment %q:\n%s", want, got.Markdown)
}

// TestExtensionFor pins the file extension used for each known image MIME
// type. A wrong extension breaks the report's relative image link, so every
// MIME type the table knows, and the neutral fallback, is asserted.
func TestExtensionFor(t *testing.T) {
	cases := []struct {
		name string
		mime string
		want string
	}{
		{"png", "image/png", ".png"},
		{"jpeg", "image/jpeg", ".jpg"},
		{"jpg", "image/jpg", ".jpg"},
		{"gif", "image/gif", ".gif"},
		{"svg", "image/svg+xml", ".svg"},
		{"webp", "image/webp", ".webp"},
		{"trimmed-and-case-insensitive", "  Image/PNG  ", ".png"},
		{"unknown-falls-back-to-img", "image/tiff", ".img"},
		{"empty-falls-back-to-img", "", ".img"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.want, extensionFor(tt.mime))
		})
	}
}
