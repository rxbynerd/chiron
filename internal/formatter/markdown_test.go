package formatter

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rxbynerd/chiron/internal/types"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

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
			// C1-CODE-3 (a): a citation title containing `]` must have
			// the bracket escaped so it cannot break out of the link
			// text.
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
			// C1-CODE-3 (b): a citation URI containing `)` must have the
			// paren percent-encoded so it cannot terminate the link
			// target early.
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
			// C1-CODE-3 (c): a javascript: URI is not a web-verifiable
			// source — no Markdown link (and no front-matter source) is
			// emitted for it; only the https citation survives.
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewMarkdown().Format(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("Format: %v", err)
			}

			golden := filepath.Join("testdata", tc.name+".md")
			if *update {
				if err := os.WriteFile(golden, got.Markdown, 0o644); err != nil {
					t.Fatalf("updating golden: %v", err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("reading golden (run with -update to create): %v", err)
			}
			if !bytes.Equal(got.Markdown, want) {
				t.Errorf("markdown mismatch with %s\n--- got ---\n%s\n--- want ---\n%s", golden, got.Markdown, want)
			}

			if !reflect.DeepEqual(got.Assets, tc.wantAssets) {
				t.Errorf("assets:\n got %+v\nwant %+v", got.Assets, tc.wantAssets)
			}
		})
	}
}

func TestMarkdownFormatNilInteraction(t *testing.T) {
	if _, err := NewMarkdown().Format(context.Background(), nil); err == nil {
		t.Error("Format(nil) must error, not render an empty document")
	}
}

// TestExtensionFor pins C2-TEST-8: a wrong extension breaks the
// report's relative image link, so every MIME type the table knows —
// and the neutral fallback — is asserted.
func TestExtensionFor(t *testing.T) {
	cases := []struct {
		mime string
		want string
	}{
		{"image/png", ".png"},
		{"image/jpeg", ".jpg"},
		{"image/jpg", ".jpg"},
		{"image/gif", ".gif"},
		{"image/svg+xml", ".svg"},
		{"image/webp", ".webp"},
		{"  Image/PNG  ", ".png"}, // trimmed and case-insensitive
		{"image/tiff", ".img"},    // unknown: neutral, never empty
		{"", ".img"},
	}
	for _, tt := range cases {
		if got := extensionFor(tt.mime); got != tt.want {
			t.Errorf("extensionFor(%q) = %q, want %q", tt.mime, got, tt.want)
		}
	}
}
