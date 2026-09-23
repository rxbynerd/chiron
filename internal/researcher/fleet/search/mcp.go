package search

import (
	"context"
	"encoding/json"
	"fmt"
)

// initialize / tools/call parameter and result shapes. Only the fields this
// client sends or reads are modelled; unknown fields are ignored on decode.

type initializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    clientCapabilities `json:"capabilities"`
	ClientInfo      clientInfo         `json:"clientInfo"`
}

// clientCapabilities is deliberately empty: this client neither offers
// sampling nor consumes roots/prompts. An empty object is a valid, minimal
// capability advertisement.
type clientCapabilities struct{}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type callToolParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// callToolResult is the tools/call result envelope (MCP spec): a content
// array of typed blocks, an optional structuredContent object, and an
// isError flag distinguishing a tool-level error from a protocol error.
type callToolResult struct {
	Content           []contentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// contentBlock is one block in a tool result's content array. Only text
// blocks are consumed; other types (image, resource) are ignored for search.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// resultsDoc is the ASSUMED search-tool result shape (docs/DECISIONS.md): a
// JSON document {"results":[{"title","url","snippet"}, ...]}, carried either
// as structuredContent or serialised inside a text content block. When the
// shape does not match, Search degrades to surfacing text content rather than
// erroring.
type resultsDoc struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	} `json:"results"`
}

// Search runs the minimal MCP flow — initialize, an initialized notification,
// then a tools/call for the configured tool — and returns the parsed results,
// ending any session the server issued with a best-effort DELETE. The exchange
// is bounded by RequestTimeout (a tighter caller deadline wins) and MaxBodyBytes.
//
// The three round-trips are NOT individually retried. tools/call may invoke a
// billable upstream search, so it is single-attempt like the model adapter's
// paid POST; initialize/initialized are cheap but are not retried either,
// keeping the flow simple and its cost ceiling obvious. A caller that wants to
// retry a failed search does so knowingly.
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	if query == "" {
		return nil, fmt.Errorf("search: query must not be empty")
	}

	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	sess, err := c.initialize(ctx)
	if err != nil {
		return nil, err
	}
	if sess.id != "" {
		defer c.endSession(ctx, sess)
	}
	if err := c.notifyInitialized(ctx, sess); err != nil {
		return nil, err
	}
	return c.callSearch(ctx, sess, query)
}

// initialize performs the MCP initialize handshake and returns the session
// for later requests: any Mcp-Session-Id the server assigned (empty when the
// server is stateless) and the protocol version it chose, falling back to
// mcpProtocolVersion when the result names none.
func (c *Client) initialize(ctx context.Context) (session, error) {
	id := 1
	rpc, sessionID, err := c.doRequest(ctx, session{}, rpcRequest{
		JSONRPC: jsonRPCVersion,
		ID:      &id,
		Method:  "initialize",
		Params: initializeParams{
			ProtocolVersion: mcpProtocolVersion,
			Capabilities:    clientCapabilities{},
			ClientInfo:      clientInfo{Name: "chiron", Version: "v2"},
		},
	})
	if err != nil {
		return session{}, err
	}
	if rpc.Error != nil {
		return session{}, fmt.Errorf("search: initialize rejected: %s", c.scrub(rpc.Error.Error()))
	}
	sess := session{id: sessionID, protocolVersion: mcpProtocolVersion}
	var result initializeResult
	if json.Unmarshal(rpc.Result, &result) == nil && result.ProtocolVersion != "" {
		sess.protocolVersion = result.ProtocolVersion
	}
	return sess, nil
}

// notifyInitialized sends the notifications/initialized notification that
// completes the handshake. A notification has no reply to correlate.
func (c *Client) notifyInitialized(ctx context.Context, sess session) error {
	_, _, err := c.doRequest(ctx, sess, rpcRequest{
		JSONRPC: jsonRPCVersion,
		Method:  "notifications/initialized",
	})
	return err
}

// callSearch issues the tools/call for the configured search tool and parses
// its result.
func (c *Client) callSearch(ctx context.Context, sess session, query string) ([]Result, error) {
	id := 2
	rpc, _, err := c.doRequest(ctx, sess, rpcRequest{
		JSONRPC: jsonRPCVersion,
		ID:      &id,
		Method:  "tools/call",
		Params: callToolParams{
			Name:      c.toolName,
			Arguments: map[string]interface{}{c.queryArgKey: query},
		},
	})
	if err != nil {
		return nil, err
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("search: tools/call rejected: %s", c.scrub(rpc.Error.Error()))
	}

	var result callToolResult
	if err := json.Unmarshal(rpc.Result, &result); err != nil {
		return nil, fmt.Errorf("search: decoding tool result: %s", c.scrub(err.Error()))
	}
	if result.IsError {
		return nil, fmt.Errorf("search: tool reported an error: %s", c.scrub(firstText(result.Content)))
	}
	return parseResults(result), nil
}

// parseResults maps a tools/call result onto []Result. The assumed shape is a
// JSON document {"results":[{"title","url","snippet"}, ...]} — recorded in
// docs/DECISIONS.md — carried either as structuredContent or serialised in a
// text content block. Parsing degrades gracefully: if neither carries the
// expected shape, the first text block is surfaced as a single Result snippet
// rather than erroring, so a differently-shaped-but-useful reply still feeds
// the worker something.
func parseResults(result callToolResult) []Result {
	// Prefer structuredContent when the server provides it.
	if len(result.StructuredContent) > 0 {
		if rs, ok := parseResultsDoc(result.StructuredContent); ok {
			return rs
		}
	}
	// Otherwise look for the JSON document inside a text content block.
	for _, block := range result.Content {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		if rs, ok := parseResultsDoc([]byte(block.Text)); ok {
			return rs
		}
	}
	// Graceful degradation: no recognised results document. Surface the first
	// non-empty text block as a single snippet so the worker still gets the
	// tool's prose rather than an error.
	if text := firstText(result.Content); text != "" {
		return []Result{{Snippet: text}}
	}
	return nil
}

// parseResultsDoc attempts to decode raw JSON as the assumed results document.
// It reports ok only when the JSON is an object carrying a "results" array —
// an empty results array is a valid (zero-hit) success, but arbitrary JSON
// that lacks the key is not, so the caller can fall through to degradation.
func parseResultsDoc(raw []byte) ([]Result, bool) {
	// Distinguish "results key present" from "absent" so a text block that
	// merely happens to be JSON without a results array is not mistaken for a
	// zero-hit success.
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
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
	}
	return out, true
}

// firstText returns the first non-empty text content block, or "".
func firstText(blocks []contentBlock) string {
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}
