package gemini

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/rxbynerd/chiron/internal/interactions"
)

// buildInput assembles the create request's input: the prompt string
// alone when there is no multimodal grounding, otherwise a Content
// array opening with the prompt followed by one typed part per --input
// (docs/INTERACTIONS-API.md §3). Local files travel as base64 data;
// URLs travel as uri references for the API to fetch.
func buildInput(prompt string, inputs []string) (any, error) {
	if len(inputs) == 0 {
		return prompt, nil
	}
	parts := []interactions.Content{{Type: interactions.ContentText, Text: prompt}}
	for _, in := range inputs {
		part, err := inputPart(in)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// inputPart builds one grounding part from a path or URL, inferring
// the MIME type and choosing the part type from it: image/* parts are
// images, everything else is a document.
func inputPart(in string) (interactions.Content, error) {
	if strings.HasPrefix(in, "http://") || strings.HasPrefix(in, "https://") {
		mimeType := mimeFromName(in)
		if mimeType == "" {
			return interactions.Content{}, fmt.Errorf("gemini: cannot infer a MIME type for input URL %q — name the document with a recognised extension", in)
		}
		return interactions.Content{Type: partType(mimeType), MIMEType: mimeType, URI: in}, nil
	}

	data, err := os.ReadFile(in)
	if err != nil {
		return interactions.Content{}, fmt.Errorf("gemini: reading input %s: %w", in, err)
	}
	mimeType := mimeFromName(in)
	if mimeType == "" {
		// Sniff content as a fallback for extensionless local files;
		// strip the parameters DetectContentType appends.
		mimeType = http.DetectContentType(data)
		if i := strings.IndexByte(mimeType, ';'); i >= 0 {
			mimeType = strings.TrimSpace(mimeType[:i])
		}
	}
	return interactions.Content{Type: partType(mimeType), MIMEType: mimeType, Data: data}, nil
}

// partType selects the wire part type for a MIME type.
func partType(mimeType string) interactions.ContentType {
	if strings.HasPrefix(mimeType, "image/") {
		return interactions.ContentImage
	}
	return interactions.ContentDocument
}

// mimeFromName infers a MIME type from a path or URL extension using a
// hand-rolled table — mime.TypeByExtension consults platform databases
// and would make requests differ across machines (the same rationale as
// the formatter's extension table). Unknown extensions return "".
func mimeFromName(name string) string {
	// Trim query/fragment so URL extensions resolve.
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".md", ".markdown":
		return "text/markdown"
	case ".txt":
		return "text/plain"
	case ".html", ".htm":
		return "text/html"
	case ".csv":
		return "text/csv"
	case ".json":
		return "application/json"
	default:
		return ""
	}
}
