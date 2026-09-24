package fleet

import (
	"regexp"

	"github.com/rxbynerd/chiron/internal/secret"
)

var (
	markdownImage = regexp.MustCompile(`!\[([^\]]*)\]\(`)
	htmlComment   = regexp.MustCompile(`(?s)<!--.*?-->`)
	// htmlTag matches an element tag but not an autolink such as
	// <https://example.org>, whose name is followed by a colon.
	htmlTag = regexp.MustCompile(`(?s)</?[A-Za-z][A-Za-z0-9-]*(?:\s[^<>]*)?/?>`)
)

// sanitiseAnswer prepares model-written Markdown for a report that a viewer
// may render: images become plain links so no remote beacon fires on open,
// raw HTML is removed so nothing scripted or embedded survives, and
// credential-shaped material is scrubbed. Text content is otherwise kept
// verbatim.
func sanitiseAnswer(s string) string {
	s = markdownImage.ReplaceAllString(s, "[$1](")
	s = htmlComment.ReplaceAllString(s, "")
	s = htmlTag.ReplaceAllString(s, "")
	return secret.Scrub(s)
}
