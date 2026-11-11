package fleet

import (
	"regexp"
	"strings"

	"github.com/rxbynerd/chiron/internal/secret"
)

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	// htmlTag matches an element tag but not an autolink such as
	// <https://example.org>, whose name is followed by a colon.
	htmlTag = regexp.MustCompile(`(?s)</?[A-Za-z][A-Za-z0-9-]*(?:\s[^<>]*)?/?>`)
)

// sanitiseAnswer prepares model-written Markdown for a report that a viewer
// may render. Raw HTML tags and comments are removed until none remains, and
// every "<" left that could still open raw HTML is escaped, so nothing
// scripted or embedded survives and no unclosed comment or tag can swallow
// the text after it. Every image opener "![" becomes a plain "[" so no image
// form (inline, reference, shortcut, nested or escaped brackets) can fire a
// remote beacon on open, and credential-shaped material is scrubbed. Text
// content is otherwise kept verbatim.
func sanitiseAnswer(s string) string {
	// Removing one tag can join the text around it into another, so repeat
	// until nothing changes.
	for {
		next := htmlTag.ReplaceAllString(htmlComment.ReplaceAllString(s, ""), "")
		if next == s {
			break
		}
		s = next
	}
	// Removing one opener can join a preceding "!" to the "[" ("!![" becomes
	// "!["), so repeat until none remains.
	for strings.Contains(s, "![") {
		s = strings.ReplaceAll(s, "![", "[")
	}
	return escapeHTMLOpeners(secret.Scrub(s))
}

// escapeHTMLOpeners backslash-escapes every "<" that could open CommonMark
// raw HTML, inline or block, so each renders as a literal "<". It is the
// backstop for anything tag removal missed, such as a "<" inside an
// attribute value or an unclosed comment. Code spans and blocks are not
// special-cased, so an escaped "<" in code shows its backslash.
func escapeHTMLOpeners(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + strings.Count(s, "<"))
	for i := 0; i < len(s); i++ {
		if s[i] == '<' && opensHTML(s, i) {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// opensHTML reports whether the "<" at s[i] could begin raw HTML: it is
// followed by a letter, "/", "!" or "?", the only ways raw HTML begins; it
// does not open an http or https autolink; and no odd run of backslashes
// already escapes it.
func opensHTML(s string, i int) bool {
	if i+1 >= len(s) {
		return false
	}
	switch c := s[i+1]; {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', c == '/', c == '!', c == '?':
	default:
		return false
	}
	if rest := s[i+1:]; strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
		return false
	}
	backslashes := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		backslashes++
	}
	return backslashes%2 == 0
}
