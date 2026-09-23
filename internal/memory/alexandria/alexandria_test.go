package alexandria

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rxbynerd/chiron/internal/memory"
)

const (
	testKey          = "alx_0123456789abcdefABCDEF0123456789"
	testAccessID     = "access-id-0001.access"
	testAccessSecret = "cfsecret-9876543210fedcbaFEDCBA9876543210"
)

// newClient builds a Client pointed at endpoint, failing the test on a
// construction error.
func newClient(t *testing.T, endpoint string, mutate ...func(*Options)) *Client {
	t.Helper()
	opts := Options{Endpoint: endpoint, APIKey: testKey}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func strPtr(s string) *string { return &s }

// hits returns n numbered chunk results.
func hits(n int) []SearchResult {
	out := make([]SearchResult, n)
	for i := range out {
		out[i] = SearchResult{
			Ref:  "kb://source/00000000-0000-0000-0000-000000000000#L" + strconv.Itoa(i+1) + "-L" + strconv.Itoa(i+2),
			Unit: "chunk",
			ID:   "00000000-0000-0000-0000-000000000000",
		}
	}
	return out
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name    string
		opts    Options
		wantErr string
		absent  string
	}{
		{name: "https endpoint", opts: Options{Endpoint: "https://alexandria.example.com", APIKey: testKey}},
		{name: "https endpoint with path", opts: Options{Endpoint: "https://alexandria.example.com/api/", APIKey: testKey}},
		{name: "http loopback", opts: Options{Endpoint: "http://127.0.0.1:8787", APIKey: testKey}},
		{name: "http localhost", opts: Options{Endpoint: "http://localhost:8787", APIKey: testKey}},
		{name: "empty key", opts: Options{Endpoint: "https://alexandria.example.com"}, wantErr: "API key must not be empty"},
		{name: "empty endpoint", opts: Options{APIKey: testKey}, wantErr: "endpoint must not be empty"},
		{name: "relative endpoint", opts: Options{Endpoint: "alexandria.example.com", APIKey: testKey}, wantErr: "absolute https://"},
		{name: "http non-loopback", opts: Options{Endpoint: "http://alexandria.example.com", APIKey: testKey}, wantErr: "absolute https://"},
		{name: "ftp scheme", opts: Options{Endpoint: "ftp://alexandria.example.com", APIKey: testKey}, wantErr: "absolute https://"},
		{name: "userinfo", opts: Options{Endpoint: "https://user:hunter2@alexandria.example.com", APIKey: testKey}, wantErr: "userinfo", absent: "hunter2"},
		{name: "malformed with at sign withheld", opts: Options{Endpoint: "https//hunter2@alexandria", APIKey: testKey}, wantErr: "withheld", absent: "hunter2"},
		{name: "query", opts: Options{Endpoint: "https://alexandria.example.com/?space=a", APIKey: testKey}, wantErr: "query or fragment"},
		{name: "empty query", opts: Options{Endpoint: "https://alexandria.example.com/?", APIKey: testKey}, wantErr: "query or fragment"},
		{name: "fragment", opts: Options{Endpoint: "https://alexandria.example.com/#x", APIKey: testKey}, wantErr: "query or fragment"},
		{name: "access id without secret", opts: Options{Endpoint: "https://alexandria.example.com", APIKey: testKey, AccessClientID: testAccessID}, wantErr: "set together"},
		{name: "access secret without id", opts: Options{Endpoint: "https://alexandria.example.com", APIKey: testKey, AccessClientSecret: testAccessSecret}, wantErr: "set together"},
		{name: "default limit too high", opts: Options{Endpoint: "https://alexandria.example.com", APIKey: testKey, DefaultLimit: 21}, wantErr: "default limit"},
		{name: "default limit negative", opts: Options{Endpoint: "https://alexandria.example.com", APIKey: testKey, DefaultLimit: -1}, wantErr: "default limit"},
		{name: "max tokens too low", opts: Options{Endpoint: "https://alexandria.example.com", APIKey: testKey, MaxTokens: 99}, wantErr: "max tokens"},
		{name: "max tokens too high", opts: Options{Endpoint: "https://alexandria.example.com", APIKey: testKey, MaxTokens: 20001}, wantErr: "max tokens"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("New: unexpected error %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("New: want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
			if !strings.HasPrefix(err.Error(), "alexandria: ") {
				t.Errorf("error %q lacks the alexandria: prefix", err)
			}
			if tt.absent != "" && strings.Contains(err.Error(), tt.absent) {
				t.Errorf("error %q echoes %q", err, tt.absent)
			}
		})
	}
}

func TestNewDoesNotMutateCallerClient(t *testing.T) {
	caller := &http.Client{}
	newClient(t, "https://alexandria.example.com", func(o *Options) { o.HTTPClient = caller })
	if caller.CheckRedirect != nil {
		t.Error("New set CheckRedirect on the caller's client")
	}
}

func TestRecallMapsFragmentAndChunkHits(t *testing.T) {
	fake := NewFakeServer(SearchResponse{
		AppliedMode: "hybrid",
		Results: []SearchResult{
			{
				Ref: "kb://fragment/11111111-1111-1111-1111-111111111111", Unit: "fragment",
				ID: "11111111-1111-1111-1111-111111111111", Space: "team-notes", Title: "Deploy runbook",
				Kind: "howto", Snippet: "Run `just deploy`.", Score: 0.82, Stale: true,
				UpdatedAt: "2026-09-01T10:00:00Z",
			},
			{
				Ref: "kb://source/22222222-2222-2222-2222-222222222222#L10-L20", Unit: "chunk",
				ID: "22222222-2222-2222-2222-222222222222", Title: "architecture.md",
				Snippet: "The worker loop is bounded.", Score: 0.5,
			},
			{Ref: "", Unit: "chunk", Snippet: "no handle"},
			{
				Ref: "kb://fragment/weird", Unit: "fragment", ID: "a/../b?c", Space: "team-notes",
				Kind: "fact", Snippet: "escaped", Score: 0.1,
			},
			{
				Ref: "kb://fragment/no-id", Unit: "fragment", Space: "team-notes",
				Kind: "fact", Snippet: "no id", Score: 0.05,
			},
		},
	})
	defer fake.Close()
	c := newClient(t, fake.URL())

	got, err := c.Recall(context.Background(), "team-notes", memory.Query{Text: "how do we deploy?", Limit: 10})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	want := []memory.Recalled{
		{
			Reference: memory.Reference{
				Namespace: "team-notes",
				Digest:    "kb://fragment/11111111-1111-1111-1111-111111111111",
				Locator:   fake.URL() + "/f/11111111-1111-1111-1111-111111111111",
			},
			Memory: memory.Memory{
				Text: "Run `just deploy`.",
				Meta: memory.ArtifactMeta{
					Name:      "Deploy runbook",
					MediaType: "text/markdown",
					Labels: map[string]string{
						"kind": "howto", "unit": "fragment", "space": "team-notes",
						"stale": "true", "updated_at": "2026-09-01T10:00:00Z",
					},
				},
			},
			Score: 0.82,
		},
		{
			Reference: memory.Reference{
				Namespace: "team-notes",
				Digest:    "kb://source/22222222-2222-2222-2222-222222222222#L10-L20",
				Locator:   "kb://source/22222222-2222-2222-2222-222222222222#L10-L20",
			},
			Memory: memory.Memory{
				Text: "The worker loop is bounded.",
				Meta: memory.ArtifactMeta{
					Name:      "architecture.md",
					MediaType: "text/markdown",
					Labels:    map[string]string{"unit": "chunk", "space": "team-notes"},
				},
			},
			Score: 0.5,
		},
		{
			Reference: memory.Reference{
				Namespace: "team-notes",
				Digest:    "kb://fragment/weird",
				Locator:   fake.URL() + "/f/a%2F..%2Fb%3Fc",
			},
			Memory: memory.Memory{
				Text: "escaped",
				Meta: memory.ArtifactMeta{
					MediaType: "text/markdown",
					Labels:    map[string]string{"kind": "fact", "unit": "fragment", "space": "team-notes"},
				},
			},
			Score: 0.1,
		},
		{
			Reference: memory.Reference{
				Namespace: "team-notes",
				Digest:    "kb://fragment/no-id",
				Locator:   "kb://fragment/no-id",
			},
			Memory: memory.Memory{
				Text: "no id",
				Meta: memory.ArtifactMeta{
					MediaType: "text/markdown",
					Labels:    map[string]string{"kind": "fact", "unit": "fragment", "space": "team-notes"},
				},
			},
			Score: 0.05,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Recall mapping mismatch\n got: %+v\nwant: %+v", got, want)
	}

	reqs := fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodGet || r.Path != "/v1/search" {
		t.Errorf("request = %s %s, want GET /v1/search", r.Method, r.Path)
	}
	if r.Accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", r.Accept)
	}
	wantQuery := map[string]string{"q": "how do we deploy?", "mode": "hybrid", "max_tokens": "4000", "space": "team-notes"}
	if len(r.Query) != len(wantQuery) {
		t.Errorf("query = %v, want exactly %v", r.Query, wantQuery)
	}
	for k, v := range wantQuery {
		if got := r.Query.Get(k); got != v {
			t.Errorf("query %s = %q, want %q", k, got, v)
		}
	}
}

func TestRecallNamespaceFallsBackWhenResultHasNoSpace(t *testing.T) {
	fake := NewFakeServer(SearchResponse{Results: hits(1)})
	defer fake.Close()
	c := newClient(t, fake.URL())

	tests := []struct {
		name      string
		ns        memory.Namespace
		wantNS    memory.Namespace
		wantLabel bool
	}{
		{name: "namespace passed", ns: "research", wantNS: "research", wantLabel: true},
		{name: "no namespace", ns: "", wantNS: "", wantLabel: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.Recall(context.Background(), tt.ns, memory.Query{Text: "x"})
			if err != nil {
				t.Fatalf("Recall: %v", err)
			}
			if got[0].Reference.Namespace != tt.wantNS {
				t.Errorf("Namespace = %q, want %q", got[0].Reference.Namespace, tt.wantNS)
			}
			if _, ok := got[0].Memory.Meta.Labels["space"]; ok != tt.wantLabel {
				t.Errorf("space label present = %v, want %v", ok, tt.wantLabel)
			}
		})
	}
}

func TestRecallLimit(t *testing.T) {
	fake := NewFakeServer(SearchResponse{Results: hits(25)})
	defer fake.Close()

	tests := []struct {
		name         string
		defaultLimit int
		limit        int
		want         int
	}{
		{name: "zero uses package default", limit: 0, want: 5},
		{name: "negative uses package default", limit: -3, want: 5},
		{name: "zero uses configured default", defaultLimit: 3, limit: 0, want: 3},
		{name: "explicit limit", limit: 2, want: 2},
		{name: "explicit limit overrides default", defaultLimit: 3, limit: 7, want: 7},
		{name: "clamped to twenty", limit: 50, want: 20},
		{name: "configured maximum", defaultLimit: 20, want: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, fake.URL(), func(o *Options) { o.DefaultLimit = tt.defaultLimit })
			got, err := c.Recall(context.Background(), "", memory.Query{Text: "x", Limit: tt.limit})
			if err != nil {
				t.Fatalf("Recall: %v", err)
			}
			if len(got) != tt.want {
				t.Errorf("hits = %d, want %d", len(got), tt.want)
			}
		})
	}
}

func TestRecallFewerResultsThanLimit(t *testing.T) {
	fake := NewFakeServer(SearchResponse{Results: hits(2)})
	defer fake.Close()
	c := newClient(t, fake.URL())

	got, err := c.Recall(context.Background(), "", memory.Query{Text: "x", Limit: 10})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("hits = %d, want 2", len(got))
	}
}

func TestRecallSpaceValidation(t *testing.T) {
	tests := []struct {
		name      string
		ns        memory.Namespace
		wantErr   string
		wantSpace string
	}{
		{name: "valid slug", ns: "team-notes", wantSpace: "team-notes"},
		{name: "single character", ns: "a", wantSpace: "a"},
		{name: "digits", ns: "2026-q3", wantSpace: "2026-q3"},
		{name: "sixty-four characters", ns: memory.Namespace(strings.Repeat("a", 64)), wantSpace: strings.Repeat("a", 64)},
		{name: "empty searches all spaces", ns: ""},
		{name: "uppercase", ns: "Team", wantErr: "not a valid space slug"},
		{name: "leading hyphen", ns: "-team", wantErr: "not a valid space slug"},
		{name: "trailing hyphen", ns: "team-", wantErr: "not a valid space slug"},
		{name: "underscore", ns: "team_notes", wantErr: "not a valid space slug"},
		{name: "space character", ns: "team notes", wantErr: "not a valid space slug"},
		{name: "query injection", ns: "a&space=b", wantErr: "not a valid space slug"},
		{name: "sixty-five characters", ns: memory.Namespace(strings.Repeat("a", 65)), wantErr: "exceeds the 64-character"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := NewFakeServer(SearchResponse{})
			defer fake.Close()
			c := newClient(t, fake.URL())

			_, err := c.Recall(context.Background(), tt.ns, memory.Query{Text: "x"})
			reqs := fake.Requests()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				if len(reqs) != 0 {
					t.Errorf("requests = %d, want none before validation passes", len(reqs))
				}
				return
			}
			if err != nil {
				t.Fatalf("Recall: %v", err)
			}
			if len(reqs) != 1 {
				t.Fatalf("requests = %d, want 1", len(reqs))
			}
			if _, present := reqs[0].Query["space"]; present != (tt.wantSpace != "") {
				t.Errorf("space param present = %v, want %v", present, tt.wantSpace != "")
			}
			if got := reqs[0].Query.Get("space"); got != tt.wantSpace {
				t.Errorf("space = %q, want %q", got, tt.wantSpace)
			}
		})
	}
}

func TestRecallHeaders(t *testing.T) {
	tests := []struct {
		name       string
		accessID   string
		accessSec  string
		maxTokens  int
		wantTokens string
	}{
		{name: "bearer only", wantTokens: "4000"},
		{name: "bearer and Access service token", accessID: testAccessID, accessSec: testAccessSecret, maxTokens: 1500, wantTokens: "1500"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := NewFakeServer(SearchResponse{})
			defer fake.Close()
			c := newClient(t, fake.URL(), func(o *Options) {
				o.AccessClientID, o.AccessClientSecret, o.MaxTokens = tt.accessID, tt.accessSec, tt.maxTokens
			})

			if _, err := c.Recall(context.Background(), "", memory.Query{Text: "x"}); err != nil {
				t.Fatalf("Recall: %v", err)
			}
			r := fake.Requests()[0]
			if r.Authorization != "Bearer "+testKey {
				t.Errorf("Authorization = %q, want bearer token", r.Authorization)
			}
			if r.AccessClientID != tt.accessID || r.AccessClientSecret != tt.accessSec {
				t.Errorf("Access headers = (%q, %q), want (%q, %q)", r.AccessClientID, r.AccessClientSecret, tt.accessID, tt.accessSec)
			}
			if got := r.Query.Get("max_tokens"); got != tt.wantTokens {
				t.Errorf("max_tokens = %q, want %q", got, tt.wantTokens)
			}
			for k, vs := range r.Query {
				for _, v := range vs {
					if strings.Contains(v, testKey) {
						t.Errorf("query param %s carries the API key", k)
					}
				}
			}
		})
	}
}

func TestRecallRefusesRedirect(t *testing.T) {
	var targetHits atomic.Int32
	var targetAuth atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		targetAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer target.Close()

	tests := []struct {
		name     string
		location func() string
	}{
		{name: "cross-origin", location: func() string { return target.URL + "/v1/search" }},
		{name: "same-origin", location: func() string { return "/v1/search?moved=1" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := NewFakeServer(SearchResponse{}, WithRedirect(tt.location()))
			defer fake.Close()
			c := newClient(t, fake.URL(), func(o *Options) {
				o.AccessClientID, o.AccessClientSecret = testAccessID, testAccessSecret
			})

			_, err := c.Recall(context.Background(), "", memory.Query{Text: "x"})
			if err == nil || !strings.Contains(err.Error(), "redirect") {
				t.Fatalf("err = %v, want a redirect refusal", err)
			}
			if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), testAccessSecret) {
				t.Errorf("error echoes a credential: %v", err)
			}
			if n := len(fake.Requests()); n != 1 {
				t.Errorf("fake requests = %d, want 1 (redirect not followed)", n)
			}
		})
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("redirect target received %d requests (Authorization %v), want 0", n, targetAuth.Load())
	}
}

func TestRecallErrorStatuses(t *testing.T) {
	echo := `{"error":"invalid token ` + testKey + `"}`
	tests := []struct {
		name    string
		opts    []FakeOption
		want    []string
		absent  []string
		maxLen  int
		keyBody bool
	}{
		{
			name: "429 with Retry-After",
			opts: []FakeOption{WithStatus(http.StatusTooManyRequests, `{"error":"slow down"}`), WithHeader("Retry-After", "30")},
			want: []string{"HTTP 429", "Retry-After: 30"},
		},
		{
			name: "429 without Retry-After",
			opts: []FakeOption{WithStatus(http.StatusTooManyRequests, "")},
			want: []string{"HTTP 429"}, absent: []string{"Retry-After"},
		},
		{
			name: "429 Retry-After control characters stripped",
			opts: []FakeOption{WithStatus(http.StatusTooManyRequests, ""), WithHeader("Retry-After", "30\tseconds")},
			want: []string{"Retry-After: 30seconds"},
		},
		{
			name: "401 never echoes the token",
			opts: []FakeOption{WithStatus(http.StatusUnauthorized, echo)},
			want: []string{"HTTP 401"}, absent: []string{testKey, "invalid token", "REDACTED"},
		},
		{
			name: "403 never echoes the token",
			opts: []FakeOption{WithStatus(http.StatusForbidden, echo)},
			want: []string{"HTTP 403"}, absent: []string{testKey, "invalid token"},
		},
		{
			name: "500 carries a scrubbed excerpt",
			opts: []FakeOption{WithStatus(http.StatusInternalServerError, echo)},
			want: []string{"HTTP 500", "invalid token", "[REDACTED:alexandria-api-key]"}, absent: []string{testKey},
		},
		{
			name: "500 Access secret scrubbed",
			opts: []FakeOption{WithStatus(http.StatusInternalServerError, "bad secret "+testAccessSecret)},
			want: []string{"[REDACTED:alexandria-access-secret]"}, absent: []string{testAccessSecret},
		},
		{
			name: "502 excerpt bounded",
			opts: []FakeOption{WithStatus(http.StatusBadGateway, strings.Repeat("upstream down ", 10000))},
			want: []string{"HTTP 502", "upstream down", "..."}, maxLen: 700,
		},
		{
			name: "500 without body",
			opts: []FakeOption{WithStatus(http.StatusInternalServerError, "")},
			want: []string{"HTTP 500"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := NewFakeServer(SearchResponse{}, tt.opts...)
			defer fake.Close()
			c := newClient(t, fake.URL(), func(o *Options) {
				o.AccessClientID, o.AccessClientSecret = testAccessID, testAccessSecret
			})

			_, err := c.Recall(context.Background(), "", memory.Query{Text: "x"})
			if err == nil {
				t.Fatal("Recall: want error, got nil")
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "alexandria: ") {
				t.Errorf("error %q lacks the alexandria: prefix", msg)
			}
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error %q does not contain %q", msg, w)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(msg, a) {
					t.Errorf("error %q contains %q", msg, a)
				}
			}
			if tt.maxLen > 0 && len(msg) > tt.maxLen {
				t.Errorf("error length %d exceeds %d", len(msg), tt.maxLen)
			}
			if n := len(fake.Requests()); n != 1 {
				t.Errorf("requests = %d, want exactly 1 (no retry)", n)
			}
		})
	}
}

func TestRecallBodyBound(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		bound   int64
		wantErr bool
	}{
		{name: "over the bound fails", size: 4096, bound: 1024, wantErr: true},
		{name: "under the bound succeeds", size: 100, bound: 1024},
		{name: "default bound refuses over one MiB", size: 1<<20 + 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := NewFakeServer(SearchResponse{}, WithOversizedBody(tt.size))
			defer fake.Close()
			c := newClient(t, fake.URL(), func(o *Options) { o.MaxBodyBytes = tt.bound })

			_, err := c.Recall(context.Background(), "", memory.Query{Text: "x"})
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "exceeds") {
					t.Fatalf("err = %v, want a body-bound failure", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Recall: %v", err)
			}
		})
	}
}

func TestRecallDegradedLabel(t *testing.T) {
	tests := []struct {
		name     string
		degraded *string
		want     string
	}{
		{name: "embedding unavailable", degraded: strPtr("embedding_unavailable"), want: "embedding_unavailable"},
		{name: "rerank unavailable", degraded: strPtr("rerank_unavailable"), want: "rerank_unavailable"},
		{name: "not degraded", degraded: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := NewFakeServer(SearchResponse{Results: hits(3), Degraded: tt.degraded})
			defer fake.Close()
			c := newClient(t, fake.URL())

			got, err := c.Recall(context.Background(), "", memory.Query{Text: "x"})
			if err != nil {
				t.Fatalf("Recall: %v", err)
			}
			if len(got) != 3 {
				t.Fatalf("hits = %d, want 3", len(got))
			}
			for i, h := range got {
				v, ok := h.Memory.Meta.Labels["degraded"]
				if ok != (tt.want != "") || v != tt.want {
					t.Errorf("hit %d degraded label = (%q, %v), want %q", i, v, ok, tt.want)
				}
			}
		})
	}
}

func TestRecallStrictDecode(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantErr  string
		wantHits int
	}{
		{name: "array", body: `[{"ref":"kb://fragment/x"}]`, wantErr: "not a JSON object"},
		{name: "null", body: `null`, wantErr: "not a JSON object"},
		{name: "string", body: `"results"`, wantErr: "not a JSON object"},
		{name: "not JSON", body: `<html>gateway</html>`, wantErr: "not a JSON object"},
		{name: "whitespace only", body: "  \n", wantErr: "not a JSON object"},
		{name: "object without results", body: `{"error":"nope"}`, wantErr: "no results array"},
		{name: "null results", body: `{"results":null}`, wantErr: "no results array"},
		{name: "results not an array", body: `{"results":{}}`, wantErr: "decoding response"},
		{name: "trailing data", body: `{"results":[]} {}`, wantErr: "decoding response"},
		{name: "unknown fields ignored", body: `{"results":[{"ref":"kb://fragment/a","unit":"fragment","id":"a","reranked":true,"contradicted_by":[{"ref":"kb://fragment/b"}],"citations":[{"ref":"kb://source/c","verified":true}]}],"next_cursor":null,"usage":{"tokens":10},"space_context":{"slug":"a"}}`, wantHits: 1},
		{name: "null optional fields", body: `{"results":[{"ref":"kb://fragment/a","unit":"fragment","id":"a","title":null,"updated_at":null,"stale":null,"score":null}],"degraded":null}`, wantHits: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := NewFakeServer(SearchResponse{}, WithRawBody(tt.body))
			defer fake.Close()
			c := newClient(t, fake.URL())

			got, err := c.Recall(context.Background(), "", memory.Query{Text: "x"})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Recall: %v", err)
			}
			if len(got) != tt.wantHits {
				t.Errorf("hits = %d, want %d", len(got), tt.wantHits)
			}
		})
	}
}

func TestRecallRejectsEmptyQuery(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "empty", text: ""},
		{name: "whitespace", text: " \t\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := NewFakeServer(SearchResponse{})
			defer fake.Close()
			c := newClient(t, fake.URL())

			_, err := c.Recall(context.Background(), "", memory.Query{Text: tt.text})
			if err == nil || !strings.Contains(err.Error(), "query text must not be empty") {
				t.Fatalf("err = %v, want an empty-query error", err)
			}
			if n := len(fake.Requests()); n != 0 {
				t.Errorf("requests = %d, want none", n)
			}
		})
	}
}

func TestRecallTransportErrorScrubbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := srv.URL
	srv.Close()
	c := newClient(t, endpoint)

	_, err := c.Recall(context.Background(), "", memory.Query{Text: testKey})
	if err == nil {
		t.Fatal("Recall: want a transport error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "alexandria: GET /v1/search: ") {
		t.Errorf("error %q lacks the transport prefix", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("transport error echoes the key: %v", err)
	}
}
