package exa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
)

const (
	// highlightCharacters asks for short highlights. Exa treats it as a
	// hint, so maxSnippetBytes is the bound that holds.
	highlightCharacters = 400
	maxSnippetBytes     = 1000
	snippetSeparator    = " … "
	// maxObjectiveUnits keeps the objective under Exa's 4096-character
	// limit, counted in UTF-16 code units so either counting rule passes.
	maxObjectiveUnits = 4000
	maxNotes          = 5
	maxNoteBytes      = 500
)

// Search runs one auto-type search for q.Text, asking for highlights as the
// snippet, and returns at most NumResults results. q.Objective is sent, cut
// to fit Exa's limit, only when SendObjective is set. Exa's notices become
// Notes; its warnings are ignored. Spend is the reported cost, Unknown when
// a billed reply did not report one.
func (c *Client) Search(ctx context.Context, q web.Query) (web.SearchResponse, error) {
	if strings.TrimSpace(q.Text) == "" {
		return web.SearchResponse{}, errors.New("exa: query must not be empty")
	}
	body := searchRequest{
		Query:      q.Text,
		Type:       "auto",
		NumResults: c.numResults,
		Contents:   searchContents{Highlights: highlightsOptions{MaxCharacters: highlightCharacters}},
	}
	if c.sendObjective {
		body.Objective = cutUTF16(strings.TrimSpace(q.Objective), maxObjectiveUnits)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return web.SearchResponse{}, fmt.Errorf("exa: encoding search request: %w", err)
	}

	data, spend, err := c.post(ctx, searchPath, payload)
	if err != nil {
		return web.SearchResponse{Spend: spend}, err
	}
	env, err := decodeSearch(data)
	if err != nil {
		return web.SearchResponse{Spend: web.Spend{Unknown: true}}, fmt.Errorf("exa: decoding search response: %s", c.scrub(err.Error()))
	}

	resp := web.SearchResponse{Spend: spendFrom(env.CostDollars), Notes: notes(env.Notices)}
	for _, r := range *env.Results {
		if len(resp.Results) == c.numResults {
			break
		}
		resp.Results = append(resp.Results, web.Result{Title: r.Title, URL: r.URL, Snippet: snippet(r)})
	}
	return resp, nil
}

// decodeSearch requires a JSON object carrying a results array.
func decodeSearch(data []byte) (searchEnvelope, error) {
	var env searchEnvelope
	if err := decodeObject(data, &env); err != nil {
		return env, err
	}
	if env.Results == nil {
		return env, errors.New("response has no results array")
	}
	return env, nil
}

// decodeObject decodes data, which must be a JSON object, into v.
func decodeObject(data []byte, v any) error {
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("response is not a JSON object")
	}
	return json.Unmarshal(data, v)
}

// snippet is a result's highlights joined, falling back to its text and
// then its summary, cut to maxSnippetBytes.
func snippet(r SearchResult) string {
	var parts []string
	for _, h := range r.Highlights {
		if h = strings.TrimSpace(h); h != "" {
			parts = append(parts, h)
		}
	}
	s := strings.Join(parts, snippetSeparator)
	if s == "" {
		s = strings.TrimSpace(r.Text)
	}
	if s == "" {
		s = strings.TrimSpace(r.Summary)
	}
	return boundBytes(strings.ToValidUTF8(s, "�"), maxSnippetBytes)
}

// notes maps Exa's notices to one-line, bounded notes, at most maxNotes.
func notes(notices []Notice) []string {
	var out []string
	for _, n := range notices {
		if len(out) == maxNotes {
			break
		}
		if m := boundBytes(oneLine(strings.ToValidUTF8(n.Message, "�")), maxNoteBytes); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// cutUTF16 cuts s on a rune boundary to at most n UTF-16 code units.
func cutUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}
