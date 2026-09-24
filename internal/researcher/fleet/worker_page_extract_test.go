package fleet

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// extract reduces src as pageText does for HTML, without the byte bound.
func extract(src string) string {
	return collapseWhitespace(htmlToText(src))
}

// para is long enough to earn paragraph credit.
const para = "This paragraph carries the real content of the page, long enough to score."

func TestAttrWords(t *testing.T) {
	for _, tt := range []struct {
		value       string
		boilerplate bool
		content     bool
	}{
		{"navbar", true, false},
		{"navigation", true, false},
		{"site-footer", true, false},
		{"cookie-banner", true, false},
		{"comments", true, false},
		{"share_buttons", true, false},
		{"x-promo2", true, false},
		{"Sidebar", true, false},
		{"siteFooter", true, false},
		{"topNavBar", true, false},
		{"SideBar", true, false},
		{"BreadCrumbs", true, false},
		{"breadcrumbs", true, false},
		{"related-articles", true, false},
		{"unavailable", false, false},
		{"canvas", false, false},
		{"renavigate", false, false},
		{"bg-navy", false, false},
		{"bgNavy", false, false},
		{"commentary", false, false},
		{"commentaryBox", false, false},
		{"ad-slot", false, false},
		{"header", false, false},
		{"", false, false},
		{"article-body", false, true},
		{"mainContent", false, true},
		{"maincontent", false, false},
		{"main-nav", true, true},
		{"post comments", true, true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			b, c := attrWords(tt.value)
			if b != tt.boilerplate || c != tt.content {
				t.Errorf("attrWords(%q) = (%v, %v), want (%v, %v)", tt.value, b, c, tt.boilerplate, tt.content)
			}
		})
	}
}

func TestTagAttrs(t *testing.T) {
	for _, tt := range []struct {
		inner, id, class, role string
	}{
		{`div id="a" class='b c' role=main`, "a", "b c", "main"},
		{`DIV ID=x CLASS = "y"`, "x", "y", ""},
		{`div class="first" class="second"`, "", "first", ""},
		{`div data-class="no" class=yes/`, "", "yes/", ""},
		{`div class="unterminated`, "", "unterminated", ""},
		{`div =stray id=z`, "z", "", ""},
		{`br/`, "", "", ""},
	} {
		t.Run(tt.inner, func(t *testing.T) {
			id, class, role := tagAttrs(tt.inner)
			if id != tt.id || class != tt.class || role != tt.role {
				t.Errorf("tagAttrs(%q) = (%q, %q, %q), want (%q, %q, %q)", tt.inner, id, class, role, tt.id, tt.class, tt.role)
			}
		})
	}
}

// TestExtractBoilerplateRoles: every boilerplate role drops its element,
// matched case-insensitively on the first role token.
func TestExtractBoilerplateRoles(t *testing.T) {
	roles := append([]string{"Navigation", "navigation menu"}, boilerplateRoles...)
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			got := extract(`<main><div role="` + role + `">Furniture text</div><p>` + para + `</p></main>`)
			if strings.Contains(got, "Furniture") || !strings.Contains(got, para) {
				t.Errorf("role %q: got %q", role, got)
			}
		})
	}
	got := extract(`<main><div role="presentation">Kept text</div><p>` + para + `</p></main>`)
	if !strings.Contains(got, "Kept text") {
		t.Errorf("a non-boilerplate role dropped its element: %q", got)
	}
}

func TestExtractSelection(t *testing.T) {
	const other = "Some other paragraph that is outside the chosen content, also long enough."
	for _, tt := range []struct {
		name   string
		src    string
		want   []string
		absent []string
	}{
		{
			name:   "role main chosen",
			src:    `<div><p>` + other + `</p></div><div role="main"><p>` + para + `</p></div>`,
			want:   []string{para},
			absent: []string{other},
		},
		{
			name:   "main preferred over article",
			src:    `<article><p>` + other + `</p></article><main><p>` + para + `</p></main>`,
			want:   []string{para},
			absent: []string{other},
		},
		{
			name:   "largest article",
			src:    `<article><p>Short teaser.</p></article><article><p>` + para + `</p><p>` + para + `</p></article>`,
			want:   []string{para},
			absent: []string{"Short teaser"},
		},
		{
			name:   "article inside aside is not content",
			src:    `<aside><article><p>` + other + other + other + `</p></article></aside><article><p>` + para + `</p></article>`,
			want:   []string{para},
			absent: []string{other},
		},
		{
			name:   "header kept inside article",
			src:    `<header>Site name</header><article><header><h1>The headline</h1></header><p>` + para + `</p></article>`,
			want:   []string{"The headline", para},
			absent: []string{"Site name"},
		},
		{
			name:   "header kept inside main",
			src:    `<header>Site name</header><main><header><h1>The headline</h1></header><p>` + para + `</p></main>`,
			want:   []string{"The headline", para},
			absent: []string{"Site name"},
		},
		{
			name:   "page header dropped from the body",
			src:    `<body><header>Site name</header><p>Short body text.</p></body>`,
			want:   []string{"Short body text."},
			absent: []string{"Site name"},
		},
		{
			name:   "furniture tags dropped inside the root",
			src:    `<main><p>` + para + `</p><footer>Foot</footer><nav>Links</nav><aside>Aside</aside><form>Search</form></main>`,
			want:   []string{para},
			absent: []string{"Foot", "Links", "Aside", "Search"},
		},
		{
			name: "protected elements survive furniture attributes",
			src:  `<body class="has-sidebar"><main class="nav-open"><article class="comment-thread"><p>` + para + `</p></article></main></body>`,
			want: []string{para},
		},
		{
			name: "content word vetoes a boilerplate attribute",
			src:  `<main><div class="comments article-body"><p>` + para + `</p></div></main>`,
			want: []string{para},
		},
		{
			name: "code is exempt from attribute heuristics",
			src:  `<main><p>` + para + `</p><pre><code><span class="hljs-comment"># keep me</span></code></pre></main>`,
			want: []string{"# keep me"},
		},
		{
			name:   "attribute names and values match case-insensitively",
			src:    `<MAIN><DIV CLASS=NavBar>Menu</DIV><Div Id="BreadCrumbs">Home / Docs</Div><P>` + para + `</P></MAIN>`,
			want:   []string{para},
			absent: []string{"Menu", "Home / Docs"},
		},
		{
			name:   "implausibly small landmark falls through to scoring",
			src:    `<main><p>Tiny.</p></main><div><p>` + para + `</p><p>` + para + `</p></div>`,
			want:   []string{para},
			absent: []string{"Tiny."},
		},
		{
			name: "scoring prefers text over links",
			src: `<div><p><a href="/1">A link with a long enough label to count</a></p>` +
				`<p><a href="/2">Another link with a long enough label too</a></p></div>` +
				`<div><p>` + para + `</p></div>`,
			want:   []string{para},
			absent: []string{"A link", "Another link"},
		},
		{
			name:   "content inside a form is scored",
			src:    `<body><form><div class="nav">Menu</div><div><p>` + para + `</p><p>` + other + `</p></div></form></body>`,
			want:   []string{para, other},
			absent: []string{"Menu"},
		},
		{
			name:   "form inside an article is dropped",
			src:    `<article><p>` + para + `</p><form><label>Leave a reply</label></form></article>`,
			want:   []string{para},
			absent: []string{"Leave a reply"},
		},
		{
			name: "heading and long paragraph siblings join the top block",
			src: `<div><h1>The headline</h1><div>` +
				`<p>First, second, and third.` + para + `</p><p>First, second, and third.` + para + `</p>` +
				`<p>First, second, and third.` + para + `</p></div>` +
				`<p>A closing paragraph without any links that is comfortably long enough to be joined to the chosen block of content.</p>` +
				`<div><a href="/x">Unrelated link</a></div></div>`,
			want:   []string{"The headline", para, "A closing paragraph"},
			absent: []string{"Unrelated link"},
		},
		{
			name:   "unclosed head ends at body content",
			src:    `<html><head><title>Page title</title><p>Body text is here.</p>`,
			want:   []string{"Body text is here."},
			absent: []string{"Page title"},
		},
		{
			name: "loose text ends the head",
			src:  `<head><meta charset="utf-8">Loose text</head>`,
			want: []string{"Loose text"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := extract(tt.src)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in %q", w, got)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(got, a) {
					t.Errorf("unexpected %q in %q", a, got)
				}
			}
		})
	}
}

// TestExtractSafetyNet: when the chosen content renders no letters or
// digits, the whole document's visible text is returned instead.
func TestExtractSafetyNet(t *testing.T) {
	src := `<body><form><b>Loose text</b> in a form<nav>Menu</nav><script>hidden()</script></form></body>`
	doc := parsePage(src)
	if content := doc.render(doc.contentRoots(), true); hasLetterOrDigit(content) {
		t.Fatalf("content rendering = %q, want no text", content)
	}
	got := extract(src)
	if got != "Loose text in a form\nMenu" {
		t.Errorf("got %q, want the whole document's visible text", got)
	}
}

func TestParsePageStructure(t *testing.T) {
	// parents returns name->parent-name pairs in document order.
	parents := func(d *pageDoc) string {
		var out []string
		for i := 1; i < len(d.elems); i++ {
			p := d.elems[d.elems[i].parent].name
			if p == "" {
				p = "#root"
			}
			out = append(out, d.elems[i].name+"<"+p)
		}
		return strings.Join(out, " ")
	}
	for _, tt := range []struct {
		name, src, want string
	}{
		{"unmatched close tags ignored", `<div>a</span></p>b</div><p>c</p>`, "div<#root p<#root"},
		{"close pops to the nearest match", `<div><span><b>x</div><p>y</p>`, "div<#root span<div b<span p<#root"},
		{"unclosed paragraphs close each other", `<div><p>one<p>two<ul><li>a<li>b</ul></div>`, "div<#root p<div p<div ul<div li<ul li<ul"},
		{"unclosed cells and rows", `<table><tr><td>a<td>b<tr><td>c</table>`, "table<#root tr<table td<tr td<tr tr<table td<tr"},
		{"definition terms", `<dl><dt>a<dd>b<dt>c</dl>`, "dl<#root dt<dl dd<dl dt<dl"},
		{"body closes an open head", `<head><title>x</title><body><p>y</p>`, "head<#root title<head body<#root p<body"},
		{"void elements never open a scope", `<div><img src=x><br><input>text</div>`, "div<#root"},
		{"raw-text elements are not elements", `<div><script>x</script><style>y</style></div>`, "div<#root"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := parents(parsePage(tt.src)); got != tt.want {
				t.Errorf("structure = %q, want %q", got, tt.want)
			}
		})
	}

	d := parsePage(`<div><section><p>text`)
	for i, e := range d.elems {
		if e.end != int32(len(d.tokens)) || e.elemEnd != int32(len(d.elems)) {
			t.Errorf("element %d (%s) is not closed at end of input: end %d, elemEnd %d", i, e.name, e.end, e.elemEnd)
		}
	}
	if got := extract(`<div><section><p>text`); got != "text" {
		t.Errorf("unclosed elements: got %q", got)
	}
}

func TestHTMLToTextEntities(t *testing.T) {
	got := extract(`<p>R&amp;D &pound;5 &#8212; &#x2014; a&nbsp;b &lt;p&gt; &copy &unknown;</p>`)
	want := "R&D £5 — — a b <p> © &unknown;"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// adversarialPages are hostile or malformed inputs extraction must handle in
// linear time and bounded memory.
func adversarialPages() []struct {
	name  string
	src   string
	check func(t *testing.T, p *pageParser, text string)
} {
	deepDivs := 20000
	distinctTags := 5000
	manyElements := maxPageElements + 5000
	var distinct strings.Builder
	for i := range distinctTags {
		distinct.WriteString("<x" + strconv.Itoa(i) + ">")
	}
	return []struct {
		name  string
		src   string
		check func(t *testing.T, p *pageParser, text string)
	}{
		{
			name: "many scripts",
			src:  strings.Repeat("<script>var s = '</div>';</script>", 50000) + "<p>after scripts</p>",
			check: func(t *testing.T, _ *pageParser, text string) {
				if !strings.HasSuffix(text, "after scripts") || strings.Contains(text, "var s") {
					t.Errorf("text ends %q", text[max(0, len(text)-40):])
				}
			},
		},
		{
			name: "nesting beyond the depth cap",
			src:  strings.Repeat("<div>", deepDivs) + "deep text" + strings.Repeat("</div>", deepDivs) + "<p>after</p>",
			check: func(t *testing.T, p *pageParser, text string) {
				if len(p.elems) > maxPageDepth+2 {
					t.Errorf("%d elements, want at most the depth cap", len(p.elems))
				}
				if !strings.Contains(text, "deep text") || !strings.HasSuffix(text, "after") {
					t.Errorf("text lost: %q", text[max(0, len(text)-60):])
				}
				if last := p.elems[len(p.elems)-1]; last.name != "p" || last.parent != 0 {
					t.Errorf("close tags beyond the cap popped pushed elements: last is %s under %d", last.name, last.parent)
				}
			},
		},
		{
			name: "distinct unclosed tags beyond the depth cap",
			src:  distinct.String() + "tail text",
			check: func(t *testing.T, p *pageParser, text string) {
				if len(p.overflow) > maxPageDepth {
					t.Errorf("overflow tracks %d names, want at most %d", len(p.overflow), maxPageDepth)
				}
				if !strings.Contains(text, "tail text") {
					t.Errorf("text lost: %q", text)
				}
			},
		},
		{
			name: "element count beyond the cap",
			src:  strings.Repeat("<span>x</span>", manyElements),
			check: func(t *testing.T, p *pageParser, text string) {
				if len(p.elems) != maxPageElements {
					t.Errorf("%d elements, want the cap %d", len(p.elems), maxPageElements)
				}
				if n := strings.Count(text, "x"); n != manyElements {
					t.Errorf("text has %d of %d spans' text", n, manyElements)
				}
			},
		},
		{
			name: "unmatched close tags under a deep stack",
			src:  strings.Repeat("<div>", 600) + strings.Repeat("</span>", 200000) + "kept" + strings.Repeat("</div>", 600),
			check: func(t *testing.T, _ *pageParser, text string) {
				if !strings.Contains(text, "kept") {
					t.Errorf("text lost: %q", text)
				}
			},
		},
		{
			name: "unclosed paragraphs",
			src:  strings.Repeat("<p>x", 200000),
			check: func(t *testing.T, p *pageParser, text string) {
				for i, e := range p.elems[1:] {
					if e.parent != 0 {
						t.Fatalf("paragraph %d is nested under element %d", i+1, e.parent)
					}
				}
				if n := strings.Count(text, "x"); n != 200000 {
					t.Errorf("text has %d of 200000 paragraphs", n)
				}
			},
		},
		{
			name: "unterminated quote",
			src:  `<p>before</p><a href="x>` + strings.Repeat("swallowed ", 100000),
			check: func(t *testing.T, _ *pageParser, text string) {
				if text != "before" {
					t.Errorf("got %q, want only the text before the tag", text[:min(len(text), 40)])
				}
			},
		},
		{
			name: "unterminated comment",
			src:  `<p>before</p><!--` + strings.Repeat("swallowed -- > ", 100000),
			check: func(t *testing.T, _ *pageParser, text string) {
				if text != "before" {
					t.Errorf("got %q, want only the text before the comment", text[:min(len(text), 40)])
				}
			},
		},
		{
			name: "unterminated script full of close tags",
			src:  `<p>before</p><script>` + strings.Repeat("</div></scrip></SCRIPTS>", 50000),
			check: func(t *testing.T, _ *pageParser, text string) {
				if text != "before" {
					t.Errorf("got %q, want only the text before the script", text[:min(len(text), 40)])
				}
			},
		},
		{
			name: "many unclosed attributes",
			src:  strings.Repeat(`<div class=a'b id="c`, 20000),
			check: func(t *testing.T, _ *pageParser, text string) {
				if text != "" {
					t.Errorf("got %q, want no text", text[:min(len(text), 40)])
				}
			},
		},
	}
}

// TestExtractAdversarial runs every adversarial page; each must complete
// promptly (quadratic behaviour would not), keep the element and token
// slices within their up-front reservation, and degrade as its check
// expects.
func TestExtractAdversarial(t *testing.T) {
	for _, tt := range adversarialPages() {
		t.Run(tt.name, func(t *testing.T) {
			p := newPageParser(tt.src)
			markup, starts := countMarkup(tt.src)
			tokenCap, elemCap := cap(p.tokens), cap(p.elems)
			if tokenCap != 2*markup+1 || elemCap != min(starts, maxPageElements-1)+1 {
				t.Fatalf("reservation = %d tokens, %d elements; want %d, %d", tokenCap, elemCap, 2*markup+1, min(starts, maxPageElements-1)+1)
			}
			p.parse()
			if cap(p.tokens) != tokenCap || cap(p.elems) != elemCap {
				t.Errorf("parse grew the slices: %d tokens (reserved %d), %d elements (reserved %d)",
					cap(p.tokens), tokenCap, cap(p.elems), elemCap)
			}
			tt.check(t, p, extract(tt.src))
		})
	}
}

func BenchmarkHTMLToText(b *testing.B) {
	news, err := os.ReadFile(filepath.Join("testdata", "pages", "news-article.html"))
	if err != nil {
		b.Fatal(err)
	}
	cases := []struct{ name, src string }{{"news article", string(news)}}
	for _, p := range adversarialPages() {
		cases = append(cases, struct{ name, src string }{p.name, p.src})
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.src)))
			b.ReportAllocs()
			for b.Loop() {
				htmlToText(c.src)
			}
		})
	}
}
