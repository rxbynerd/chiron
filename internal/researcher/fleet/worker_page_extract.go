package fleet

import (
	"html"
	"math"
	"strings"
	"unicode"
)

// Structural caps. A tag beyond either cap is not pushed as an element: its
// text is still captured and a block tag still breaks the line, so a hostile
// page degrades to a flatter structure rather than an error.
const (
	maxPageDepth    = 512
	maxPageElements = 100_000
	// maxPageSource keeps token offsets within int32; fetched bodies are
	// orders of magnitude smaller.
	maxPageSource = 1 << 30
)

// Content-selection thresholds. Lengths count bytes of non-space text.
const (
	minParagraphBytes     = 25
	creditWalkLimit       = 8
	landmarkMinShare      = 4 // a landmark must hold at least 1/4 of the page's content text
	contentAttrBonus      = 25
	siblingMinScore       = 10.0
	siblingScoreShare     = 0.2
	siblingParagraphBytes = 80
	siblingMaxLinkDensity = 0.25
)

type tagTrait uint32

const (
	traitVoid        tagTrait = 1 << iota // never opens a scope
	traitBlock                            // opening starts a new line
	traitCell                             // opening starts a new table cell
	traitRawText                          // content skipped unparsed and never visible
	traitHidden                           // content parsed but never visible
	traitHeadContent                      // may appear in <head> without closing it
	traitBoilerplate                      // page furniture by tag
	traitForm                             // dropped, but descendants may still be the content root
	traitHeader                           // page furniture unless inside a content landmark
	traitMain
	traitArticle
	traitLink
	traitCode          // descendants are exempt from attribute heuristics
	traitParagraph     // scored as a paragraph when long enough
	traitLeafParagraph // scored as a paragraph only without block children
	traitCandidate     // may be chosen as the content root by scoring
	traitHeading       // joined to a chosen sibling block it introduces
	traitProtected     // never page furniture by attribute
)

// impliedGroup names the elements an opening tag implicitly closes when one
// of them is the current element, a subset of the HTML parser's implied end
// tags that keeps unclosed <p>, <li> and table cells from nesting.
type impliedGroup uint8

const (
	groupP impliedGroup = 1 << iota
	groupLI
	groupDL
	groupCell
	groupRow
	groupTableSection
)

type tagInfo struct {
	traits tagTrait
	group  impliedGroup // the group this element belongs to
	closes impliedGroup // the groups its opening closes
	weight int8         // scoring weight as a candidate root
}

var tagInfos = buildTagInfos()

func buildTagInfos() map[string]tagInfo {
	m := map[string]tagInfo{}
	with := func(names []string, f func(*tagInfo)) {
		for _, n := range names {
			info := m[n]
			f(&info)
			m[n] = info
		}
	}
	add := func(t tagTrait, names ...string) {
		with(names, func(i *tagInfo) { i.traits |= t })
	}
	closes := func(g impliedGroup, names ...string) {
		with(names, func(i *tagInfo) { i.closes |= g })
	}
	headings := []string{"h1", "h2", "h3", "h4", "h5", "h6"}

	add(traitVoid, "area", "base", "br", "col", "embed", "hr", "img", "input",
		"link", "meta", "param", "source", "track", "wbr")
	add(traitBlock, "p", "div", "br", "li", "ul", "ol", "tr", "table", "thead",
		"tbody", "section", "article", "header", "footer", "nav", "aside",
		"blockquote", "pre", "hr", "dt", "dd", "figure", "figcaption", "main",
		"form", "title")
	add(traitBlock, headings...)
	add(traitCell, "td", "th")
	add(traitRawText, "script", "style", "noscript", "template", "svg", "iframe", "object")
	add(traitHidden, "head")
	add(traitHeadContent, "base", "link", "meta", "title", "style", "script", "noscript", "template")
	add(traitBoilerplate, "nav", "footer", "aside")
	add(traitForm, "form")
	add(traitHeader, "header")
	add(traitMain, "main")
	add(traitArticle, "article")
	add(traitLink, "a")
	add(traitCode, "pre", "code")
	add(traitParagraph, "p", "pre")
	add(traitLeafParagraph, "div", "td")
	add(traitCandidate, "div", "section", "article", "main", "body", "table", "td", "blockquote")
	add(traitHeading, headings...)
	add(traitProtected, "html", "body", "main", "article")
	with([]string{"div"}, func(i *tagInfo) { i.weight = 5 })
	with([]string{"td", "blockquote"}, func(i *tagInfo) { i.weight = 3 })

	with([]string{"p"}, func(i *tagInfo) { i.group = groupP })
	with([]string{"li"}, func(i *tagInfo) { i.group = groupLI })
	with([]string{"dt", "dd"}, func(i *tagInfo) { i.group = groupDL })
	with([]string{"td", "th"}, func(i *tagInfo) { i.group = groupCell })
	with([]string{"tr"}, func(i *tagInfo) { i.group = groupRow })
	with([]string{"thead", "tbody", "tfoot"}, func(i *tagInfo) { i.group = groupTableSection })
	closes(groupP, "address", "article", "aside", "blockquote", "center", "details",
		"dialog", "dir", "div", "dl", "fieldset", "figcaption", "figure", "footer",
		"form", "header", "hgroup", "hr", "main", "menu", "nav", "ol", "p", "pre",
		"section", "summary", "table", "ul", "li", "dt", "dd", "td", "th", "tr")
	closes(groupP, headings...)
	closes(groupLI, "li")
	closes(groupDL, "dt", "dd")
	closes(groupCell, "td", "th", "tr", "thead", "tbody", "tfoot")
	closes(groupRow, "tr", "thead", "tbody", "tfoot")
	closes(groupTableSection, "thead", "tbody", "tfoot")
	return m
}

// The boilerplate vocabulary matched against id and class tokens: a token
// matches when it starts with a boilerplate word and not with an exception.
// A token equal to a content word vetoes the match for the whole element.
var (
	boilerplateWords      = []string{"nav", "menu", "sidebar", "footer", "cookie", "banner", "consent", "share", "comment", "advert", "promo", "breadcrumb", "related"}
	boilerplateExceptions = []string{"navy", "commentary"}
	contentWords          = []string{"article", "content", "main", "body", "post", "entry", "story", "text"}
	boilerplateRoles      = []string{"navigation", "banner", "contentinfo", "complementary", "search", "menu", "menubar", "dialog", "alertdialog"}
)

type elemFlag uint32

const (
	flagDrop         elemFlag = 1 << iota // skipped when rendering content
	flagHidden                            // skipped in every rendering
	flagInFurniture                       // inside page furniture: never an article or scored content root
	flagTagFurniture                      // nav, footer, aside or a page header, or inside one: never a content root
	flagInLandmark                        // inside main, article or role=main
	flagInLink                            // inside a link
	flagInCode                            // inside pre or code
	flagLandmarkMain                      // a main element or role=main
	flagContentAttr                       // id or class carries a content word
	flagBlockChild                        // has a block-level child element
	flagScored                            // received paragraph credit
)

// pageElement is one element of a parsed page. Its subtree is the token
// range [open, end) and the element range [index, elemEnd).
type pageElement struct {
	name    string
	info    tagInfo
	parent  int32
	open    int32
	end     int32
	elemEnd int32
	flags   elemFlag
	text    int32 // non-space text bytes in the subtree, excluding dropped descendants
	link    int32 // the part of text inside links
	commas  int32
	score   float64 // paragraph credit received
}

// pageToken is one item of the token stream: the text src[a:b] when a is
// non-negative, otherwise the token kind a (with the element b for tokOpen).
type pageToken struct {
	a, b int32
}

const (
	tokOpen    int32 = -1 - iota // element b opens
	tokNewline                   // a line break from a void or unpushed tag
	tokTab                       // a cell break from an unpushed tag
)

// pageDoc is a page parsed into a flat element slice (element 0 is a virtual
// document root) and a token stream in document order. Text tokens are byte
// ranges of src. Both slices are sized once from the markup count, so memory
// is at most two 8-byte tokens per tag plus one element per start tag up to
// maxPageElements.
type pageDoc struct {
	src    string
	elems  []pageElement
	tokens []pageToken
}

type pageParser struct {
	pageDoc
	stack []int32
	// open counts the stacked elements by name, so a close tag with no open
	// match is dropped in constant time and a matching one pops, amortised,
	// in constant time.
	open map[string]int32
	// overflow counts tags left unpushed by the caps, so their close tags do
	// not pop pushed ancestors. It holds at most maxPageDepth names.
	overflow map[string]int32
	// overflowFull is set once an unpushed tag's name did not fit in
	// overflow. From then on a close tag whose name is not counted may
	// belong to such a tag, so it is ignored rather than popping.
	overflowFull bool
}

func newPageParser(src string) *pageParser {
	if len(src) > maxPageSource {
		src = src[:maxPageSource]
	}
	markup, starts := countMarkup(src)
	elems := make([]pageElement, 1, min(starts, maxPageElements-1)+1)
	elems[0].parent = -1
	return &pageParser{
		pageDoc: pageDoc{
			src:    src,
			elems:  elems,
			tokens: make([]pageToken, 0, 2*markup+1),
		},
		stack:    []int32{0},
		open:     map[string]int32{},
		overflow: map[string]int32{},
	}
}

func parsePage(src string) *pageDoc {
	p := newPageParser(src)
	p.parse()
	return &p.pageDoc
}

// countMarkup counts the '<' that open markup and, of those, the ones that
// open a start tag: upper bounds on the tags and elements parse sees. Each
// tag adds at most one token and ends at most one text run.
func countMarkup(src string) (markup, starts int) {
	for i := 0; ; i++ {
		j := strings.IndexByte(src[i:], '<')
		if j < 0 {
			return markup, starts
		}
		i += j
		if opensMarkup(src, i) {
			markup++
			if isASCIILetter(src[i+1]) {
				starts++
			}
		}
	}
}

// parse tokenizes src in one forward pass. Unterminated markup (a tag,
// comment or raw-text element that never closes) ends the document there.
// Elements still open at the end are closed.
func (p *pageParser) parse() {
	src := p.src
	// textStart is where the pending text run began, or -1 once unterminated
	// markup has ended the document.
	i, textStart := 0, 0
scan:
	for i < len(src) {
		j := strings.IndexByte(src[i:], '<')
		if j < 0 {
			break
		}
		lt := i + j
		if !opensMarkup(src, lt) {
			i = lt + 1
			continue
		}
		p.text(textStart, lt)
		textStart = -1
		if strings.HasPrefix(src[lt:], "<!--") {
			end := strings.Index(src[lt+4:], "-->")
			if end < 0 {
				break
			}
			i = lt + 4 + end + 3
			textStart = i
			continue
		}
		tagEnd := findTagEnd(src, lt)
		if tagEnd < 0 {
			break
		}
		inner := src[lt+1 : tagEnd]
		i = tagEnd + 1
		name, closing := tagName(inner)
		switch {
		case name == "":
		case closing:
			p.closeTag(name)
		case tagInfos[name].traits&traitRawText != 0:
			// Skip to the first matching close tag. svg, template and
			// object can nest; the outer element's content after a nested
			// close tag is then parsed as page markup.
			closeAt := indexCloseTag(src, i, name)
			if closeAt < 0 {
				break scan
			}
			end := findTagEnd(src, closeAt)
			if end < 0 {
				break scan
			}
			i = end + 1
			p.tokens = append(p.tokens, pageToken{a: tokNewline})
		default:
			p.openTag(name, inner)
		}
		textStart = i
	}
	p.text(textStart, len(src))
	for len(p.stack) > 1 {
		p.pop()
	}
	root := &p.elems[0]
	root.end = int32(len(p.tokens))
	root.elemEnd = int32(len(p.elems))
}

func (p *pageParser) top() *pageElement {
	return &p.elems[p.stack[len(p.stack)-1]]
}

// text records src[start:end] as text of the current element.
func (p *pageParser) text(start, end int) {
	if start < 0 || start >= end {
		return
	}
	s := p.src[start:end]
	visible, commas := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isHTMLSpace(c):
			continue
		case c == ',':
			commas++
		case c == 0xEF && strings.HasPrefix(s[i:], "，"), c == 0xE3 && strings.HasPrefix(s[i:], "、"):
			commas++
		}
		visible++
	}
	// Visible text implies the end of <head>.
	if visible > 0 && p.top().info.traits&traitHidden != 0 {
		p.pop()
	}
	p.tokens = append(p.tokens, pageToken{a: int32(start), b: int32(end)})
	e := p.top()
	e.text += int32(visible)
	e.commas += int32(commas)
	if e.flags&flagInLink != 0 || e.info.traits&traitLink != 0 {
		e.link += int32(visible)
	}
}

func (p *pageParser) openTag(name, inner string) {
	info := tagInfos[name]
	p.impliedClose(info)
	if info.traits&traitVoid == 0 && len(p.stack) <= maxPageDepth && len(p.elems) < maxPageElements {
		p.push(name, inner, info)
		return
	}
	switch {
	case info.traits&traitBlock != 0:
		p.tokens = append(p.tokens, pageToken{a: tokNewline})
	case info.traits&traitCell != 0:
		p.tokens = append(p.tokens, pageToken{a: tokTab})
	}
	if info.traits&traitVoid != 0 {
		return
	}
	if n, ok := p.overflow[name]; ok || len(p.overflow) < maxPageDepth {
		p.overflow[name] = n + 1
	} else {
		p.overflowFull = true
	}
}

// impliedClose pops the elements an opening of info implicitly closes: an
// open <head> for anything that does not belong in it, and the current
// element while it is in a group the opening closes.
func (p *pageParser) impliedClose(info tagInfo) {
	for len(p.stack) > 1 {
		top := p.top()
		if (top.info.traits&traitHidden != 0 && info.traits&traitHeadContent == 0) ||
			top.info.group&info.closes != 0 {
			p.pop()
			continue
		}
		return
	}
}

func (p *pageParser) push(name, inner string, info tagInfo) {
	parentIdx := p.stack[len(p.stack)-1]
	parent := &p.elems[parentIdx]
	if info.traits&traitBlock != 0 {
		parent.flags |= flagBlockChild
	}

	e := pageElement{name: name, info: info, parent: parentIdx, open: int32(len(p.tokens))}
	e.flags = parent.flags & (flagInFurniture | flagTagFurniture | flagInLandmark | flagInLink | flagInCode)
	if parent.flags&flagDrop != 0 && parent.info.traits&traitForm == 0 {
		e.flags |= flagInFurniture
	}
	if parent.flags&flagLandmarkMain != 0 || parent.info.traits&traitArticle != 0 {
		e.flags |= flagInLandmark
	}
	if parent.info.traits&traitLink != 0 {
		e.flags |= flagInLink
	}
	if parent.info.traits&traitCode != 0 {
		e.flags |= flagInCode
	}
	e.flags |= classify(info, e.flags, inner)

	idx := int32(len(p.elems))
	p.elems = append(p.elems, e)
	p.tokens = append(p.tokens, pageToken{a: tokOpen, b: idx})
	p.stack = append(p.stack, idx)
	p.open[name]++
}

// classify returns an element's own landmark, content and drop flags from
// its tag and its id, class and role attributes.
func classify(info tagInfo, inherited elemFlag, inner string) elemFlag {
	var f elemFlag
	id, class, role := tagAttrs(inner)
	role = firstField(role)
	if info.traits&traitMain != 0 || asciiEqualFold(role, "main") {
		f |= flagLandmarkMain
	}
	boilerID, contentID := attrWords(id)
	boilerClass, contentClass := attrWords(class)
	if contentID || contentClass {
		f |= flagContentAttr
	}

	switch {
	case info.traits&traitHidden != 0:
		f |= flagDrop | flagHidden
	case info.traits&traitForm != 0:
		f |= flagDrop
	case info.traits&traitBoilerplate != 0, info.traits&traitHeader != 0 && inherited&flagInLandmark == 0:
		f |= flagDrop | flagTagFurniture
	case info.traits&(traitProtected|traitCode) != 0, f&flagLandmarkMain != 0, inherited&flagInCode != 0:
	case containsFold(boilerplateRoles, role):
		f |= flagDrop
	case (boilerID || boilerClass) && !contentID && !contentClass:
		f |= flagDrop
	}
	return f
}

func (p *pageParser) closeTag(name string) {
	if n := p.overflow[name]; n > 0 {
		if n == 1 {
			delete(p.overflow, name)
		} else {
			p.overflow[name] = n - 1
		}
		return
	}
	if p.overflowFull || p.open[name] == 0 {
		return
	}
	for {
		idx := p.stack[len(p.stack)-1]
		p.pop()
		if p.elems[idx].name == name {
			return
		}
	}
}

// pop closes the current element: it credits the element's ancestors if it
// is a scoring paragraph and adds its text totals to its parent unless it is
// dropped from the rendered content.
func (p *pageParser) pop() {
	idx := p.stack[len(p.stack)-1]
	p.stack = p.stack[:len(p.stack)-1]
	e := &p.elems[idx]
	e.end = int32(len(p.tokens))
	e.elemEnd = int32(len(p.elems))
	if n := p.open[e.name] - 1; n > 0 {
		p.open[e.name] = n
	} else {
		delete(p.open, e.name)
	}
	p.credit(idx)
	if e.flags&flagDrop == 0 {
		parent := &p.elems[e.parent]
		parent.text += e.text
		parent.link += e.link
		parent.commas += e.commas
	}
}

// credit scores a closed paragraph as readability does, 1 + its commas (at
// most 10) + one point per 100 bytes (at most 3), and adds that to its
// nearest candidate ancestor and half of it to the next. The walk stops at a
// dropped element, whose text its ancestors never render.
func (p *pageParser) credit(idx int32) {
	e := &p.elems[idx]
	if e.flags&(flagDrop|flagInFurniture) != 0 || e.text < minParagraphBytes {
		return
	}
	leaf := e.info.traits&traitLeafParagraph != 0 && e.flags&flagBlockChild == 0
	if e.info.traits&traitParagraph == 0 && !leaf {
		return
	}
	score := 1 + math.Min(float64(e.commas), 10) + math.Min(float64(e.text/100), 3)
	share := 1.0
	a := e.parent
	for steps := 0; a > 0 && steps < creditWalkLimit; steps++ {
		anc := &p.elems[a]
		if anc.flags&flagDrop != 0 {
			return
		}
		if anc.info.traits&traitCandidate != 0 {
			anc.score += score * share
			anc.flags |= flagScored
			if share < 1 {
				return
			}
			share = 0.5
		}
		a = anc.parent
	}
}

// contentRoots chooses the elements whose subtrees are the page's main
// content: the main landmark, else the largest article, else the
// best-scoring block and its qualifying siblings, else the body (or the
// whole document for a fragment). A main landmark is chosen inside a wrapper
// dropped by its attributes but never inside a furniture tag; an article is
// never chosen inside furniture. A landmark holding under a quarter of the
// page's content text falls through to the next rule.
func (d *pageDoc) contentRoots() []int32 {
	total := d.nonLink(0)
	plausible := func(e int32) bool {
		return e > 0 && int(d.nonLink(e))*landmarkMinShare >= int(total)
	}
	isMain := func(e *pageElement) bool {
		return e.flags&flagLandmarkMain != 0 && e.flags&(flagDrop|flagTagFurniture) == 0
	}
	if m := d.largest(isMain); plausible(m) {
		return []int32{m}
	}
	isArticle := func(e *pageElement) bool {
		return e.info.traits&traitArticle != 0 && e.flags&(flagDrop|flagInFurniture) == 0
	}
	if a := d.largest(isArticle); plausible(a) {
		return []int32{a}
	}
	if c := d.bestCandidate(); c > 0 {
		return d.withSiblings(c)
	}
	for i := 1; i < len(d.elems); i++ {
		if d.elems[i].name == "body" {
			return []int32{int32(i)}
		}
	}
	return []int32{0}
}

// largest returns the matching element with the most non-link text, the
// first in document order on a tie, or -1.
func (d *pageDoc) largest(match func(*pageElement) bool) int32 {
	best, most := int32(-1), int32(-1)
	for i := 1; i < len(d.elems); i++ {
		if e := &d.elems[i]; match(e) && d.nonLink(int32(i)) > most {
			best, most = int32(i), d.nonLink(int32(i))
		}
	}
	return best
}

// bestCandidate returns the highest-scoring eligible block, the first in
// document order on a tie, or -1 when nothing scored.
func (d *pageDoc) bestCandidate() int32 {
	best, bestScore := int32(-1), 0.0
	for i := 1; i < len(d.elems); i++ {
		if !d.eligible(int32(i)) {
			continue
		}
		if s := d.finalScore(int32(i)); s > bestScore {
			best, bestScore = int32(i), s
		}
	}
	return best
}

func (d *pageDoc) eligible(i int32) bool {
	e := &d.elems[i]
	return e.flags&flagScored != 0 && e.flags&(flagDrop|flagInFurniture) == 0
}

// finalScore is a candidate's tag weight, content-word bonus and paragraph
// credit, scaled down by the share of its text that is link text.
func (d *pageDoc) finalScore(i int32) float64 {
	e := &d.elems[i]
	s := e.score + float64(e.info.weight)
	if e.flags&flagContentAttr != 0 {
		s += contentAttrBonus
	}
	return s * (1 - d.linkDensity(i))
}

// withSiblings returns top with the siblings that belong to the same
// content, in document order: blocks scoring at least a fifth of top (and
// at least siblingMinScore), long low-link paragraphs, and headings that
// introduce a joined block.
func (d *pageDoc) withSiblings(top int32) []int32 {
	parent := d.elems[top].parent
	threshold := math.Max(siblingMinScore, d.finalScore(top)*siblingScoreShare)
	var children []int32
	var joined []bool
	for c := parent + 1; c < d.elems[parent].elemEnd; c = d.elems[c].elemEnd {
		children = append(children, c)
		joined = append(joined, c == top || d.joinsSibling(c, threshold))
	}
	for i := len(children) - 2; i >= 0; i-- {
		e := &d.elems[children[i]]
		if e.info.traits&traitHeading != 0 && e.flags&flagDrop == 0 && joined[i+1] {
			joined[i] = true
		}
	}
	var roots []int32
	for i, c := range children {
		if joined[i] {
			roots = append(roots, c)
		}
	}
	return roots
}

func (d *pageDoc) joinsSibling(c int32, threshold float64) bool {
	e := &d.elems[c]
	if e.flags&(flagDrop|flagInFurniture) != 0 {
		return false
	}
	if d.eligible(c) && d.finalScore(c) >= threshold {
		return true
	}
	return e.name == "p" && d.nonLink(c) >= siblingParagraphBytes && d.linkDensity(c) < siblingMaxLinkDensity
}

func (d *pageDoc) nonLink(i int32) int32 {
	return d.elems[i].text - d.elems[i].link
}

func (d *pageDoc) linkDensity(i int32) float64 {
	e := &d.elems[i]
	if e.text == 0 {
		return 0
	}
	return float64(e.link) / float64(e.text)
}

// render returns the entity-decoded text of the roots' subtrees in order,
// with block openings as newlines and cell openings as tabs. In content mode
// dropped elements are skipped with their subtrees; otherwise only hidden
// ones are. A root itself is always rendered.
func (d *pageDoc) render(roots []int32, content bool) string {
	skip := flagHidden
	if content {
		skip = flagDrop
	}
	var b strings.Builder
	b.Grow(len(d.src) / 2)
	for _, r := range roots {
		for i := d.elems[r].open; i < d.elems[r].end; i++ {
			t := d.tokens[i]
			switch {
			case t.a >= 0:
				b.WriteString(d.src[t.a:t.b])
			case t.a == tokNewline:
				b.WriteByte('\n')
			case t.a == tokTab:
				b.WriteByte('\t')
			case t.a == tokOpen:
				e := &d.elems[t.b]
				if t.b != r && e.flags&skip != 0 {
					i = e.end - 1
					continue
				}
				switch {
				case e.info.traits&traitBlock != 0:
					b.WriteByte('\n')
				case e.info.traits&traitCell != 0:
					b.WriteByte('\t')
				}
			}
		}
	}
	return html.UnescapeString(b.String())
}

// tagAttrs returns the id, class and role attribute values from the inside
// of a start tag. Names match ASCII case-insensitively; values may be
// double-quoted, single-quoted or unquoted; the first occurrence wins.
func tagAttrs(inner string) (id, class, role string) {
	var seenID, seenClass, seenRole bool
	i := 0
	for i < len(inner) && !isHTMLSpace(inner[i]) && inner[i] != '/' {
		i++
	}
	for i < len(inner) {
		if c := inner[i]; isHTMLSpace(c) || c == '/' {
			i++
			continue
		}
		start := i
		for i < len(inner) && !isHTMLSpace(inner[i]) && inner[i] != '/' && inner[i] != '=' {
			i++
		}
		name := inner[start:i]
		for i < len(inner) && isHTMLSpace(inner[i]) {
			i++
		}
		value := ""
		if i < len(inner) && inner[i] == '=' {
			i++
			for i < len(inner) && isHTMLSpace(inner[i]) {
				i++
			}
			if i < len(inner) && (inner[i] == '"' || inner[i] == '\'') {
				q := inner[i]
				i++
				n := strings.IndexByte(inner[i:], q)
				if n < 0 {
					n = len(inner) - i
				}
				value = inner[i : i+n]
				i += n + 1
			} else {
				vs := i
				for i < len(inner) && !isHTMLSpace(inner[i]) {
					i++
				}
				value = inner[vs:i]
			}
		}
		switch {
		case !seenID && asciiEqualFold(name, "id"):
			id, seenID = value, true
		case !seenClass && asciiEqualFold(name, "class"):
			class, seenClass = value, true
		case !seenRole && asciiEqualFold(name, "role"):
			role, seenRole = value, true
		}
	}
	return id, class, role
}

// attrWords reports whether an id or class value has a boilerplate token and
// whether it has a content token. The tokens are its runs of ASCII letters
// and digits and, for a camelCase run, also the run's parts, so "SideBar",
// "siteFooter" and "mainContent" all match.
func attrWords(v string) (boilerplate, content bool) {
	check := func(tok string) {
		switch {
		case containsFold(contentWords, tok):
			content = true
		case hasFoldPrefix(tok, boilerplateWords) && !hasFoldPrefix(tok, boilerplateExceptions):
			boilerplate = true
		}
	}
	for i := 0; i < len(v); {
		for i < len(v) && !isASCIIAlnum(v[i]) {
			i++
		}
		start := i
		for i < len(v) && isASCIIAlnum(v[i]) {
			i++
		}
		run := v[start:i]
		if run == "" {
			continue
		}
		check(run)
		part := 0
		for j := 1; j < len(run); j++ {
			if 'A' <= run[j] && run[j] <= 'Z' && 'a' <= run[j-1] && run[j-1] <= 'z' {
				check(run[part:j])
				part = j
			}
		}
		if part > 0 {
			check(run[part:])
		}
	}
	return boilerplate, content
}

// hasFoldPrefix reports whether tok starts with any of words, ignoring ASCII
// case.
func hasFoldPrefix(tok string, words []string) bool {
	for _, w := range words {
		if len(tok) >= len(w) && asciiEqualFold(tok[:len(w)], w) {
			return true
		}
	}
	return false
}

func containsFold(words []string, s string) bool {
	for _, w := range words {
		if asciiEqualFold(s, w) {
			return true
		}
	}
	return false
}

func firstField(s string) string {
	start := 0
	for start < len(s) && isHTMLSpace(s[start]) {
		start++
	}
	end := start
	for end < len(s) && !isHTMLSpace(s[end]) {
		end++
	}
	return s[start:end]
}

func isASCIIAlnum(c byte) bool {
	return isASCIILetter(c) || '0' <= c && c <= '9'
}

func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
