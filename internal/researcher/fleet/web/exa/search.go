package exa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
)

const (
	// highlightCharacters asks for short highlights. Exa treats it as a
	// hint; the worker bounds every snippet it renders.
	highlightCharacters = 400
	snippetSeparator    = " … "
	// maxObjectiveUnits keeps the objective under Exa's 4096-character
	// limit, counted in UTF-16 code units so either counting rule passes.
	maxObjectiveUnits = 4000
)

// Search runs one auto-type search for q.Text, asking for highlights as the
// snippet, and returns at most NumResults results. q.Objective is sent, cut
// to fit Exa's limit, only when SendObjective is set. Exa's notices become
// Notes; its warnings are ignored. Spend is the reported cost, Unknown when
// a billed reply did not report one. An error message Exa returns has the
// query and objective redacted.
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

	data, spend, err := c.post(ctx, searchPath, payload, q.Text, body.Objective)
	if err != nil {
		return web.SearchResponse{Spend: spend}, err
	}
	var env searchEnvelope
	if err := decodeObject(data, &env); err != nil {
		return web.SearchResponse{Spend: web.Spend{Unknown: true}}, fmt.Errorf("exa: decoding search response: %s", c.scrub(err.Error()))
	}
	spend = spendFrom(env.CostDollars)
	if env.Results == nil {
		return web.SearchResponse{Spend: spend}, errors.New("exa: decoding search response: no results array")
	}

	resp := web.SearchResponse{Spend: spend, Notes: notes(env.Notices)}
	for _, r := range *env.Results {
		if len(resp.Results) == c.numResults {
			break
		}
		resp.Results = append(resp.Results, web.Result{Title: r.Title, URL: r.URL, Snippet: snippet(r)})
	}
	return resp, nil
}

// decodeObject decodes data, which must be a JSON object, into v.
func decodeObject(data []byte, v any) error {
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("response is not a JSON object")
	}
	return json.Unmarshal(data, v)
}

// snippet is a result's highlights joined, falling back to its text and
// then its summary.
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
	return strings.ToValidUTF8(s, "�")
}

// notes returns the non-blank messages of Exa's notices.
func notes(notices []Notice) []string {
	var out []string
	for _, n := range notices {
		if m := strings.TrimSpace(strings.ToValidUTF8(n.Message, "�")); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// cutUTF16 cuts s on a rune boundary to at most n UTF-16 code units.
func cutUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}
