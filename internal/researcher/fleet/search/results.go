package search

import (
	"encoding/json"

	"github.com/rxbynerd/chiron/internal/mcpclient"
)

// resultsDoc is the ASSUMED search-tool result shape (docs/DECISIONS.md): a
// JSON document {"results":[{"title","url","snippet"}, ...]}, carried either
// as structuredContent or serialised inside a text content block. When the
// shape does not match, Search degrades to surfacing text content rather than
// erroring.
type resultsDoc struct {
	Results []resultItem `json:"results"`
}

type resultItem struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// parseResults maps a tools/call result onto []Result. The assumed shape is a
// JSON document {"results":[{"title","url","snippet"}, ...]} — recorded in
// docs/DECISIONS.md — carried either as structuredContent or serialised in a
// text content block. Parsing degrades gracefully: if neither carries the
// expected shape, the first text block is surfaced as a single Result snippet
// rather than erroring, so a differently-shaped-but-useful reply still feeds
// the worker something.
func parseResults(result mcpclient.ToolResult) []Result {
	if len(result.StructuredContent) > 0 {
		if rs, ok := parseResultsDoc(result.StructuredContent); ok {
			return rs
		}
	}
	for _, block := range result.Content {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		if rs, ok := parseResultsDoc([]byte(block.Text)); ok {
			return rs
		}
	}
	if text := mcpclient.FirstText(result.Content); text != "" {
		return []Result{{Snippet: text}}
	}
	return nil
}

// parseResultsDoc attempts to decode raw JSON as the assumed results document.
// It reports ok only when the JSON is an object carrying a "results" array —
// an empty results array is a valid (zero-hit) success, but arbitrary JSON
// that lacks the key is not, so the caller can fall through to degradation.
func parseResultsDoc(raw []byte) ([]Result, bool) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, false
	}
	if _, present := probe["results"]; !present {
		return nil, false
	}
	var doc resultsDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false
	}
	out := make([]Result, 0, len(doc.Results))
	for _, r := range doc.Results {
		out = append(out, Result(r))
	}
	return out, true
}
