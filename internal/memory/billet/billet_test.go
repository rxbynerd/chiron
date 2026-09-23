package billet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rxbynerd/chiron/internal/memory"
)

const testKey = "sk-billet-0123456789abcdefABCDEF"

func newClient(t *testing.T, endpoint string, mutate ...func(*Options)) *Client {
	t.Helper()
	opts := Options{Endpoint: endpoint}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestRecallMapping(t *testing.T) {
	longTitle := strings.Repeat("é", 130)
	fake := NewFakeServer([]Record{
		{MemoryID: "mem-1", Content: "\n  Paris is the capital of France.  \nSecond line.", Score: 0.75, CreatedAt: "2026-09-01T10:00:00Z"},
		{MemoryID: "mem-2", Content: longTitle + "\nbody", Score: 0.5},
	})
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := c.Recall(context.Background(), "team-a", memory.Query{Text: "capital of France", Limit: 4})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d hits, want 2: %+v", len(got), got)
	}

	first := got[0]
	wantRef := memory.Reference{Namespace: "team-a", Digest: "mem-1", Locator: "billet://memory/mem-1"}
	if first.Reference != wantRef {
		t.Errorf("reference = %+v, want %+v", first.Reference, wantRef)
	}
	if first.Memory.Text != "\n  Paris is the capital of France.  \nSecond line." {
		t.Errorf("text = %q, want the content verbatim", first.Memory.Text)
	}
	if first.Memory.Meta.Name != "Paris is the capital of France." {
		t.Errorf("name = %q, want the first non-blank line, trimmed", first.Memory.Meta.Name)
	}
	if first.Memory.Meta.MediaType != "text/plain" {
		t.Errorf("media type = %q, want text/plain", first.Memory.Meta.MediaType)
	}
	if first.Memory.Meta.Labels["created_at"] != "2026-09-01T10:00:00Z" || len(first.Memory.Meta.Labels) != 1 {
		t.Errorf("labels = %+v, want only created_at", first.Memory.Meta.Labels)
	}
	if first.Score != 0.75 {
		t.Errorf("score = %v, want 0.75", first.Score)
	}

	second := got[1]
	if n := utf8.RuneCountInString(second.Memory.Meta.Name); n != 120 {
		t.Errorf("name runes = %d, want the first line bounded to 120", n)
	}
	if second.Memory.Meta.Labels != nil {
		t.Errorf("labels = %+v, want none without created_at", second.Memory.Meta.Labels)
	}

	call := fake.Requests()[2]
	if call.ToolName != "search_memory" {
		t.Errorf("tool = %q, want search_memory", call.ToolName)
	}
	if call.Arguments["query"] != "capital of France" || call.Arguments["limit"] != float64(4) {
		t.Errorf("arguments = %+v, want query and limit 4", call.Arguments)
	}
	if len(call.Arguments) != 2 {
		t.Errorf("arguments = %+v, want exactly query and limit", call.Arguments)
	}
}

func TestRecallLimitClamp(t *testing.T) {
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
			fake := NewFakeServer(nil)
			defer fake.Close()

			c := newClient(t, fake.URL(), func(o *Options) { o.DefaultLimit = tt.defaultLimit })
			if _, err := c.Recall(context.Background(), "", memory.Query{Text: "q", Limit: tt.queryLimit}); err != nil {
				t.Fatalf("Recall: %v", err)
			}
			if got := fake.Requests()[2].Arguments["limit"]; got != tt.want {
				t.Errorf("limit sent = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRecallKeepsAtMostLimitHits(t *testing.T) {
	// A server returning more records than asked is cut to the limit.
	raw := `{"content":[],"structuredContent":{"records":[` +
		`{"memory_id":"a","content":"1","score":0.9},` +
		`{"memory_id":"b","content":"2","score":0.8},` +
		`{"memory_id":"c","content":"3","score":0.7}]}}`
	fake := NewFakeServer(nil, WithRawResult(raw))
	defer fake.Close()

	c := newClient(t, fake.URL())
	got, err := c.Recall(context.Background(), "", memory.Query{Text: "q", Limit: 2})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) != 2 || got[0].Reference.Digest != "a" || got[1].Reference.Digest != "b" {
		t.Errorf("hits = %+v, want the first two", got)
	}
}

func TestRecallReplyShapes(t *testing.T) {
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
			fake := NewFakeServer(nil, WithRawResult(tt.raw))
			defer fake.Close()

			c := newClient(t, fake.URL())
			got, err := c.Recall(context.Background(), "", memory.Query{Text: "q"})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Recall error = %v, want one containing %q", err, tt.wantErr)
				}
				if !strings.HasPrefix(err.Error(), "billet: search_memory: ") {
					t.Errorf("error = %v, want the billet: search_memory: prefix", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Recall: %v", err)
			}
			ids := make([]string, 0, len(got))
			for _, h := range got {
				ids = append(ids, h.Reference.Digest)
			}
			if strings.Join(ids, ",") != strings.Join(tt.wantIDs, ",") {
				t.Errorf("ids = %v, want %v", ids, tt.wantIDs)
			}
			if fake.ToolCallCount() != 1 {
				t.Errorf("tools/call count = %d, want 1", fake.ToolCallCount())
			}
		})
	}
}

func TestRecallBoundsContent(t *testing.T) {
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
			fake := NewFakeServer([]Record{{MemoryID: "big", Content: content}})
			defer fake.Close()

			c := newClient(t, fake.URL(), func(o *Options) { o.MaxHitBytes = tt.maxHitBytes })
			got, err := c.Recall(context.Background(), "", memory.Query{Text: "q"})
			if err != nil {
				t.Fatalf("Recall: %v", err)
			}
			text := got[0].Memory.Text
			if len(text) > tt.wantMax || len(text) < tt.wantMax-2 {
				t.Errorf("text is %d bytes, want within a rune of %d", len(text), tt.wantMax)
			}
			if !utf8.ValidString(text) {
				t.Error("bounded text split a rune")
			}
		})
	}
}

func TestRecallToolError(t *testing.T) {
	fake := NewFakeServer(nil, WithToolError("budget exceeded"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Recall(context.Background(), "", memory.Query{Text: "q"})
	if err == nil || err.Error() != "billet: search_memory failed: budget exceeded" {
		t.Errorf("error = %v, want the tool's text surfaced", err)
	}
}

func TestRecallEmptyQueryRejected(t *testing.T) {
	fake := NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.Recall(context.Background(), "", memory.Query{Text: "  "}); err == nil {
		t.Fatal("Recall with a blank query should fail")
	}
	if fake.CallCount() != 0 {
		t.Errorf("call count = %d, want 0; validation must precede any request", fake.CallCount())
	}
}

func TestRememberSendsContentAndKind(t *testing.T) {
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
			fake := NewFakeServer(nil)
			defer fake.Close()

			c := newClient(t, fake.URL())
			ref, err := c.Remember(context.Background(), "team-a", memory.Memory{
				Text: "Objective\n\nAnswer",
				Meta: memory.ArtifactMeta{Name: "Objective", Labels: tt.labels},
			})
			if err != nil {
				t.Fatalf("Remember: %v", err)
			}
			want := memory.Reference{Namespace: "team-a", Digest: "mem-fake-1", Locator: "billet://memory/mem-fake-1"}
			if ref != want {
				t.Errorf("reference = %+v, want %+v", ref, want)
			}
			if saved := fake.SavedContents(); len(saved) != 1 || saved[0] != "Objective\n\nAnswer" {
				t.Errorf("saved contents = %q, want the memory text", saved)
			}
			call := fake.Requests()[2]
			if call.ToolName != "save_memory" {
				t.Errorf("tool = %q, want save_memory", call.ToolName)
			}
			if call.Arguments["kind"] != tt.wantKind || len(call.Arguments) != 2 {
				t.Errorf("arguments = %+v, want content and kind %q only", call.Arguments, tt.wantKind)
			}
		})
	}
}

func TestRememberRefusedBeforeRequest(t *testing.T) {
	tests := []struct {
		name    string
		mem     memory.Memory
		wantErr string
	}{
		{"oversized content", memory.Memory{Text: strings.Repeat("x", MaxContentBytes+1)}, "over Billet's 262144-byte limit"},
		{"blank content", memory.Memory{Text: " \n"}, "must not be empty"},
		{"unknown kind", memory.Memory{Text: "x", Meta: memory.ArtifactMeta{Labels: map[string]string{"kind": "opinion"}}}, `kind "opinion"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := NewFakeServer(nil)
			defer fake.Close()

			c := newClient(t, fake.URL())
			_, err := c.Remember(context.Background(), "", tt.mem)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Remember error = %v, want one containing %q", err, tt.wantErr)
			}
			if fake.CallCount() != 0 {
				t.Errorf("call count = %d, want 0; the refusal must precede any request", fake.CallCount())
			}
		})
	}
}

func TestRememberAtContentLimitSent(t *testing.T) {
	fake := NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := c.Remember(context.Background(), "", memory.Memory{Text: strings.Repeat("x", MaxContentBytes)}); err != nil {
		t.Fatalf("Remember at exactly the limit: %v", err)
	}
	if n := len(fake.SavedContents()); n != 1 {
		t.Errorf("saved %d memories, want 1", n)
	}
}

func TestRememberReplyShapes(t *testing.T) {
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
			fake := NewFakeServer(nil, WithRawResult(tt.raw))
			defer fake.Close()

			c := newClient(t, fake.URL())
			ref, err := c.Remember(context.Background(), "", memory.Memory{Text: "x"})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Remember error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Remember: %v", err)
			}
			if ref.Digest != tt.wantID || ref.Locator != LocatorPrefix+tt.wantID {
				t.Errorf("reference = %+v, want id %q", ref, tt.wantID)
			}
		})
	}
}

func TestRememberToolError(t *testing.T) {
	fake := NewFakeServer(nil, WithToolError("backend unavailable"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.Remember(context.Background(), "", memory.Memory{Text: "x"})
	if err == nil || err.Error() != "billet: save_memory failed: backend unavailable" {
		t.Errorf("error = %v, want the tool's text surfaced", err)
	}
}

func TestKeyHeaderOnlyAndNeverInErrors(t *testing.T) {
	t.Run("tool error echoing the key", func(t *testing.T) {
		fake := NewFakeServer(nil, WithToolError("proxy refused key "+testKey))
		defer fake.Close()

		c := newClient(t, fake.URL(), func(o *Options) { o.APIKey = testKey })
		_, recallErr := c.Recall(context.Background(), "", memory.Query{Text: "q"})
		_, rememberErr := c.Remember(context.Background(), "", memory.Memory{Text: "x"})
		for _, err := range []error{recallErr, rememberErr} {
			if err == nil {
				t.Fatal("want a tool error")
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("error leaked the key: %v", err)
			}
		}
		for i, req := range fake.Requests() {
			if req.Authorization != "Bearer "+testKey {
				t.Errorf("request[%d] Authorization = %q, want the bearer key header", i, req.Authorization)
			}
		}
	})

	t.Run("HTTP error body echoing the key", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("forbidden for " + testKey))
		}))
		defer server.Close()

		c := newClient(t, server.URL, func(o *Options) { o.APIKey = testKey })
		_, err := c.Recall(context.Background(), "", memory.Query{Text: "q"})
		if err == nil {
			t.Fatal("Recall should fail on a 403")
		}
		if strings.Contains(err.Error(), testKey) {
			t.Errorf("error leaked the key: %v", err)
		}
		if !strings.HasPrefix(err.Error(), "billet: search_memory: mcp: initialize failed: HTTP 403") {
			t.Errorf("error = %v, want the transport failure under the billet prefix", err)
		}
	})

	t.Run("keyless sends no Authorization", func(t *testing.T) {
		fake := NewFakeServer(nil)
		defer fake.Close()

		c := newClient(t, fake.URL())
		if _, err := c.Recall(context.Background(), "", memory.Query{Text: "q"}); err != nil {
			t.Fatalf("Recall: %v", err)
		}
		for i, req := range fake.Requests() {
			if req.Authorization != "" {
				t.Errorf("request[%d] carried Authorization %q on a keyless client", i, req.Authorization)
			}
		}
	})
}

func TestNewValidatesEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  string
	}{
		{"cleartext non-loopback", "http://billet.internal/", "must be an absolute https:// URL"},
		{"userinfo", "https://user:Winter2026!@billet.internal/", "userinfo"},
		{"empty", "", "must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(Options{Endpoint: tt.endpoint})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("New error = %v, want one containing %q", err, tt.wantErr)
			}
			if !strings.HasPrefix(err.Error(), "billet: mcp: ") {
				t.Errorf("error = %v, want the billet: mcp: prefix", err)
			}
			if strings.Contains(err.Error(), "Winter2026!") {
				t.Errorf("error echoed the credentials: %v", err)
			}
		})
	}
}

func TestFakeUnknownToolIsRPCError(t *testing.T) {
	fake := NewFakeServer(nil)
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := c.mcp.CallTool(context.Background(), "forget_memory", nil)
	if err == nil || !strings.Contains(err.Error(), `unknown tool "forget_memory"`) {
		t.Errorf("error = %v, want an unknown-tool rejection", err)
	}
}

func TestFirstLine(t *testing.T) {
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
			if got := firstLine(tt.in); got != tt.want {
				t.Errorf("firstLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
