package fleet

import (
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"
)

// renderedPage is a fetched page reduced to transcript text.
type renderedPage struct {
	text      string
	truncated bool
}

// pageText reduces a fetched page to bounded plain text for the transcript.
// HTML is reduced to the visible text of its main content; other textual
// media types are passed through; anything else (images, PDFs, archives) is
// reported as unusable so the loop feeds that back to the model instead of
// inlining bytes. The result is valid UTF-8, whitespace-collapsed, and cut at
// maxBytes on a rune boundary after extraction.
func pageText(page fetchedPage, maxBytes int) (renderedPage, bool) {
	mediaType, _, _ := mime.ParseMediaType(page.ContentType)
	if mediaType == "" {
		mediaType, _, _ = mime.ParseMediaType(http.DetectContentType(page.Content))
	}

	var text string
	switch {
	case mediaType == "text/html", mediaType == "application/xhtml+xml":
		text = htmlToText(string(page.Content))
	case isTextualMediaType(mediaType):
		text = string(page.Content)
	default:
		return renderedPage{}, false
	}

	text = strings.ToValidUTF8(text, "�")
	text = collapseWhitespace(text)

	truncated := false
	if len(text) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
		truncated = true
	}
	return renderedPage{text: text, truncated: truncated}, true
}

func isTextualMediaType(mediaType string) bool {
	switch {
	case strings.HasPrefix(mediaType, "text/"):
		return true
	case mediaType == "application/json", mediaType == "application/xml",
		mediaType == "application/javascript", mediaType == "application/x-ndjson":
		return true
	case strings.HasSuffix(mediaType, "+json"), strings.HasSuffix(mediaType, "+xml"):
		return true
	}
	return false
}

// htmlToText reduces an HTML document to the visible text of its main
// content (see pageDoc.contentRoots), with block-element and table-cell
// openings as newlines and tabs and entities decoded. When that content has
// no letters or digits but the whole document does, the whole document's
// visible text is returned instead. It is a tolerant scanner, not a parser:
// malformed markup degrades to leftover text rather than an error.
func htmlToText(src string) string {
	doc := parsePage(src)
	text := doc.render(doc.contentRoots(), true)
	if !hasLetterOrDigit(text) {
		if all := doc.render([]int32{0}, false); hasLetterOrDigit(all) {
			return all
		}
	}
	return text
}

// opensMarkup reports whether the '<' at src[i] starts a tag, comment or
// declaration. As in the HTML tokenizer, a '<' followed by anything other
// than a letter, '/', '!' or '?' is literal text.
func opensMarkup(src string, i int) bool {
	if i+1 >= len(src) {
		return false
	}
	c := src[i+1]
	return isASCIILetter(c) || c == '/' || c == '!' || c == '?'
}

// findTagEnd returns the index of the '>' closing the tag that opens at
// start, or -1 if the tag never closes. A quote opens a quoted value only
// directly after '=' (whitespace aside), so a stray apostrophe in an
// attribute name or unquoted value does not swallow the rest of the page.
func findTagEnd(src string, start int) int {
	var quote byte
	afterEquals := false
	for j := start + 1; j < len(src); j++ {
		c := src[j]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '>':
			return j
		case c == '=':
			afterEquals = true
		case afterEquals && (c == '"' || c == '\''):
			quote = c
			afterEquals = false
		case isHTMLSpace(c):
		default:
			afterEquals = false
		}
	}
	return -1
}

// indexCloseTag returns the index of the first "</name" at or after from,
// matched ASCII case-insensitively and followed by whitespace, '/', '>' or
// the end of input, or -1 if there is none. It never allocates and its
// cursor only advances, so skipping many raw-text elements stays linear.
func indexCloseTag(src string, from int, name string) int {
	for i := from; i < len(src); {
		j := strings.Index(src[i:], "</")
		if j < 0 {
			return -1
		}
		start := i + j
		k := start + 2
		end := k + len(name)
		if end <= len(src) && asciiEqualFold(src[k:end], name) &&
			(end == len(src) || isHTMLSpace(src[end]) || src[end] == '/' || src[end] == '>') {
			return start
		}
		i = k
	}
	return -1
}

func isASCIILetter(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

func isHTMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// asciiEqualFold reports whether a and b are equal under ASCII case folding,
// the folding HTML uses for tag and attribute names.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if asciiLower(a[i]) != asciiLower(b[i]) {
			return false
		}
	}
	return true
}

func asciiLower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// tagName extracts the lower-cased element name from the inside of a tag and
// reports whether it is a closing tag. Declarations and processing
// instructions yield an empty name.
func tagName(inner string) (name string, closing bool) {
	inner = strings.TrimSpace(inner)
	if inner == "" || inner[0] == '!' || inner[0] == '?' {
		return "", false
	}
	if inner[0] == '/' {
		closing = true
		inner = inner[1:]
	}
	end := 0
	for end < len(inner) {
		c := inner[end]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '/' || c == '>' {
			break
		}
		end++
	}
	return strings.ToLower(inner[:end]), closing
}

// collapseWhitespace trims trailing spaces from every line, squeezes runs of
// spaces and tabs, and limits blank-line runs to one.
func collapseWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
