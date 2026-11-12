package fleet

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// extract reduces src as pageText does for HTML, without the byte bound.
func extract(src string) string {
	return collapseWhitespace(htmlToText(src))
}

// para is long enough to earn paragraph credit.
const para = "This paragraph carries the real content of the page, long enough to score."

func TestAttrWords_ClassifiesBoilerplateAndContent(t *testing.T) {
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
		{"CybotCookiebotDialog", true, false},
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
		{"related-content", true, true},
		{"post comments", true, true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			must := require.New(t)
			b, c := attrWords(tt.value)
			must.Equal(tt.boilerplate, b, "boilerplate flag")
			must.Equal(tt.content, c, "content flag")
		})
	}
}

func TestTagAttrs_ParsesIDClassAndRole(t *testing.T) {
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
			must := require.New(t)
			id, class, role := tagAttrs(tt.inner)
			must.Equal(tt.id, id, "id")
			must.Equal(tt.class, class, "class")
			must.Equal(tt.role, role, "role")
		})
	}
}

// TestExtract_DropsBoilerplateRoles: every boilerplate role drops its element,
// matched case-insensitively on the first role token.
func TestExtract_DropsBoilerplateRoles(t *testing.T) {
	must := require.New(t)
	roles := append([]string{"Navigation", "navigation menu"}, boilerplateRoles...)
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			must := require.New(t)
			got := extract(`<main><div role="` + role + `">Furniture text</div><p>` + para + `</p></main>`)
			must.NotContains(got, "Furniture")
			must.Contains(got, para)
		})
	}
	got := extract(`<main><div role="presentation">Kept text</div><p>` + para + `</p></main>`)
	must.Contains(got, "Kept text", "a non-boilerplate role dropped its element")
}

func TestExtract_SelectsContentRoot(t *testing.T) {
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
			name:   "main landmark inside furniture is not content",
			src:    `<nav><div role="main"><p>` + other + `</p></div></nav><article><p>` + para + `</p></article>`,
			want:   []string{para},
			absent: []string{other},
		},
		{
			name:   "furniture tag with role main is not content",
			src:    `<nav role="main"><p>` + other + `</p></nav><article><p>` + para + `</p></article>`,
			want:   []string{para},
			absent: []string{other},
		},
		{
			name:   "main landmark inside a wrapper dropped by class is content",
			src:    `<div class="has-sidebar"><div class="sidebar">Side links</div><main><p>` + para + `</p></main></div>`,
			want:   []string{para},
			absent: []string{"Side links"},
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
			must := require.New(t)
			got := extract(tt.src)
			for _, w := range tt.want {
				must.Contains(got, w)
			}
			for _, a := range tt.absent {
				must.NotContains(got, a)
			}
		})
	}
}

// TestParsePage_NestedLinkTextCountsAsLinkText: text in elements nested inside a
// link counts as link text exactly as text directly inside the link does.
func TestParsePage_NestedLinkTextCountsAsLinkText(t *testing.T) {
	must := require.New(t)
	const label = "Read more about this story"
	want := parsePage(`<div><p>` + para + `</p><a href="/x">` + label + `</a></div>`).elems[1]
	must.NotZero(want.link, "direct link text should be counted as link text")
	must.NotEqual(want.text, want.link, "direct link text should be part of the text")
	for _, tt := range []struct {
		name, src string
	}{
		{"nested in span", `<div><p>` + para + `</p><a href="/x"><span>` + label + `</span></a></div>`},
		{"nested in bold", `<div><p>` + para + `</p><a href="/x"><b><span>Read more</span> about this story</b></a></div>`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got := parsePage(tt.src).elems[1]
			must.Equal(want.link, got.link, "link bytes")
			must.Equal(want.text, got.text, "text bytes")
		})
	}
}

// TestExtract_CommaCreditIsCapped: a comma-stuffed paragraph earns at most ten
// points for its commas, so a block of it cannot outscore real prose.
func TestExtract_CommaCreditIsCapped(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const story = "The council met on Tuesday evening and, after a long debate, approved the plan " +
		"to build new cycle lanes across the city centre, starting in January."
	src := `<div>` + strings.Repeat(`<p>`+story+`</p>`, 4) + `</div>` +
		`<div id="responses"><p>` + strings.Repeat("Great, ", 200) + `</p></div>`
	d := parsePage(src)
	top := d.bestCandidate()
	must.Positive(top, "no candidate scored")
	check.Contains(d.render([]int32{top}, true), story, "the comma-stuffed block outscored the story")
	check.Contains(extract(src), story, "story missing from the extracted text")
}

// TestExtract_FallsBackToWholeDocument: when the chosen content renders no
// letters or digits, the whole document's visible text is returned instead.
func TestExtract_FallsBackToWholeDocument(t *testing.T) {
	must := require.New(t)
	src := `<body><form><b>Loose text</b> in a form<nav>Menu</nav><script>hidden()</script></form></body>`
	doc := parsePage(src)
	content := doc.render(doc.contentRoots(), true)
	must.False(hasLetterOrDigit(content), "content rendering should have no text: %q", content)
	must.Equal("Loose text in a form\nMenu", extract(src), "the whole document's visible text")
}

func TestParsePage_BuildsElementTree(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
			must := require.New(t)
			must.Equal(tt.want, parents(parsePage(tt.src)))
		})
	}

	d := parsePage(`<div><section><p>text`)
	for i, e := range d.elems {
		check.Equal(int32(len(d.tokens)), e.end, "element %d (%s) is not closed at end of input: end", i, e.name)
		check.Equal(int32(len(d.elems)), e.elemEnd, "element %d (%s) is not closed at end of input: elemEnd", i, e.name)
	}
	must.Equal("text", extract(`<div><section><p>text`), "unclosed elements")
}

func TestHTMLToText_DecodesEntities(t *testing.T) {
	must := require.New(t)
	got := extract(`<p>R&amp;D &pound;5 &#8212; &#x2014; a&nbsp;b &lt;p&gt; &copy &unknown;</p>`)
	must.Equal("R&D £5 — — a b <p> © &unknown;", got)
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
	manyCells := 50000
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
				must := require.New(t)
				must.True(strings.HasSuffix(text, "after scripts"), "text ends %q", text[max(0, len(text)-40):])
				must.NotContains(text, "var s")
			},
		},
		{
			name: "nesting beyond the depth cap",
			src:  strings.Repeat("<div>", deepDivs) + "deep text" + strings.Repeat("</div>", deepDivs) + "<p>after</p>",
			check: func(t *testing.T, p *pageParser, text string) {
				check := assert.New(t)
				check.LessOrEqual(len(p.elems), maxPageDepth+2, "elements should stay within the depth cap")
				check.Contains(text, "deep text", "text lost")
				check.True(strings.HasSuffix(text, "after"), "text lost: %q", text[max(0, len(text)-60):])
				last := p.elems[len(p.elems)-1]
				check.Equal("p", last.name, "close tags beyond the cap must not pop pushed elements")
				check.Zero(last.parent, "the trailing paragraph must sit under the root")
			},
		},
		{
			name: "distinct unclosed tags beyond the depth cap",
			src:  distinct.String() + "tail text",
			check: func(t *testing.T, p *pageParser, text string) {
				check := assert.New(t)
				check.LessOrEqual(len(p.overflow), maxPageDepth, "overflow names should stay within the depth cap")
				check.Contains(text, "tail text", "text lost")
			},
		},
		{
			name: "table cells beyond the depth cap",
			src: strings.Repeat("<div>", maxPageDepth) + "<table><tr>" + strings.Repeat("<td>cell", manyCells) +
				"</table>" + strings.Repeat("</div>", maxPageDepth) + "<p>after</p>",
			check: func(t *testing.T, p *pageParser, _ string) {
				check := assert.New(t)
				all := p.render([]int32{0}, false)
				check.Equal(manyCells, strings.Count(all, "\tcell"), "every cell should follow a tab")
				check.True(strings.HasSuffix(all, "\nafter"), "text after the table is lost: %q", all[max(0, len(all)-40):])
			},
		},
		{
			// Past the depth cap with the overflow names saturated, the inner
			// <section> is uncounted; its close must not pop the pushed one.
			name: "close tag of an uncounted overflow name",
			src:  "<section>" + strings.Repeat("<div>", maxPageDepth) + distinct.String() + "<section>inner</section>tail",
			check: func(t *testing.T, p *pageParser, _ string) {
				must := require.New(t)
				must.Equal("section", p.elems[1].name, "element 1 should be the pushed section")
				got := p.render([]int32{1}, false)
				must.True(strings.HasSuffix(got, "tail"), "the pushed section closed early: its text ends %q", got[max(0, len(got)-40):])
			},
		},
		{
			name: "element count beyond the cap",
			src:  strings.Repeat("<span>x</span>", manyElements),
			check: func(t *testing.T, p *pageParser, text string) {
				check := assert.New(t)
				check.Len(p.elems, maxPageElements, "element count should be capped")
				check.Equal(manyElements, strings.Count(text, "x"), "text should keep every span's text")
			},
		},
		{
			name: "unmatched close tags under a deep stack",
			src:  strings.Repeat("<div>", 600) + strings.Repeat("</span>", 200000) + "kept" + strings.Repeat("</div>", 600),
			check: func(t *testing.T, _ *pageParser, text string) {
				must := require.New(t)
				must.Contains(text, "kept", "text lost")
			},
		},
		{
			name: "unclosed paragraphs",
			src:  strings.Repeat("<p>x", 200000),
			check: func(t *testing.T, p *pageParser, text string) {
				must := require.New(t)
				for i, e := range p.elems[1:] {
					must.Zero(e.parent, "paragraph %d is nested under an element", i+1)
				}
				must.Equal(200000, strings.Count(text, "x"), "text should keep every paragraph")
			},
		},
		{
			name: "unterminated quote",
			src:  `<p>before</p><a href="x>` + strings.Repeat("swallowed ", 100000),
			check: func(t *testing.T, _ *pageParser, text string) {
				must := require.New(t)
				must.Equal("before", text[:min(len(text), 40)], "text should stop before the tag")
			},
		},
		{
			name: "unterminated comment",
			src:  `<p>before</p><!--` + strings.Repeat("swallowed -- > ", 100000),
			check: func(t *testing.T, _ *pageParser, text string) {
				must := require.New(t)
				must.Equal("before", text[:min(len(text), 40)], "text should stop before the comment")
			},
		},
		{
			name: "unterminated script full of close tags",
			src:  `<p>before</p><script>` + strings.Repeat("</div></scrip></SCRIPTS>", 50000),
			check: func(t *testing.T, _ *pageParser, text string) {
				must := require.New(t)
				must.Equal("before", text[:min(len(text), 40)], "text should stop before the script")
			},
		},
		{
			name: "tag openers without an end",
			src:  "before" + strings.Repeat("<a", 1<<19),
			check: func(t *testing.T, _ *pageParser, text string) {
				must := require.New(t)
				must.Equal("before", text[:min(len(text), 40)], "text should stop before the tag")
			},
		},
		{
			name: "many unclosed attributes",
			src:  strings.Repeat(`<div class=a'b id="c`, 20000),
			check: func(t *testing.T, _ *pageParser, text string) {
				must := require.New(t)
				must.Empty(text[:min(len(text), 40)], "text should be empty")
			},
		},
	}
}

// TestExtract_AdversarialInputsStayBounded runs every adversarial page; each must
// complete promptly (quadratic behaviour would not), keep the element and token
// slices within their up-front reservation, and degrade as its check expects.
func TestExtract_AdversarialInputsStayBounded(t *testing.T) {
	for _, tt := range adversarialPages() {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			p := newPageParser(tt.src)
			markup, starts := countMarkup(tt.src)
			tokenCap, elemCap := cap(p.tokens), cap(p.elems)
			must.Equal(2*markup+1, tokenCap, "token reservation")
			must.Equal(min(starts, maxPageElements-1)+1, elemCap, "element reservation")
			p.parse()
			must.Equal(tokenCap, cap(p.tokens), "parse grew the token slice")
			must.Equal(elemCap, cap(p.elems), "parse grew the element slice")
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
