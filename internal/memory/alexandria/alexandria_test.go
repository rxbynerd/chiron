package alexandria_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/memory/alexandria"
	"github.com/rxbynerd/chiron/internal/memory/alexandria/alexandriatest"
)

const (
	testKey          = "alx_0123456789abcdefABCDEF0123456789"
	testAccessID     = "access-id-0001.access"
	testAccessSecret = "cfsecret-9876543210fedcbaFEDCBA9876543210"
)

// newClient builds a Client pointed at endpoint, failing the test on a
// construction error.
func newClient(t *testing.T, endpoint string, mutate ...func(*alexandria.Options)) *alexandria.Client {
	t.Helper()
	must := require.New(t)
	opts := alexandria.Options{Endpoint: endpoint, APIKey: testKey}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := alexandria.New(opts)
	must.NoError(err)
	return c
}

func strPtr(s string) *string { return &s }

// hits returns n numbered chunk results.
func hits(n int) []alexandria.SearchResult {
	out := make([]alexandria.SearchResult, n)
	for i := range out {
		out[i] = alexandria.SearchResult{
			Ref:  "kb://source/00000000-0000-0000-0000-000000000000#L" + strconv.Itoa(i+1) + "-L" + strconv.Itoa(i+2),
			Unit: "chunk",
			ID:   "00000000-0000-0000-0000-000000000000",
		}
	}
	return out
}

func TestNew_ValidatesOptions(t *testing.T) {
	tests := []struct {
		name    string
		opts    alexandria.Options
		wantErr string
		absent  string
	}{
		{name: "https endpoint", opts: alexandria.Options{Endpoint: "https://alexandria.example.com", APIKey: testKey}},
		{name: "https endpoint with path", opts: alexandria.Options{Endpoint: "https://alexandria.example.com/api/", APIKey: testKey}},
		{name: "http loopback", opts: alexandria.Options{Endpoint: "http://127.0.0.1:8787", APIKey: testKey}},
		{name: "http localhost", opts: alexandria.Options{Endpoint: "http://localhost:8787", APIKey: testKey}},
		{name: "empty key", opts: alexandria.Options{Endpoint: "https://alexandria.example.com"}, wantErr: "API key must not be empty"},
		{name: "empty endpoint", opts: alexandria.Options{APIKey: testKey}, wantErr: "endpoint must not be empty"},
		{name: "relative endpoint", opts: alexandria.Options{Endpoint: "alexandria.example.com", APIKey: testKey}, wantErr: "absolute https://"},
		{name: "http non-loopback", opts: alexandria.Options{Endpoint: "http://alexandria.example.com", APIKey: testKey}, wantErr: "absolute https://"},
		{name: "ftp scheme", opts: alexandria.Options{Endpoint: "ftp://alexandria.example.com", APIKey: testKey}, wantErr: "absolute https://"},
		{name: "userinfo", opts: alexandria.Options{Endpoint: "https://user:hunter2@alexandria.example.com", APIKey: testKey}, wantErr: "userinfo", absent: "hunter2"},
		{name: "malformed with at sign withheld", opts: alexandria.Options{Endpoint: "https//hunter2@alexandria", APIKey: testKey}, wantErr: "withheld", absent: "hunter2"},
		{name: "query", opts: alexandria.Options{Endpoint: "https://alexandria.example.com/?space=a", APIKey: testKey}, wantErr: "query or fragment"},
		{name: "empty query", opts: alexandria.Options{Endpoint: "https://alexandria.example.com/?", APIKey: testKey}, wantErr: "query or fragment"},
		{name: "fragment", opts: alexandria.Options{Endpoint: "https://alexandria.example.com/#x", APIKey: testKey}, wantErr: "query or fragment"},
		{name: "access id without secret", opts: alexandria.Options{Endpoint: "https://alexandria.example.com", APIKey: testKey, AccessClientID: testAccessID}, wantErr: "set together"},
		{name: "access secret without id", opts: alexandria.Options{Endpoint: "https://alexandria.example.com", APIKey: testKey, AccessClientSecret: testAccessSecret}, wantErr: "set together"},
		{name: "default limit too high", opts: alexandria.Options{Endpoint: "https://alexandria.example.com", APIKey: testKey, DefaultLimit: 21}, wantErr: "default limit"},
		{name: "default limit negative", opts: alexandria.Options{Endpoint: "https://alexandria.example.com", APIKey: testKey, DefaultLimit: -1}, wantErr: "default limit"},
		{name: "max tokens too low", opts: alexandria.Options{Endpoint: "https://alexandria.example.com", APIKey: testKey, MaxTokens: 99}, wantErr: "max tokens"},
		{name: "max tokens too high", opts: alexandria.Options{Endpoint: "https://alexandria.example.com", APIKey: testKey, MaxTokens: 20001}, wantErr: "max tokens"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			_, err := alexandria.New(tt.opts)
			if tt.wantErr == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tt.wantErr)
			must.True(strings.HasPrefix(err.Error(), "alexandria: "), "error %q lacks the alexandria: prefix", err.Error())
			if tt.absent != "" {
				must.NotContains(err.Error(), tt.absent, "error echoes the secret")
			}
		})
	}
}

func TestNew_DoesNotMutateCallerHTTPClient(t *testing.T) {
	must := require.New(t)
	caller := &http.Client{}
	newClient(t, "https://alexandria.example.com", func(o *alexandria.Options) { o.HTTPClient = caller })
	must.Nil(caller.CheckRedirect, "New must not set CheckRedirect on the caller's client")
}

func TestRecall_MapsFragmentAndChunkHits(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{
		AppliedMode: "hybrid",
		Results: []alexandria.SearchResult{
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
				Ref: "kb://fragment/33333333-3333-3333-3333-333333333333", Unit: "fragment", ID: "a/../b?c",
				Space: "team-notes", Kind: "fact", Snippet: "id ignored", Score: 0.1,
			},
		},
	})
	defer fake.Close()
	c := newClient(t, fake.URL())

	got, err := c.Recall(context.Background(), "team-notes", memory.Query{Text: "how do we deploy?", Limit: 10})
	must.NoError(err)

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
				Digest:    "kb://fragment/33333333-3333-3333-3333-333333333333",
				Locator:   fake.URL() + "/f/33333333-3333-3333-3333-333333333333",
			},
			Memory: memory.Memory{
				Text: "id ignored",
				Meta: memory.ArtifactMeta{
					MediaType: "text/markdown",
					Labels:    map[string]string{"kind": "fact", "unit": "fragment", "space": "team-notes"},
				},
			},
			Score: 0.1,
		},
	}
	check.Equal(want, got)

	reqs := fake.Requests()
	must.Len(reqs, 1)
	r := reqs[0]
	check.Equal(http.MethodGet, r.Method)
	check.Equal("/v1/search", r.Path)
	check.Equal("application/json", r.Accept, "Accept header")
	wantQuery := map[string]string{"q": "how do we deploy?", "mode": "hybrid", "max_tokens": "4000", "space": "team-notes"}
	check.Len(r.Query, len(wantQuery), "query %v, want exactly %v", r.Query, wantQuery)
	for k, v := range wantQuery {
		check.Equal(v, r.Query.Get(k), "query param %s", k)
	}
}

// TestRecall_DropsRefsOutsideAlexandriaGrammar: only refs in Alexandria's
// grammar become hits; any other ref, a hostile URL included, is dropped before
// the limit applies, so it never reaches a Recalled and never displaces a valid
// hit.
func TestRecall_DropsRefsOutsideAlexandriaGrammar(t *testing.T) {
	const id = "0f2e4d6c-1111-2222-3333-444455556666"
	for _, tt := range []struct {
		name string
		ref  string
		ok   bool
	}{
		{"fragment", "kb://fragment/" + id, true},
		{"source", "kb://source/" + id, true},
		{"source lines", "kb://source/" + id + "#L10-L20", true},
		{"source characters by length", "kb://source/" + id + "#C120+40", true},
		{"source character range", "kb://source/" + id + "#C120-C160", true},
		{"https URL", "https://attacker.example/phish?x=1", false},
		{"billet locator", "billet://memory/m1", false},
		{"fragment not a UUID", "kb://fragment/weird", false},
		{"fragment with a path", "kb://fragment/" + id + "/../x", false},
		{"fragment with a locator", "kb://fragment/" + id + "#L1-L2", false},
		{"uppercase UUID", "kb://fragment/0F2E4D6C-1111-2222-3333-444455556666", false},
		{"source with a query", "kb://source/" + id + "?x=1", false},
		{"source with a free-form locator", "kb://source/" + id + "#<img>", false},
		{"source with half a line range", "kb://source/" + id + "#L10", false},
		{"unknown kind", "kb://space/" + id, false},
		{"leading space", " kb://fragment/" + id, false},
		{"trailing newline", "kb://fragment/" + id + "\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			const valid = "kb://source/22222222-2222-2222-2222-222222222222#L1-L2"
			fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{Results: []alexandria.SearchResult{
				{Ref: tt.ref, Unit: "chunk", ID: id, Title: "candidate", Snippet: "candidate"},
				{Ref: valid, Unit: "chunk", Title: "valid"},
			}})
			defer fake.Close()

			got, err := newClient(t, fake.URL()).Recall(context.Background(), "", memory.Query{Text: "x", Limit: 1})
			must.NoError(err)
			must.Len(got, 1)
			accepted := got[0].Reference.Digest == tt.ref
			must.Equal(tt.ok, accepted, "ref %q accepted, reference %+v", tt.ref, got[0].Reference)
			if !tt.ok {
				must.Equal(valid, got[0].Reference.Locator, "the rejected hit displaced the valid one")
			}
		})
	}
}

// TestRecall_BoundsTitleAndSnippet: a hit's title is cut to 200 runes and its
// snippet to 4 KiB, both on rune boundaries (see maxTitleRunes and
// maxSnippetBytes in internal/memory/alexandria/recall.go).
func TestRecall_BoundsTitleAndSnippet(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const (
		maxTitleRunes   = 200
		maxSnippetBytes = 4 << 10
		truncatedMarker = " [truncated]"
	)
	fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{Results: []alexandria.SearchResult{{
		Ref:     "kb://source/22222222-2222-2222-2222-222222222222",
		Unit:    "chunk",
		Title:   strings.Repeat("ü", 10<<10),
		Snippet: strings.Repeat("é", 64<<10),
	}}})
	defer fake.Close()

	got, err := newClient(t, fake.URL()).Recall(context.Background(), "", memory.Query{Text: "x"})
	must.NoError(err)
	must.Len(got, 1)
	m := got[0].Memory
	check.Equal(maxTitleRunes, utf8.RuneCountInString(m.Meta.Name), "title runes")
	check.LessOrEqual(len(m.Text), maxSnippetBytes, "snippet bytes")
	check.True(utf8.ValidString(m.Text), "snippet is valid UTF-8")
	check.True(strings.HasSuffix(m.Text, truncatedMarker), "snippet ends with the truncation marker")
}

func TestRecall_NamespaceFallsBackWhenResultHasNoSpace(t *testing.T) {
	fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{Results: hits(1)})
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
			must := require.New(t)
			got, err := c.Recall(context.Background(), tt.ns, memory.Query{Text: "x"})
			must.NoError(err)
			must.Equal(tt.wantNS, got[0].Reference.Namespace)
			_, hasSpace := got[0].Memory.Meta.Labels["space"]
			must.Equal(tt.wantLabel, hasSpace, "space label present")
		})
	}
}

func TestRecall_ClampsAndDefaultsLimit(t *testing.T) {
	fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{Results: hits(25)})
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
			must := require.New(t)
			c := newClient(t, fake.URL(), func(o *alexandria.Options) { o.DefaultLimit = tt.defaultLimit })
			got, err := c.Recall(context.Background(), "", memory.Query{Text: "x", Limit: tt.limit})
			must.NoError(err)
			must.Len(got, tt.want)
		})
	}
}

func TestRecall_ReturnsFewerHitsThanLimit(t *testing.T) {
	must := require.New(t)
	fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{Results: hits(2)})
	defer fake.Close()
	c := newClient(t, fake.URL())

	got, err := c.Recall(context.Background(), "", memory.Query{Text: "x", Limit: 10})
	must.NoError(err)
	must.Len(got, 2)
}

func TestRecall_ValidatesSpaceSlug(t *testing.T) {
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
			must := require.New(t)
			fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{})
			defer fake.Close()
			c := newClient(t, fake.URL())

			_, err := c.Recall(context.Background(), tt.ns, memory.Query{Text: "x"})
			reqs := fake.Requests()
			if tt.wantErr != "" {
				must.ErrorContains(err, tt.wantErr)
				must.Empty(reqs, "no request before validation passes")
				return
			}
			must.NoError(err)
			must.Len(reqs, 1)
			_, present := reqs[0].Query["space"]
			must.Equal(tt.wantSpace != "", present, "space param present")
			must.Equal(tt.wantSpace, reqs[0].Query.Get("space"))
		})
	}
}

func TestRecall_SendsAuthHeadersAndKeepsKeyOutOfQuery(t *testing.T) {
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
			must := require.New(t)
			fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{})
			defer fake.Close()
			c := newClient(t, fake.URL(), func(o *alexandria.Options) {
				o.AccessClientID, o.AccessClientSecret, o.MaxTokens = tt.accessID, tt.accessSec, tt.maxTokens
			})

			_, err := c.Recall(context.Background(), "", memory.Query{Text: "x"})
			must.NoError(err)
			r := fake.Requests()[0]
			must.Equal("Bearer "+testKey, r.Authorization)
			must.Equal(tt.accessID, r.AccessClientID, "Access client ID header")
			must.Equal(tt.accessSec, r.AccessClientSecret, "Access client secret header")
			must.Equal(tt.wantTokens, r.Query.Get("max_tokens"))
			for k, vs := range r.Query {
				for _, v := range vs {
					must.NotContains(v, testKey, "query param %s carries the API key", k)
				}
			}
		})
	}
}

func TestRecall_RefusesRedirects(t *testing.T) {
	must := require.New(t)
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
			must := require.New(t)
			fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{}, alexandriatest.WithRedirect(tt.location()))
			defer fake.Close()
			c := newClient(t, fake.URL(), func(o *alexandria.Options) {
				o.AccessClientID, o.AccessClientSecret = testAccessID, testAccessSecret
			})

			_, err := c.Recall(context.Background(), "", memory.Query{Text: "x"})
			must.ErrorContains(err, "redirect")
			must.NotContains(err.Error(), testKey, "error echoes the API key")
			must.NotContains(err.Error(), testAccessSecret, "error echoes the Access secret")
			must.Len(fake.Requests(), 1, "redirect not followed")
		})
	}
	must.Zero(targetHits.Load(), "redirect target received requests (Authorization %v)", targetAuth.Load())
}

func TestRecall_RefusesRedirectsDespiteCallerPolicy(t *testing.T) {
	// New must impose RefuseAllRedirects even when the caller's http.Client
	// already carries a permissive policy, for every redirect including one
	// that stays on the same origin.
	must := require.New(t)
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
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
			must := require.New(t)
			fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{}, alexandriatest.WithRedirect(tt.location()))
			defer fake.Close()
			c := newClient(t, fake.URL(), func(o *alexandria.Options) {
				o.AccessClientID, o.AccessClientSecret = testAccessID, testAccessSecret
				o.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
			})

			_, err := c.Recall(context.Background(), "", memory.Query{Text: "x"})
			must.ErrorContains(err, "redirect", "refusal must hold even though the caller's client permits redirects")
			must.Len(fake.Requests(), 1, "redirect not followed")
		})
	}
	must.Zero(targetHits.Load(), "redirect target received requests")
}

func TestRecall_ErrorStatusesAreScrubbedAndBounded(t *testing.T) {
	echo := `{"error":"invalid token ` + testKey + `"}`
	tests := []struct {
		name   string
		opts   []alexandriatest.FakeOption
		want   []string
		absent []string
		maxLen int
	}{
		{
			name: "429 with Retry-After",
			opts: []alexandriatest.FakeOption{alexandriatest.WithStatus(http.StatusTooManyRequests, `{"error":"slow down"}`), alexandriatest.WithHeader("Retry-After", "30")},
			want: []string{"HTTP 429", "Retry-After: 30"},
		},
		{
			name: "429 without Retry-After",
			opts: []alexandriatest.FakeOption{alexandriatest.WithStatus(http.StatusTooManyRequests, "")},
			want: []string{"HTTP 429"}, absent: []string{"Retry-After"},
		},
		{
			name: "429 Retry-After control characters stripped",
			opts: []alexandriatest.FakeOption{alexandriatest.WithStatus(http.StatusTooManyRequests, ""), alexandriatest.WithHeader("Retry-After", "30\tseconds")},
			want: []string{"Retry-After: 30seconds"},
		},
		{
			name: "401 never echoes the token",
			opts: []alexandriatest.FakeOption{alexandriatest.WithStatus(http.StatusUnauthorized, echo)},
			want: []string{"HTTP 401"}, absent: []string{testKey, "invalid token", "REDACTED"},
		},
		{
			name: "403 never echoes the token",
			opts: []alexandriatest.FakeOption{alexandriatest.WithStatus(http.StatusForbidden, echo)},
			want: []string{"HTTP 403"}, absent: []string{testKey, "invalid token"},
		},
		{
			name: "500 carries a scrubbed excerpt",
			opts: []alexandriatest.FakeOption{alexandriatest.WithStatus(http.StatusInternalServerError, echo)},
			want: []string{"HTTP 500", "invalid token", "[REDACTED:alexandria-api-key]"}, absent: []string{testKey},
		},
		{
			name: "500 Access secret scrubbed",
			opts: []alexandriatest.FakeOption{alexandriatest.WithStatus(http.StatusInternalServerError, "bad secret "+testAccessSecret)},
			want: []string{"[REDACTED:alexandria-access-secret]"}, absent: []string{testAccessSecret},
		},
		{
			name: "502 excerpt bounded",
			opts: []alexandriatest.FakeOption{alexandriatest.WithStatus(http.StatusBadGateway, strings.Repeat("upstream down ", 10000))},
			want: []string{"HTTP 502", "upstream down", "..."}, maxLen: 700,
		},
		{
			name: "500 without body",
			opts: []alexandriatest.FakeOption{alexandriatest.WithStatus(http.StatusInternalServerError, "")},
			want: []string{"HTTP 500"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{}, tt.opts...)
			defer fake.Close()
			c := newClient(t, fake.URL(), func(o *alexandria.Options) {
				o.AccessClientID, o.AccessClientSecret = testAccessID, testAccessSecret
			})

			_, err := c.Recall(context.Background(), "", memory.Query{Text: "x"})
			must.Error(err)
			msg := err.Error()
			must.True(strings.HasPrefix(msg, "alexandria: "), "error %q lacks the alexandria: prefix", msg)
			for _, w := range tt.want {
				must.ErrorContains(err, w)
			}
			for _, a := range tt.absent {
				must.NotContains(msg, a)
			}
			if tt.maxLen > 0 {
				must.LessOrEqual(len(msg), tt.maxLen, "error length")
			}
			must.Len(fake.Requests(), 1, "no retry")
		})
	}
}

func TestRecall_BodySizeBound(t *testing.T) {
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
			must := require.New(t)
			fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{}, alexandriatest.WithOversizedBody(tt.size))
			defer fake.Close()
			c := newClient(t, fake.URL(), func(o *alexandria.Options) { o.MaxBodyBytes = tt.bound })

			_, err := c.Recall(context.Background(), "", memory.Query{Text: "x"})
			if tt.wantErr {
				must.ErrorContains(err, "exceeds")
				return
			}
			must.NoError(err)
		})
	}
}

func TestRecall_DegradedLabelFollowsResponse(t *testing.T) {
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
			must := require.New(t)
			fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{Results: hits(3), Degraded: tt.degraded})
			defer fake.Close()
			c := newClient(t, fake.URL())

			got, err := c.Recall(context.Background(), "", memory.Query{Text: "x"})
			must.NoError(err)
			must.Len(got, 3)
			for i, h := range got {
				label, present := h.Memory.Meta.Labels["degraded"]
				must.Equal(tt.want != "", present, "hit %d has a degraded label", i)
				must.Equal(tt.want, label, "hit %d degraded label", i)
			}
		})
	}
}

func TestRecall_DecodesResponseStrictly(t *testing.T) {
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
		{name: "unknown fields ignored", body: `{"results":[{"ref":"kb://fragment/0f2e4d6c-1111-2222-3333-444455556666","unit":"fragment","id":"a","reranked":true,"contradicted_by":[{"ref":"kb://fragment/b"}],"citations":[{"ref":"kb://source/c","verified":true}]}],"next_cursor":null,"usage":{"tokens":10},"space_context":{"slug":"a"}}`, wantHits: 1},
		{name: "null optional fields", body: `{"results":[{"ref":"kb://fragment/0f2e4d6c-1111-2222-3333-444455556666","unit":"fragment","id":"a","title":null,"updated_at":null,"stale":null,"score":null}],"degraded":null}`, wantHits: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{}, alexandriatest.WithRawBody(tt.body))
			defer fake.Close()
			c := newClient(t, fake.URL())

			got, err := c.Recall(context.Background(), "", memory.Query{Text: "x"})
			if tt.wantErr != "" {
				must.ErrorContains(err, tt.wantErr)
				return
			}
			must.NoError(err)
			must.Len(got, tt.wantHits)
		})
	}
}

func TestRecall_RejectsEmptyQueryBeforeRequest(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "empty", text: ""},
		{name: "whitespace", text: " \t\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := alexandriatest.NewFakeServer(alexandria.SearchResponse{})
			defer fake.Close()
			c := newClient(t, fake.URL())

			_, err := c.Recall(context.Background(), "", memory.Query{Text: tt.text})
			must.ErrorContains(err, "query text must not be empty")
			must.Empty(fake.Requests(), "no request before validation passes")
		})
	}
}

func TestRecall_TransportErrorIsScrubbed(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := srv.URL
	srv.Close()
	c := newClient(t, endpoint)

	_, err := c.Recall(context.Background(), "", memory.Query{Text: testKey})
	must.Error(err, "want a transport error")
	must.True(strings.HasPrefix(err.Error(), "alexandria: GET /v1/search: "), "error %q lacks the transport prefix", err.Error())
	must.NotContains(err.Error(), testKey, "transport error echoes the key")
}
