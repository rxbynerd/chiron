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
// may render: raw HTML is removed so nothing scripted or embedded survives,
// every image opener "![" becomes a plain "[" so no image form (inline,
// reference, shortcut, nested or escaped brackets) can fire a remote beacon
// on open, and credential-shaped material is scrubbed. Text content is
// otherwise kept verbatim.
func sanitiseAnswer(s string) string {
	s = htmlComment.ReplaceAllString(s, "")
	s = htmlTag.ReplaceAllString(s, "")
	// Removing one opener can join a preceding "!" to the "[" ("!![" becomes
	// "!["), so repeat until none remains.
	for strings.Contains(s, "![") {
		s = strings.ReplaceAll(s, "![", "[")
	}
	return secret.Scrub(s)
}
