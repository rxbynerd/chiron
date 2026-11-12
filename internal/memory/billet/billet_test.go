package billet_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/mcpclient"
	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/memory/billet"
	"github.com/rxbynerd/chiron/internal/memory/billet/billettest"
)

const testKey = "sk-billet-0123456789abcdefABCDEF"

func newClient(t *testing.T, endpoint string, mutate ...func(*billet.Options)) *billet.Client {
	t.Helper()
	must := require.New(t)
	opts := billet.Options{Endpoint: endpoint}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := billet.New(opts)
	must.NoError(err)
	return c
}

func TestBillet_Recall_MapsRecordsToHits(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	longTitle := strings.Repeat("é", 130)
	fake := billettest.NewFakeServer([]billettest.Record{
		{MemoryID: "mem-1", Content: "\n  Paris is the capital of France.  \nSecond line.", Score: 0.75, CreatedAt: "2026-09-01T10:00:00Z"},
		{MemoryID: "mem-2", Content: longTitle + "\nbody", Score: 0.5},
	})
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := c.Recall(context.Background(), "team-a", memory.Query{Text: "capital of France", Limit: 4})
	must.NoError(err)
	must.Len(got, 2, "hits: %+v", got)

	first := got[0]
	check.Equal(memory.Reference{Namespace: "team-a", Digest: "mem-1", Locator: "billet://memory/mem-1"}, first.Reference)
	check.Equal("\n  Paris is the capital of France.  \nSecond line.", first.Memory.Text, "text is the content verbatim")
	check.Equal("Paris is the capital of France.", first.Memory.Meta.Name, "name is the first non-blank line, trimmed")
	check.Equal("text/plain", first.Memory.Meta.MediaType)
	check.Equal(map[string]string{"created_at": "2026-09-01T10:00:00Z"}, first.Memory.Meta.Labels, "labels hold only created_at")
	check.InDelta(0.75, first.Score, 0)

	second := got[1]
	check.Equal(120, utf8.RuneCountInString(second.Memory.Meta.Name), "name is the first line bounded to 120 runes")
	check.Nil(second.Memory.Meta.Labels, "labels stay nil without created_at")

	call := fake.Requests()[2]
	check.Equal("search_memory", call.ToolName)
	check.Equal("capital of France", call.Arguments["query"])
	check.InDelta(4, call.Arguments["limit"], 0)
	check.Len(call.Arguments, 2, "arguments are exactly query and limit")
}

func TestBillet_Recall_ClampsSearchLimit(t *testing.T) {
	tests := []struct {
		name         string
		defaultLimit int
		queryLimit   int
		want         float64
	}{
		{"defaults to five", 0, 0, 5},
		{"negative query limit uses default", 0, -3, 5},
		{"configured default", 7, 0, 7},
		{"configured default clamped", 99, 0, 20},
		{"query limit wins", 7, 3, 3},
		{"query limit clamped", 0, 50, 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := billettest.NewFakeServer(nil)
			defer fake.Close()

			c := newClient(t, fake.URL(), func(o *billet.Options) { o.DefaultLimit = tt.defaultLimit })
			_, err := c.Recall(context.Background(), "", memory.Query{Text: "q", Limit: tt.queryLimit})
			must.NoError(err)
			must.InDelta(tt.want, fake.Requests()[2].Arguments["limit"], 0, "limit sent")
		})
	}
}

func TestBillet_Recall_TruncatesHitsToLimit(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	// A server returning more records than asked is cut to the limit.
	raw := `{"content":[],"structuredContent":{"records":[` +
		`{"memory_id":"a","content":"1","score":0.9},` +
		`{"memory_id":"b","content":"2","score":0.8},` +
		`{"memory_id":"c","content":"3","score":0.7}]}}`
	fake := billettest.NewFakeServer(nil, billettest.WithRawResult(raw))
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := c.Recall(context.Background(), "", memory.Query{Text: "q", Limit: 2})
	must.NoError(err)
	must.Len(got, 2, "want the first two hits: %+v", got)
	check.Equal("a", got[0].Reference.Digest)
	check.Equal("b", got[1].Reference.Digest)
}

func TestBillet_Recall_ParsesReplyShapes(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantIDs []string
		wantErr string
	}{
		{
			name:    "text block when no structuredContent",
			raw:     `{"content":[{"type":"text","text":"{\"records\":[{\"memory_id\":\"t1\",\"content\":\"x\",\"score\":1}]}"}]}`,
			wantIDs: []string{"t1"},
		},
		{
			name:    "structuredContent preferred over text",
			raw:     `{"content":[{"type":"text","text":"{\"records\":[{\"memory_id\":\"text\",\"content\":\"x\"}]}"}],"structuredContent":{"records":[{"memory_id":"structured","content":"y"}]}}`,
			wantIDs: []string{"structured"},
		},
		{
			name:    "text block used when structuredContent lacks records",
			raw:     `{"content":[{"type":"text","text":"{\"records\":[{\"memory_id\":\"t2\",\"content\":\"x\"}]}"}],"structuredContent":{"other":1}}`,
			wantIDs: []string{"t2"},
		},
		{
			name:    "empty records is a zero-hit success",
			raw:     `{"content":[],"structuredContent":{"records":[]}}`,
			wantIDs: []string{},
		},
		{
			name:    "null records is a zero-hit success",
			raw:     `{"content":[],"structuredContent":{"records":null}}`,
			wantIDs: []string{},
		},
		{
			name:    "prose is not a memory",
			raw:     `{"content":[{"type":"text","text":"I remember that Paris is in France."}]}`,
			wantErr: `reply carries no "records" document`,
		},
		{
			name:    "JSON without records is not a memory",
			raw:     `{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"t\"}]}"}],"structuredContent":{"results":[]}}`,
			wantErr: `reply carries no "records" document`,
		},
		{
			name:    "empty result",
			raw:     `{"content":[]}`,
			wantErr: `reply carries no "records" document`,
		},
		{
			name:    "records not an array",
			raw:     `{"content":[],"structuredContent":{"records":"nope"}}`,
			wantErr: "malformed reply",
		},
		{
			name:    "record without memory_id",
			raw:     `{"content":[],"structuredContent":{"records":[{"content":"x"}]}}`,
			wantErr: "record 0: record has no memory_id",
		},
		{
			name:    "memory_id with a path separator",
			raw:     `{"content":[],"structuredContent":{"records":[{"memory_id":"a/../b","content":"x"}]}}`,
			wantErr: "memory_id contains '/'",
		},
		{
			name:    "memory_id with a newline",
			raw:     `{"content":[],"structuredContent":{"records":[{"memory_id":"a\nRef: evil","content":"x"}]}}`,
			wantErr: `memory_id contains '\n'`,
		},
		{
			name:    "memory_id too long",
			raw:     `{"content":[],"structuredContent":{"records":[{"memory_id":"` + strings.Repeat("a", 257) + `","content":"x"}]}}`,
			wantErr: "memory_id exceeds 256 bytes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := billettest.NewFakeServer(nil, billettest.WithRawResult(tt.raw))
			defer fake.Close()

			c := newClient(t, fake.URL())
			got, err := c.Recall(context.Background(), "", memory.Query{Text: "q"})
			if tt.wantErr != "" {
				must.ErrorContains(err, tt.wantErr)
				must.True(strings.HasPrefix(err.Error(), "billet: search_memory: "), "error = %v, want the billet: search_memory: prefix", err)
				return
			}
			must.NoError(err)
			ids := make([]string, 0, len(got))
			for _, h := range got {
				ids = append(ids, h.Reference.Digest)
			}
			must.Equal(tt.wantIDs, ids)
			must.Equal(1, fake.ToolCallCount(), "tools/call count")
		})
	}
}

func TestBillet_Recall_BoundsHitText(t *testing.T) {
	// "€" is three bytes, so a byte bound that is not a multiple of three
	// must cut back to a rune boundary.
	content := strings.Repeat("€", 4000)
	tests := []struct {
		name        string
		maxHitBytes int
		wantMax     int
	}{
		{"default 8 KiB", 0, 8 << 10},
		{"custom bound", 100, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := billettest.NewFakeServer([]billettest.Record{{MemoryID: "big", Content: content}})
			defer fake.Close()

			c := newClient(t, fake.URL(), func(o *billet.Options) { o.MaxHitBytes = tt.maxHitBytes })
			got, err := c.Recall(context.Background(), "", memory.Query{Text: "q"})
			must.NoError(err)
			text := got[0].Memory.Text
			must.LessOrEqual(len(text), tt.wantMax, "bounded text is longer than the bound")
			must.GreaterOrEqual(len(text), tt.wantMax-2, "bounded text is more than a rune short of the bound")
			must.True(utf8.ValidString(text), "bounded text split a rune")
		})
	}
}

func TestBillet_Recall_SurfacesToolError(t *testing.T) {
	must := require.New(t)
	fake := billettest.NewFakeServer(nil, billettest.WithToolError("budget exceeded"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Recall(context.Background(), "", memory.Query{Text: "q"})
	must.EqualError(err, "billet: search_memory failed: budget exceeded")
}

func TestBillet_Recall_RejectsBlankQueryBeforeRequest(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fake := billettest.NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Recall(context.Background(), "", memory.Query{Text: "  "})
	must.Error(err, "Recall with a blank query should fail")
	check.Equal(0, fake.CallCount(), "validation must precede any request")
}

func TestBillet_Remember_SendsContentAndKind(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		wantKind string
	}{
		{"defaults to fact", nil, "fact"},
		{"explicit fact", map[string]string{"kind": "fact", "agent": "worker"}, "fact"},
		{"event", map[string]string{"kind": "event"}, "event"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := billettest.NewFakeServer(nil)
			defer fake.Close()

			c := newClient(t, fake.URL())
			ref, err := c.Remember(context.Background(), "team-a", memory.Memory{
				Text: "Objective\n\nAnswer",
				Meta: memory.ArtifactMeta{Name: "Objective", Labels: tt.labels},
			})
			must.NoError(err)
			must.Equal(memory.Reference{Namespace: "team-a", Digest: "mem-fake-1", Locator: "billet://memory/mem-fake-1"}, ref)
			must.Equal([]string{"Objective\n\nAnswer"}, fake.SavedContents(), "saved contents")
			call := fake.Requests()[2]
			must.Equal("save_memory", call.ToolName)
			must.Equal(tt.wantKind, call.Arguments["kind"])
			must.Len(call.Arguments, 2, "arguments are content and kind only")
		})
	}
}

func TestBillet_Remember_RefusesInvalidMemoryBeforeRequest(t *testing.T) {
	tests := []struct {
		name    string
		mem     memory.Memory
		wantErr string
	}{
		{"oversized content", memory.Memory{Text: strings.Repeat("x", billet.MaxContentBytes+1)}, "over Billet's 262144-byte limit"},
		{"blank content", memory.Memory{Text: " \n"}, "must not be empty"},
		{"unknown kind", memory.Memory{Text: "x", Meta: memory.ArtifactMeta{Labels: map[string]string{"kind": "opinion"}}}, `kind "opinion"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := billettest.NewFakeServer(nil)
			defer fake.Close()

			c := newClient(t, fake.URL())
			_, err := c.Remember(context.Background(), "", tt.mem)
			must.ErrorContains(err, tt.wantErr)
			must.Equal(0, fake.CallCount(), "the refusal must precede any request")
		})
	}
}

func TestBillet_Remember_SendsContentAtLimit(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	fake := billettest.NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Remember(context.Background(), "", memory.Memory{Text: strings.Repeat("x", billet.MaxContentBytes)})
	must.NoError(err, "Remember at exactly the limit")
	check.Len(fake.SavedContents(), 1, "saved memories")
}

func TestBillet_Remember_ParsesReplyShapes(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantID  string
		wantErr string
	}{
		{"text block reply", `{"content":[{"type":"text","text":"{\"memory_id\":\"m-1\",\"accepted\":true}"}]}`, "m-1", ""},
		{"not accepted", `{"content":[],"structuredContent":{"memory_id":"m-1","accepted":false}}`, "", "memory not accepted"},
		{"accepted missing", `{"content":[],"structuredContent":{"memory_id":"m-1"}}`, "", "memory not accepted"},
		{"memory_id missing", `{"content":[],"structuredContent":{"accepted":true}}`, "", `reply carries no "memory_id" document`},
		{"memory_id empty", `{"content":[],"structuredContent":{"memory_id":"","accepted":true}}`, "", "record has no memory_id"},
		{"prose", `{"content":[{"type":"text","text":"saved!"}]}`, "", `reply carries no "memory_id" document`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := billettest.NewFakeServer(nil, billettest.WithRawResult(tt.raw))
			defer fake.Close()

			c := newClient(t, fake.URL())
			ref, err := c.Remember(context.Background(), "", memory.Memory{Text: "x"})
			if tt.wantErr != "" {
				must.ErrorContains(err, tt.wantErr)
				return
			}
			must.NoError(err)
			must.Equal(tt.wantID, ref.Digest)
			must.Equal(billet.LocatorPrefix+tt.wantID, ref.Locator)
		})
	}
}

func TestBillet_Remember_SurfacesToolError(t *testing.T) {
	must := require.New(t)
	fake := billettest.NewFakeServer(nil, billettest.WithToolError("backend unavailable"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Remember(context.Background(), "", memory.Memory{Text: "x"})
	must.EqualError(err, "billet: save_memory failed: backend unavailable")
}

// TestBillet_ToolError_BoundsMessage: a tool error's text is cut to
// maxToolErrBytes on a rune boundary, whether or not the transport already bounded it.
func TestBillet_ToolError_BoundsMessage(t *testing.T) {
	huge := "backend unavailable " + strings.Repeat("é", 1<<20)
	direct := billet.ToolError(billet.SaveTool, mcpclient.ToolResult{Content: []mcpclient.ContentBlock{{Type: "text", Text: huge}}, IsError: true})

	fake := billettest.NewFakeServer(nil, billettest.WithToolError(huge))
	defer fake.Close()
	_, viaFake := newClient(t, fake.URL()).Recall(context.Background(), "", memory.Query{Text: "q"})

	for _, tt := range []struct {
		name string
		err  error
	}{
		{"direct", direct},
		{"through the transport", viaFake},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Error(tt.err)
			msg := tt.err.Error()
			must.LessOrEqual(len(msg), billet.MaxToolErrBytes+64, "error is longer than the bound plus prefix")
			must.True(utf8.ValidString(msg), "error is not valid UTF-8")
			must.True(strings.HasSuffix(msg, " [truncated]"), "error lacks the truncation marker")
			must.ErrorContains(tt.err, "backend unavailable")
		})
	}
}

func TestBillet_APIKey_SentAsHeaderOnlyNeverInErrors(t *testing.T) {
	t.Run("tool error echoing the key", func(t *testing.T) {
		must := require.New(t)
		fake := billettest.NewFakeServer(nil, billettest.WithToolError("proxy refused key "+testKey))
		defer fake.Close()

		c := newClient(t, fake.URL(), func(o *billet.Options) { o.APIKey = testKey })
		_, recallErr := c.Recall(context.Background(), "", memory.Query{Text: "q"})
		_, rememberErr := c.Remember(context.Background(), "", memory.Memory{Text: "x"})
		for _, err := range []error{recallErr, rememberErr} {
			must.Error(err)
			must.NotContains(err.Error(), testKey, "error leaked the key")
		}
		for i, req := range fake.Requests() {
			must.Equal("Bearer "+testKey, req.Authorization, "request[%d] Authorization", i)
		}
	})

	t.Run("HTTP error body echoing the key", func(t *testing.T) {
		must := require.New(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("forbidden for " + testKey))
		}))
		defer server.Close()

		c := newClient(t, server.URL, func(o *billet.Options) { o.APIKey = testKey })
		_, err := c.Recall(context.Background(), "", memory.Query{Text: "q"})
		must.Error(err, "Recall should fail on a 403")
		must.NotContains(err.Error(), testKey, "error leaked the key")
		must.True(strings.HasPrefix(err.Error(), "billet: search_memory: mcp: initialize failed: HTTP 403"), "error = %v, want the transport failure under the billet prefix", err)
	})

	t.Run("keyless sends no Authorization", func(t *testing.T) {
		must := require.New(t)
		fake := billettest.NewFakeServer(nil)
		defer fake.Close()

		c := newClient(t, fake.URL())
		_, err := c.Recall(context.Background(), "", memory.Query{Text: "q"})
		must.NoError(err)
		for i, req := range fake.Requests() {
			must.Empty(req.Authorization, "request[%d] carried Authorization on a keyless client", i)
		}
	})
}

func TestBillet_New_ValidatesEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  string
	}{
		{"cleartext non-loopback", "http://billet.internal/", "must be an absolute https:// URL"},
		{"userinfo", "https://user:Winter2026!@billet.internal/", "userinfo"},
		{"empty", "", "must not be empty"},
		{"query", "https://billet.internal/?token=Winter2026!", "query or fragment"},
		{"fragment", "https://billet.internal/#Winter2026!", "query or fragment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			_, err := billet.New(billet.Options{Endpoint: tt.endpoint})
			must.ErrorContains(err, tt.wantErr)
			must.True(strings.HasPrefix(err.Error(), "billet: mcp: "), "error = %v, want the billet: mcp: prefix", err)
			must.NotContains(err.Error(), "Winter2026!", "error echoed the credentials")
		})
	}
}

func TestBillet_Session_ReusedAndClosed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	// Recall and Remember share the client's one session, which Close ends.
	fake := billettest.NewFakeServer([]billettest.Record{{MemoryID: "m-1", Content: "x"}}, billettest.WithSessionID("sess-billet"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Recall(context.Background(), "", memory.Query{Text: "q"})
	must.NoError(err)
	_, err = c.Remember(context.Background(), "", memory.Memory{Text: "x"})
	must.NoError(err)
	check.Equal(1, fake.InitializeCount(), "initialize count")
	for range 2 {
		must.NoError(c.Close())
	}
	must.Equal(1, fake.DeleteCount(), "DELETE count")
	reqs := fake.Requests()
	end := reqs[len(reqs)-1]
	check.Equal(http.MethodDelete, end.HTTPMethod, "last request method")
	check.Equal("sess-billet", end.SessionID, "last request session")

	_, err = c.Recall(context.Background(), "", memory.Query{Text: "q"})
	must.ErrorIs(err, mcpclient.ErrClosed)
	check.True(strings.HasPrefix(err.Error(), "billet: search_memory: "), "error = %v, want the billet: search_memory: prefix", err)
}

func TestFakeUnknownToolIsRPCError(t *testing.T) {
	must := require.New(t)
	fake := billettest.NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := billet.MCPClient(c).CallTool(context.Background(), "forget_memory", nil)
	must.ErrorContains(err, `unknown tool "forget_memory"`)
}

func TestBillet_FirstLine_ReturnsTrimmedBoundedLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"blank only", " \n\t\n", ""},
		{"single line", "hello", "hello"},
		{"skips leading blank lines and trims", "\n\r\n  hello world \r\nnext", "hello world"},
		{"bounded to 120 runes", strings.Repeat("ü", 121), strings.Repeat("ü", 120)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got := billet.FirstLine(tt.in)
			must.Equal(tt.want, got, "FirstLine(%q)", tt.in)
		})
	}
}
