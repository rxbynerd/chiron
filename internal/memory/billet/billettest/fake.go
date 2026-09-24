// Package billettest ships billet.Client's scripted test double. It is a
// separate package from internal/memory/billet so net/http/httptest, needed
// only to script the double, never links into the chiron binary.
package billettest

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/rxbynerd/chiron/internal/mcpclient"
	"github.com/rxbynerd/chiron/internal/mcpclient/mcpclienttest"
	"github.com/rxbynerd/chiron/internal/memory/billet"
)

// searchTool and saveTool mirror billet's unexported tool names (see
// internal/memory/billet/billet.go); the fake speaks the wire protocol
// directly rather than reaching into billet's unexported constants.
const (
	searchTool = "search_memory"
	saveTool   = "save_memory"
)

// Record is one scripted search_memory record.
type Record struct {
	MemoryID  string
	Content   string
	Score     float64
	CreatedAt string
}

// wireRecord mirrors the search_memory record shape billet.Client decodes.
type wireRecord struct {
	MemoryID  string  `json:"memory_id"`
	Content   string  `json:"content"`
	Score     float64 `json:"score"`
	CreatedAt string  `json:"created_at"`
}

// FakeServer is a Billet stand-in built on mcpclienttest.FakeServer, shared
// by every package that drives recall or save-back in tests. search_memory
// answers with the scripted records (at most the requested limit) as
// structuredContent plus a text block carrying the same document, as Billet
// does; save_memory records the content it received and accepts it with a
// fresh id. Callers own its lifecycle: build it at the call site and defer
// Close.
type FakeServer struct {
	inner     *mcpclienttest.FakeServer
	records   []Record
	toolError string
	opts      []mcpclienttest.FakeOption

	mu    sync.Mutex
	saved []string
}

// FakeOption configures a FakeServer at construction.
type FakeOption func(*FakeServer)

// WithToolError makes both tools answer isError:true with text, as Billet
// does for "budget exceeded" or "backend unavailable".
func WithToolError(text string) FakeOption {
	return func(f *FakeServer) { f.toolError = text }
}

// WithRawResult makes every tools/call return raw verbatim as the result
// member, for malformed-shape cases.
func WithRawResult(raw string) FakeOption {
	return func(f *FakeServer) { f.opts = append(f.opts, mcpclienttest.WithRawResult(raw)) }
}

// NewFakeServer starts a fake Billet answering search_memory with records.
// Call Close when done.
func NewFakeServer(records []Record, opts ...FakeOption) *FakeServer {
	f := &FakeServer{records: records}
	for _, o := range opts {
		o(f)
	}
	f.inner = mcpclienttest.NewFakeServer(f.answer, f.opts...)
	return f
}

// URL is the endpoint to pass as Options.Endpoint.
func (f *FakeServer) URL() string { return f.inner.URL() }

// Close shuts the server down.
func (f *FakeServer) Close() { f.inner.Close() }

// Requests returns a copy of the requests received, in arrival order.
func (f *FakeServer) Requests() []mcpclienttest.FakeRequest { return f.inner.Requests() }

// CallCount reports every request received, handshake included.
func (f *FakeServer) CallCount() int { return f.inner.CallCount() }

// ToolCallCount reports the tools/call requests received.
func (f *FakeServer) ToolCallCount() int { return f.inner.ToolCallCount() }

// SavedContents returns the content of every save_memory call, in order.
func (f *FakeServer) SavedContents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.saved))
	copy(out, f.saved)
	return out
}

func (f *FakeServer) answer(tool string, args map[string]any) (mcpclient.ToolResult, error) {
	if f.toolError != "" {
		return mcpclient.ToolResult{
			Content: []mcpclient.ContentBlock{{Type: "text", Text: f.toolError}},
			IsError: true,
		}, nil
	}
	switch tool {
	case searchTool:
		limit := billet.DefaultLimit
		if n, ok := args["limit"].(float64); ok && n > 0 {
			limit = int(n)
		}
		records := make([]wireRecord, 0, len(f.records))
		for i, r := range f.records {
			if i == limit {
				break
			}
			records = append(records, wireRecord(r))
		}
		return structured(map[string]any{"records": records})
	case saveTool:
		content, _ := args["content"].(string)
		f.mu.Lock()
		f.saved = append(f.saved, content)
		id := fmt.Sprintf("mem-fake-%d", len(f.saved))
		f.mu.Unlock()
		return structured(map[string]any{"memory_id": id, "accepted": true})
	default:
		return mcpclient.ToolResult{}, fmt.Errorf("unknown tool %q", tool)
	}
}

func structured(doc any) (mcpclient.ToolResult, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return mcpclient.ToolResult{}, fmt.Errorf("fake: encoding reply: %w", err)
	}
	return mcpclient.ToolResult{
		Content:           []mcpclient.ContentBlock{{Type: "text", Text: string(raw)}},
		StructuredContent: raw,
	}, nil
}
