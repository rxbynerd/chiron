// Package billet adapts Billet, the Equestrianism suite's memory sidecar, to
// the memory.Recaller and memory.Rememberer seams (docs/KNOWLEDGE.md §3.2).
// Billet exposes search_memory and save_memory over MCP Streamable HTTP; the
// transport, its bounds and its key handling live in internal/mcpclient.
//
// Billet binds one namespace per process and accepts none per call, so the
// Namespace argument is only copied into each returned Reference.
package billet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rxbynerd/chiron/internal/mcpclient"
	"github.com/rxbynerd/chiron/internal/memory"
)

const (
	searchTool = "search_memory"
	saveTool   = "save_memory"

	// DefaultLimit is the hits per recall when neither Query.Limit nor
	// Options.DefaultLimit is positive.
	DefaultLimit = 5
	// MaxLimit caps the hits requested and returned per recall.
	MaxLimit = 20
	// DefaultMaxHitBytes bounds each recalled record's content.
	DefaultMaxHitBytes = 8 << 10
	// MaxContentBytes is Billet's save_memory content limit, enforced before
	// any request.
	MaxContentBytes = 256 << 10

	// LocatorPrefix prefixes a memory id to form Reference.Locator.
	LocatorPrefix = "billet://memory/"

	maxNameRunes    = 120
	maxToolErrBytes = 4 << 10
	maxMemoryIDLen  = 256
	defaultKind     = "fact"
	labelKind       = "kind"
	labelCreatedAt  = "created_at"
	recallMediaType = "text/plain"
)

// Options configures a Client.
type Options struct {
	// Endpoint is Billet's MCP listener, e.g. http://127.0.0.1:8140/ or
	// https://billet.internal/. Validated as mcpclient.Options.Endpoint.
	Endpoint string
	// APIKey is optional: Billet itself is unauthenticated, but a fronting
	// proxy may not be. Header-only.
	APIKey string
	// HTTPClient supplies the underlying client; nil builds one.
	HTTPClient *http.Client
	// RequestTimeout bounds one Recall or Remember. Default 30s.
	RequestTimeout time.Duration
	// DefaultLimit is the hits per recall when Query.Limit <= 0. Default 5,
	// clamped to MaxLimit.
	DefaultLimit int
	// MaxHitBytes bounds each recalled record's content, cut at a rune
	// boundary. Default DefaultMaxHitBytes.
	MaxHitBytes int
}

// Client recalls from and remembers into one Billet server.
type Client struct {
	mcp          *mcpclient.Client
	defaultLimit int
	maxHitBytes  int
}

var (
	_ memory.Recaller   = (*Client)(nil)
	_ memory.Rememberer = (*Client)(nil)
)

// New builds a Client, validating the endpoint before any request is made.
func New(opts Options) (*Client, error) {
	mc, err := mcpclient.New(mcpclient.Options{
		Endpoint:       opts.Endpoint,
		APIKey:         opts.APIKey,
		HTTPClient:     opts.HTTPClient,
		RequestTimeout: opts.RequestTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("billet: %w", err)
	}
	c := &Client{mcp: mc, defaultLimit: opts.DefaultLimit, maxHitBytes: opts.MaxHitBytes}
	if c.defaultLimit <= 0 {
		c.defaultLimit = DefaultLimit
	}
	c.defaultLimit = min(c.defaultLimit, MaxLimit)
	if c.maxHitBytes <= 0 {
		c.maxHitBytes = DefaultMaxHitBytes
	}
	return c, nil
}

// wireRecord is one search_memory record.
type wireRecord struct {
	MemoryID  string  `json:"memory_id"`
	Content   string  `json:"content"`
	Score     float64 `json:"score"`
	CreatedAt string  `json:"created_at"`
}

// Recall calls search_memory and maps each record per KNOWLEDGE §2. A reply
// without a records document is an error: the worker must never mistake
// arbitrary text for a memory.
func (c *Client) Recall(ctx context.Context, ns memory.Namespace, q memory.Query) ([]memory.Recalled, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, errors.New("billet: recall query must not be empty")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = c.defaultLimit
	}
	limit = min(limit, MaxLimit)

	result, err := c.mcp.CallTool(ctx, searchTool, map[string]any{"query": q.Text, "limit": limit})
	if err != nil {
		return nil, fmt.Errorf("billet: %s: %w", searchTool, err)
	}
	if result.IsError {
		return nil, toolError(searchTool, result)
	}

	var doc struct {
		Records []wireRecord `json:"records"`
	}
	if err := decodeReply(result, "records", &doc); err != nil {
		return nil, fmt.Errorf("billet: %s: %w", searchTool, err)
	}

	records := doc.Records
	if len(records) > limit {
		records = records[:limit]
	}
	out := make([]memory.Recalled, 0, len(records))
	for i, r := range records {
		if err := validateMemoryID(r.MemoryID); err != nil {
			return nil, fmt.Errorf("billet: %s: record %d: %w", searchTool, i, err)
		}
		hit := memory.Recalled{
			Reference: reference(ns, r.MemoryID),
			Memory: memory.Memory{
				Text: truncateRunes(r.Content, c.maxHitBytes),
				Meta: memory.ArtifactMeta{
					Name:      firstLine(r.Content),
					MediaType: recallMediaType,
				},
			},
			Score: r.Score,
		}
		if r.CreatedAt != "" {
			hit.Memory.Meta.Labels = map[string]string{labelCreatedAt: r.CreatedAt}
		}
		out = append(out, hit)
	}
	return out, nil
}

// Remember calls save_memory with the memory's text and kind
// (Labels["kind"], "fact" or "event", default "fact"). Content over
// MaxContentBytes and an unknown kind are refused before any request.
func (c *Client) Remember(ctx context.Context, ns memory.Namespace, m memory.Memory) (memory.Reference, error) {
	if strings.TrimSpace(m.Text) == "" {
		return memory.Reference{}, errors.New("billet: memory content must not be empty")
	}
	if len(m.Text) > MaxContentBytes {
		return memory.Reference{}, fmt.Errorf("billet: memory content is %d bytes, over Billet's %d-byte limit", len(m.Text), MaxContentBytes)
	}
	kind := m.Meta.Labels[labelKind]
	if kind == "" {
		kind = defaultKind
	}
	if kind != "fact" && kind != "event" {
		return memory.Reference{}, fmt.Errorf("billet: memory kind %q must be \"fact\" or \"event\"", kind)
	}

	result, err := c.mcp.CallTool(ctx, saveTool, map[string]any{"content": m.Text, "kind": kind})
	if err != nil {
		return memory.Reference{}, fmt.Errorf("billet: %s: %w", saveTool, err)
	}
	if result.IsError {
		return memory.Reference{}, toolError(saveTool, result)
	}

	var doc struct {
		MemoryID string `json:"memory_id"`
		Accepted bool   `json:"accepted"`
	}
	if err := decodeReply(result, "memory_id", &doc); err != nil {
		return memory.Reference{}, fmt.Errorf("billet: %s: %w", saveTool, err)
	}
	if !doc.Accepted {
		return memory.Reference{}, fmt.Errorf("billet: %s: memory not accepted", saveTool)
	}
	if err := validateMemoryID(doc.MemoryID); err != nil {
		return memory.Reference{}, fmt.Errorf("billet: %s: %w", saveTool, err)
	}
	return reference(ns, doc.MemoryID), nil
}

// decodeReply decodes the first of structuredContent or a text block that is
// a JSON object carrying key into dst. A candidate carrying key that fails to
// decode is an error rather than a fall-through, as is no candidate at all.
func decodeReply(result mcpclient.ToolResult, key string, dst any) error {
	candidates := make([][]byte, 0, 1+len(result.Content))
	if len(result.StructuredContent) > 0 {
		candidates = append(candidates, result.StructuredContent)
	}
	for _, b := range result.Content {
		if b.Type == "text" && b.Text != "" {
			candidates = append(candidates, []byte(b.Text))
		}
	}
	for _, raw := range candidates {
		var probe map[string]json.RawMessage
		if json.Unmarshal(raw, &probe) != nil {
			continue
		}
		if _, ok := probe[key]; !ok {
			continue
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("malformed reply: %v", err)
		}
		return nil
	}
	return fmt.Errorf("reply carries no %q document", key)
}

// toolError surfaces a tool-level failure with the tool's own text, which
// mcpclient has already scrubbed, bounded to maxToolErrBytes.
func toolError(tool string, result mcpclient.ToolResult) error {
	text := mcpclient.FirstText(result.Content)
	if text == "" {
		text = "no detail"
	}
	if len(text) > maxToolErrBytes {
		const marker = " [truncated]"
		text = truncateRunes(text, maxToolErrBytes-len(marker)) + marker
	}
	return fmt.Errorf("billet: %s failed: %s", tool, text)
}

func reference(ns memory.Namespace, id string) memory.Reference {
	return memory.Reference{Namespace: ns, Digest: id, Locator: LocatorPrefix + id}
}

// validateMemoryID admits the conservative id alphabet Billet's backends
// issue, so an id is safe to embed in a locator and render in a transcript.
func validateMemoryID(id string) error {
	if id == "" {
		return errors.New("record has no memory_id")
	}
	if len(id) > maxMemoryIDLen {
		return fmt.Errorf("memory_id exceeds %d bytes", maxMemoryIDLen)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':':
		default:
			return fmt.Errorf("memory_id contains %q; only letters, digits and -_.: are accepted", r)
		}
	}
	return nil
}

// firstLine is the content's first non-blank line, trimmed and bounded to
// maxNameRunes, standing in for the title Billet does not store.
func firstLine(s string) string {
	for line := range strings.Lines(s) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > maxNameRunes {
			return string([]rune(line)[:maxNameRunes])
		}
		return line
	}
	return ""
}

// truncateRunes cuts s to at most maxBytes without splitting a rune.
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
