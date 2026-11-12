package exa_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa/exatest"
)

// testKey is lower-case and hyphenated, a shape secret.Scrub's heuristics
// do not recognise, so only the client's exact-match redaction removes it.
const testKey = "exa-test-key-3f2b9c1e-5a7d-4e8f"

func newClient(t *testing.T, endpoint string, mutate ...func(*exa.Options)) *exa.Client {
	t.Helper()
	must := require.New(t)
	opts := exa.Options{Endpoint: endpoint, APIKey: testKey}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := exa.New(opts)
	must.NoError(err, "exa.New")
	return c
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	must := require.New(t)
	data, err := os.ReadFile(filepath.Join("testdata", name))
	must.NoError(err, "reading fixture %s", name)
	return string(data)
}

func decodeBody(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	must := require.New(t)
	var m map[string]any
	err := json.Unmarshal(raw, &m)
	must.NoError(err, "request body is not JSON:\n%s", raw)
	return m
}

func TestNew_ValidatesOptions(t *testing.T) {
	must := require.New(t)
	for _, tt := range []struct {
		name   string
		mutate func(*exa.Options)
	}{
		{"empty key", func(o *exa.Options) { o.APIKey = "" }},
		{"empty endpoint", func(o *exa.Options) { o.Endpoint = "" }},
		{"cleartext non-loopback endpoint", func(o *exa.Options) { o.Endpoint = "http://api.exa.ai" }},
		{"endpoint with query", func(o *exa.Options) { o.Endpoint = "https://api.exa.ai?x=1" }},
		{"result count above 100", func(o *exa.Options) { o.NumResults = 101 }},
		{"negative result count", func(o *exa.Options) { o.NumResults = -1 }},
		{"character bound above a million", func(o *exa.Options) { o.MaxCharacters = 1_000_001 }},
		{"negative character bound", func(o *exa.Options) { o.MaxCharacters = -1 }},
		{"negative cache age", func(o *exa.Options) { o.MaxAgeHours = -1 }},
		{"cache age above 720", func(o *exa.Options) { o.MaxAgeHours = 721 }},
		{"body bound below a page", func(o *exa.Options) { o.MaxCharacters = 100_000; o.MaxBodyBytes = 600_000 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			opts := exa.Options{Endpoint: "https://api.exa.ai", APIKey: testKey}
			tt.mutate(&opts)
			_, err := exa.New(opts)
			must.Error(err)
		})
	}
	_, err := exa.New(exa.Options{Endpoint: exa.DefaultEndpoint, APIKey: testKey, MaxCharacters: 1_000_000, MaxAgeHours: 720, NumResults: 100})
	must.NoError(err, "New at the upper bounds")
}

func TestSearch_RequestShape(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	fake := exatest.NewFakeServer(exatest.WithResults([]web.Result{{Title: "t", URL: "https://example.com/a", Snippet: "s"}}))
	defer fake.Close()
	c := newClient(t, fake.URL(), func(o *exa.Options) { o.UserAgent = "chiron-test" })

	_, err := c.Search(context.Background(), web.Query{Text: "sky colour", Objective: "why is the sky blue"})
	must.NoError(err, "Search")
	reqs := fake.Requests()
	must.Len(reqs, 1)
	r := reqs[0]
	check.Equal(http.MethodPost, r.Method)
	check.Equal("/search", r.Path)
	check.Equal(testKey, r.APIKey)
	check.Empty(r.Authorization, "only x-api-key is sent")
	check.Equal("application/json", r.ContentType)
	check.Equal("chiron-test", r.UserAgent)

	body := decodeBody(t, r.Body)
	want := map[string]any{
		"query":      "sky colour",
		"type":       "auto",
		"numResults": float64(10),
		"contents":   map[string]any{"highlights": map[string]any{"maxCharacters": float64(400)}},
	}
	for k, v := range want {
		got, _ := json.Marshal(body[k])
		exp, _ := json.Marshal(v)
		check.Equal(string(exp), string(got), "body[%s]", k)
	}
	check.NotContains(body, "objective", "objective sent with SendObjective off: %s", r.Body)
}

func TestSearch_ObjectiveCutToFit(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	fake := exatest.NewFakeServer()
	defer fake.Close()
	c := newClient(t, fake.URL(), func(o *exa.Options) { o.SendObjective = true })

	// Each emoji is two UTF-16 units, so 2,001 of them must be cut to 2,000.
	long := strings.Repeat("\U0001F600", 2001)
	_, err := c.Search(context.Background(), web.Query{Text: "q", Objective: long})
	must.NoError(err, "Search")
	got, _ := decodeBody(t, fake.Requests()[0].Body)["objective"].(string)
	check.True(utf8.ValidString(got), "objective is valid UTF-8")
	check.Len(utf16.Encode([]rune(got)), 4000, "objective length in UTF-16 units")

	_, err = c.Search(context.Background(), web.Query{Text: "q", Objective: "short goal"})
	must.NoError(err, "Search")
	check.Equal("short goal", decodeBody(t, fake.Requests()[1].Body)["objective"])
}

func TestSearch_MapsFixtures(t *testing.T) {
	for _, tt := range []struct {
		name     string
		fixture  string
		wantURLs []string
	}{
		{"highlights", "search-highlights.json", []string{
			"https://github.com/ericfitz/tmi/blob/main/internal/safehttp/safehttp.go",
			"https://pkg.go.dev/github.com/richardwooding/ssrfguard",
			"https://pkg.go.dev/github.com/cplieger/ssrf/v4",
		}},
		{"highlights max", "search-highlights-max.json", []string{
			"https://github.com/ericfitz/tmi/blob/main/internal/safehttp/safehttp.go",
			"https://pkg.go.dev/github.com/richardwooding/ssrfguard",
			"https://pkg.go.dev/github.com/cplieger/ssrf/v4",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := exatest.NewFakeServer(exatest.WithRawBody(fixture(t, tt.fixture)))
			defer fake.Close()

			resp, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "ssrf"})
			must.NoError(err, "Search")
			must.Equal(web.Spend{USD: 0.007}, resp.Spend)
			must.Len(resp.Results, len(tt.wantURLs))
			for i, r := range resp.Results {
				must.Equal(tt.wantURLs[i], r.URL, "result %d URL", i)
				must.NotEmpty(r.Title, "result %d title", i)
				must.NotEmpty(r.Snippet, "result %d snippet", i)
				must.True(utf8.ValidString(r.Snippet), "result %d snippet is valid UTF-8", i)
			}
		})
	}
}

func TestSearch_ResultMapping(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	raw := `{"results":[
		{"title":"h","url":"https://e.com/h","highlights":["one"," ","two"],"text":"ignored"},
		{"title":"t","url":"https://e.com/t","text":"page text"},
		{"title":"s","url":"https://e.com/s","summary":"a summary"},
		{"title":"x","url":"https://e.com/x"}
	],
	"notices":[{"code":"RESTRICTIONS_RELAXED","message":"site: restriction\nrelaxed"},{"message":""},
		{"message":"2"},{"message":"3"},{"message":"4"},{"message":"5"},{"message":"6"}],
	"warnings":[{"code":"IGNORED_FIELD","message":"never shown"}],
	"costDollars":{"total":0.006999999999999999}}`
	fake := exatest.NewFakeServer(exatest.WithRawBody(raw))
	defer fake.Close()

	resp, err := newClient(t, fake.URL(), func(o *exa.Options) { o.NumResults = 3 }).Search(context.Background(), web.Query{Text: "q"})
	must.NoError(err, "Search")
	must.Len(resp.Results, 3, "results = %+v, want the first 3", resp.Results)
	wantSnippets := []string{"one … two", "page text", "a summary"}
	for i, want := range wantSnippets {
		check.Equal(want, resp.Results[i].Snippet, "result %d snippet", i)
	}
	check.Equal([]string{"site: restriction\nrelaxed", "2", "3", "4", "5", "6"}, resp.Notes)
	check.Equal(web.Spend{USD: 0.007}, resp.Spend, "after rounding")
}

func TestSearch_SpendAndShapeFailures(t *testing.T) {
	for _, tt := range []struct {
		name      string
		raw       string
		wantErr   bool
		wantSpend web.Spend
	}{
		{"cost missing", `{"results":[]}`, false, web.Spend{Unknown: true}},
		{"cost total missing", `{"results":[],"costDollars":{}}`, false, web.Spend{Unknown: true}},
		{"negative cost", `{"results":[],"costDollars":{"total":-1}}`, false, web.Spend{Unknown: true}},
		{"no results array", `{"costDollars":{"total":0.007}}`, true, web.Spend{USD: 0.007}},
		{"not an object", `[]`, true, web.Spend{Unknown: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := exatest.NewFakeServer(exatest.WithRawBody(tt.raw))
			defer fake.Close()
			resp, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "q"})
			if tt.wantErr {
				must.Error(err)
			} else {
				must.NoError(err)
			}
			must.Equal(tt.wantSpend, resp.Spend)
		})
	}
}

func TestSearch_RefusesBlankQuery(t *testing.T) {
	must := require.New(t)
	fake := exatest.NewFakeServer()
	defer fake.Close()
	_, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "  "})
	must.Error(err, "Search succeeded on a blank query")
	must.Empty(fake.Requests(), "requests sent for a blank query")
}

func TestFetch_RequestShape(t *testing.T) {
	const page = "https://example.com/doc?id=7"
	for _, tt := range []struct {
		name        string
		maxAgeHours int
		wantAge     any
	}{
		{"default omits the cache age", 0, nil},
		{"cache age sent", 24, float64(24)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := exatest.NewFakeServer(exatest.WithPages(map[string]string{page: "text"}))
			defer fake.Close()
			c := newClient(t, fake.URL(), func(o *exa.Options) { o.MaxCharacters = 500; o.MaxAgeHours = tt.maxAgeHours })

			_, err := c.Fetch(context.Background(), page+"#section")
			must.NoError(err, "Fetch")
			r := fake.Requests()[0]
			must.Equal("/contents", r.Path)
			must.Equal(testKey, r.APIKey)
			must.Empty(r.Authorization, "only x-api-key is sent")
			body := decodeBody(t, r.Body)
			urls, _ := json.Marshal(body["urls"])
			must.Equal(`["`+page+`"]`, string(urls), "urls should be the URL without its fragment")
			text, _ := json.Marshal(body["text"])
			must.Equal(`{"maxCharacters":500}`, string(text), "text should be the bounded text view")
			must.Equal(tt.wantAge, body["maxAgeHours"])
		})
	}
}

func TestFetch_MapsFixtures(t *testing.T) {
	for _, tt := range []struct {
		name    string
		fixture string
		url     string
		prefix  string
	}{
		{"slash path", "contents-slash.json", "https://go.dev/doc/", "Documentation - The Go Programming Language"},
		{"redirect", "contents-redirect.json", "http://github.com/rxbynerd/chiron", "# rxbynerd/chiron"},
		{"pdf", "contents-pdf.json", "https://www.rfc-editor.org/rfc/rfc9421.pdf", "## RFC 9421"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			raw := fixture(t, tt.fixture)
			if tt.fixture == "contents-slash.json" {
				// The fixture records a two-URL request; the client sends one.
				raw = strings.Replace(raw, `,{"id":"https://go.dev/doc","status":"success","source":"cached"}`, "", 1)
			}
			fake := exatest.NewFakeServer(exatest.WithRawBody(raw))
			defer fake.Close()

			page, err := newClient(t, fake.URL(), func(o *exa.Options) { o.MaxCharacters = 500 }).Fetch(context.Background(), tt.url)
			must.NoError(err, "Fetch")
			must.Equal(tt.url, page.URL)
			must.Equal("text/markdown; charset=utf-8", page.ContentType)
			must.True(strings.HasPrefix(string(page.Content), tt.prefix), "content = %.80q, want it to start %q", page.Content, tt.prefix)
			must.True(page.Truncated, "a page cut at the 500-character bound is not marked truncated")
			must.Equal(web.Spend{USD: 0.001}, page.Spend)
		})
	}
}

func TestFetch_FailedURLs(t *testing.T) {
	must := require.New(t)
	for _, tt := range []struct {
		name       string
		fixture    string
		url        string
		wantTag    string
		wantStatus int
	}{
		{"not found", "contents-404.json", "https://go.dev/this-page-does-not-exist-chiron-smoke", "CRAWL_NOT_FOUND", 404},
		{"not cached", "contents-cacheonly.json", "https://github.com/rxbynerd/chiron/blob/main/AGENTS.md", "CONTENT_NOT_CACHED", 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := exatest.NewFakeServer(exatest.WithRawBody(fixture(t, tt.fixture)))
			defer fake.Close()

			page, err := newClient(t, fake.URL()).Fetch(context.Background(), tt.url)
			var fe *exa.FetchError
			must.ErrorAs(err, &fe)
			must.Equal(tt.wantTag, fe.Tag)
			must.Equal(tt.wantStatus, fe.TargetStatus)
			must.Equal(web.Spend{}, page.Spend, "spend for an unretrieved page")
		})
	}

	fake := exatest.NewFakeServer(exatest.WithFailedURL("https://example.com/blocked", "CRAWL_HTTP_403"))
	defer fake.Close()
	_, err := newClient(t, fake.URL()).Fetch(context.Background(), "https://example.com/blocked")
	var fe *exa.FetchError
	must.ErrorAs(err, &fe)
	must.Equal("CRAWL_HTTP_403", fe.Tag)
	must.Equal(403, fe.TargetStatus)
}

func TestFetch_RefusesInconsistentReplies(t *testing.T) {
	const u = "https://example.com/a"
	for _, tt := range []struct {
		name      string
		raw       string
		wantSpend web.Spend
	}{
		{"no status", `{"results":[{"url":"` + u + `","text":"x"}],"statuses":[],"costDollars":{"total":0.001}}`, web.Spend{USD: 0.001}},
		{"two statuses", `{"results":[],"statuses":[{"id":"` + u + `","status":"success"},{"id":"` + u + `","status":"success"}],"costDollars":{"total":0.002}}`, web.Spend{USD: 0.002}},
		{"unknown status", `{"results":[],"statuses":[{"id":"` + u + `","status":"pending"}],"costDollars":{"total":0}}`, web.Spend{}},
		{"success without a result", `{"results":[],"statuses":[{"id":"` + u + `","status":"success"}],"costDollars":{"total":0.001}}`, web.Spend{USD: 0.001}},
		{"empty text", `{"results":[{"url":"` + u + `","text":"  "}],"statuses":[{"id":"` + u + `","status":"success"}]}`, web.Spend{Unknown: true}},
		{"not JSON", `<html>`, web.Spend{Unknown: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := exatest.NewFakeServer(exatest.WithRawBody(tt.raw))
			defer fake.Close()
			page, err := newClient(t, fake.URL()).Fetch(context.Background(), u)
			must.Error(err, "Fetch succeeded with page %+v", page)
			must.Equal(tt.wantSpend, page.Spend)
		})
	}
}

func TestFetch_URLHygiene(t *testing.T) {
	for _, tt := range []struct {
		name string
		url  string
	}{
		{"userinfo", "https://user:pass@go.dev/doc/"},
		{"username only", "https://user@go.dev/doc/"},
		{"ftp scheme", "ftp://example.com/file"},
		{"javascript", "javascript:alert(1)"},
		{"no host", "https:///path"},
		{"localhost", "http://localhost:8080/"},
		{"localhost with trailing dot", "http://LOCALHOST./"},
		{"single label", "http://intranet/"},
		{".localhost", "http://app.localhost/"},
		{".local", "http://printer.local/"},
		{".internal", "http://metadata.google.internal/computeMetadata/v1/"},
		{".home.arpa", "http://router.home.arpa/"},
		{".lan", "http://nas.lan/"},
		{"bare home.arpa", "http://home.arpa/"},
		{"loopback v4", "http://127.0.0.1/"},
		{"loopback v6", "http://[::1]/"},
		{"metadata", "http://169.254.169.254/latest/meta-data/"},
		{"rfc1918", "https://10.1.2.3/"},
		{"cgnat metadata", "http://100.100.100.200/"},
		{"nat64 metadata", "http://[64:ff9b::a9fe:a9fe]/"},
		{"zoned link-local", "http://[fe80::1%25eth0]/"},
		{"numeric last label", "http://0x7f.0.0.1/"},
		{"hex last label", "http://example.0x7f/"},
		{"non-ascii last label", "http://example.中国/"},
		{"full-width dots", "http://127。0。0。1/"},
		{"too long", "https://example.com/" + strings.Repeat("a", 2048)},
		{"one byte too long", "https://example.com/" + strings.Repeat("a", 2049-len("https://example.com/"))},
		{"double trailing dot", "http://localhost../"},
		{"local with double trailing dot", "http://printer.local../"},
		{"internal with double trailing dot", "http://wiki.internal../"},
		{"loopback with trailing dots", "http://127.0.0.1../"},
		{"empty inner label", "http://a..com/"},
		{"soft hyphen in a local suffix", "http://foo.ho\u00adme.arpa/"},
		{"zero-width space", "http://wiki\u200b.example.com/"},
		{"uppercase local", "http://PRINTER.LOCAL/"},
		{".localdomain", "http://host.localdomain/"},
		{".corp", "http://wiki.corp/"},
		{".home", "http://nas.home/"},
		{".intranet", "http://portal.intranet/"},
		{".private", "http://db.private/"},
		{".test", "http://app.test/"},
		{".invalid", "http://x.invalid/"},
		{".example", "http://www.example/"},
		{".onion", "http://abcdefghijklmnop.onion/"},
		{".in-addr.arpa", "http://1.0.0.127.in-addr.arpa/"},
		{".ip6.arpa", "http://1.0.0.0.ip6.arpa/"},
		{"mapped loopback", "http://[::ffff:127.0.0.1]/"},
		{"cgnat", "http://100.64.0.1/"},
		{"short loopback", "http://127.1/"},
		{"decimal loopback", "http://2130706433/"},
		{"octal loopback", "http://0177.0.0.1/"},
		{"teredo", "http://[2001:0:4136:e378:8000:63bf:3fff:fdd2]/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := exatest.NewFakeServer()
			defer fake.Close()
			_, err := newClient(t, fake.URL()).Fetch(context.Background(), tt.url)
			must.ErrorIs(err, web.ErrRefusedDestination)
			must.NotContains(err.Error(), "pass", "error echoes userinfo")
			must.Empty(fake.Requests(), "nothing should be sent")
		})
	}

	exact := "https://example.com/" + strings.Repeat("a", 2048-len("https://example.com/"))
	for _, ok := range []string{"https://8.8.8.8/dns", "https://xn--fiqs8s.example.com/", "http://example.com:8080/a?b=c", "http://example.com./a", exact} {
		t.Run("admits "+ok[:min(len(ok), 40)], func(t *testing.T) {
			must := require.New(t)
			fake := exatest.NewFakeServer(exatest.WithPages(map[string]string{ok: "text"}))
			defer fake.Close()
			_, err := newClient(t, fake.URL()).Fetch(context.Background(), ok)
			must.NoError(err, "Fetch(%s) should be admitted", ok)
		})
	}
}

func TestErrors_ClassifyAccountFailures(t *testing.T) {
	for _, tt := range []struct {
		name         string
		status       int
		body         string
		wantTag      string
		wantUnusable bool
		wantSpend    web.Spend
	}{
		{"invalid key", 401, `{"requestId":"r1","error":"Invalid API key","tag":"INVALID_API_KEY"}`, "INVALID_API_KEY", true, web.Spend{}},
		{"no credits", 402, `{"requestId":"r1","error":"out","tag":"NO_MORE_CREDITS"}`, "NO_MORE_CREDITS", true, web.Spend{}},
		{"key budget", 402, `{"requestId":"r1","error":"out","tag":"API_KEY_BUDGET_EXCEEDED"}`, "API_KEY_BUDGET_EXCEEDED", true, web.Spend{}},
		{"team budget", 402, `{"requestId":"r1","error":"out","tag":"TEAM_BUDGET_EXCEEDED"}`, "TEAM_BUDGET_EXCEEDED", true, web.Spend{}},
		{"payment challenge", 402, fixture(t, "error-noauth.json"), "X402_PAYMENT_REQUIRED", false, web.Spend{}},
		{"team blocked", 403, `{"requestId":"r1","error":"blocked","tag":"TEAM_BLOCKED"}`, "TEAM_BLOCKED", true, web.Spend{}},
		{"feature disabled", 403, `{"requestId":"r1","error":"no","tag":"FEATURE_DISABLED"}`, "FEATURE_DISABLED", false, web.Spend{}},
		{"invalid body", 400, fixture(t, "error-long-objective.json"), "INVALID_REQUEST_BODY", false, web.Spend{}},
		{"rate limited", 429, `{"requestId":"r1","error":"slow down","tag":"RATE_LIMIT_EXCEEDED"}`, "RATE_LIMIT_EXCEEDED", false, web.Spend{}},
		{"server error", 500, `{"requestId":"r1","error":"boom","tag":"INTERNAL_ERROR"}`, "INTERNAL_ERROR", false, web.Spend{Unknown: true}},
		{"deadline", 504, `<html>gateway timeout</html>`, "", false, web.Spend{Unknown: true}},
		{"edge overload", 503, `<html>overloaded</html>`, "", false, web.Spend{Unknown: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := exatest.NewFakeServer(exatest.WithStatus(tt.status, tt.body))
			defer fake.Close()

			resp, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "q"})
			var apiErr *exa.APIError
			must.ErrorAs(err, &apiErr)
			must.Equal(tt.status, apiErr.Status)
			must.Equal(tt.wantTag, apiErr.Tag)
			must.Equal(tt.wantUnusable, errors.Is(err, web.ErrAccountUnusable))
			must.Equal(tt.wantSpend, resp.Spend)
			must.Len(fake.Requests(), 1, "nothing but an enveloped overload is re-sent")
			must.NotContains(err.Error(), "<html>", "error echoes a non-JSON body")
		})
	}
}

func TestErrors_NeverCarryTheKeyOrRequestBody(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const query = "the secret research question"
	const objective = "a confidential  objective"
	body := `{"requestId":"r1","error":"bad key ` + testKey + ` for x-api-key: ` + testKey +
		` while searching \"` + query + `\" towards a confidential objective","tag":"INVALID_API_KEY"}`
	fake := exatest.NewFakeServer(exatest.WithStatus(401, body))
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *exa.Options) { o.SendObjective = true })
	_, err := c.Search(context.Background(), web.Query{Text: query, Objective: objective})
	must.Error(err)
	msg := err.Error()
	for _, leak := range []string{testKey, query, "confidential objective"} {
		check.NotContains(msg, leak, "error leaks %q", leak)
	}
	check.Contains(msg, "[REDACTED:request-text]")
	check.Contains(msg, "(request r1)")
}

// TestErrors_RequestIDCarryingTheKeyIsDropped: a request id that is the key
// itself is never echoed.
func TestErrors_RequestIDCarryingTheKeyIsDropped(t *testing.T) {
	must := require.New(t)
	const key = "abc123def456"
	fake := exatest.NewFakeServer(exatest.WithStatus(500, `{"requestId":"`+key+`","error":"boom","tag":"INTERNAL_ERROR"}`))
	defer fake.Close()
	_, err := newClient(t, fake.URL(), func(o *exa.Options) { o.APIKey = key }).Search(context.Background(), web.Query{Text: "q"})
	var apiErr *exa.APIError
	must.ErrorAs(err, &apiErr)
	must.Empty(apiErr.RequestID)
	must.NotContains(err.Error(), key)
}

func TestErrors_RequestIDFallsBackToHeader(t *testing.T) {
	must := require.New(t)
	fake := exatest.NewFakeServer(exatest.WithStatus(500, "upstream"), exatest.WithHeader("x-request-id", "abc123"))
	defer fake.Close()
	_, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "q"})
	var apiErr *exa.APIError
	must.ErrorAs(err, &apiErr)
	must.Equal("abc123", apiErr.RequestID)
}

// TestOverloadedRequest_SentOnceMore: only Exa's enveloped
// SERVICE_OVERLOADED 503 is re-sent, once, with the same body and key, on
// either endpoint.
func TestOverloadedRequest_SentOnceMore(t *testing.T) {
	const page = "https://example.com/a"
	const overloaded = `{"requestId":"r1","error":"over capacity","tag":"SERVICE_OVERLOADED"}`
	served := map[string]string{
		"/search":   `{"results":[],"costDollars":{"total":0.007}}`,
		"/contents": `{"results":[{"url":"` + page + `","text":"t"}],"statuses":[{"id":"` + page + `","status":"success"}],"costDollars":{"total":0.001}}`,
	}
	type reply struct {
		code int
		body string
	}
	for _, path := range []string{"/search", "/contents"} {
		for _, tt := range []struct {
			name      string
			replies   []reply
			wantCalls int
			wantErr   bool
		}{
			{"overloaded then served", []reply{{503, overloaded}}, 2, false},
			{"overloaded twice", []reply{{503, overloaded}, {503, overloaded}}, 2, true},
			{"edge 503 is not re-sent", []reply{{503, "<html>busy</html>"}}, 1, true},
			{"other 503 tag is not re-sent", []reply{{503, `{"error":"x","tag":"MAINTENANCE"}`}}, 1, true},
			{"500 is not re-sent", []reply{{500, `{"error":"x","tag":"SERVICE_OVERLOADED"}`}}, 1, true},
		} {
			t.Run(path+"/"+tt.name, func(t *testing.T) {
				must := require.New(t)
				var mu sync.Mutex
				var bodies, keys []string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					mu.Lock()
					n := len(bodies)
					bodies = append(bodies, string(body))
					keys = append(keys, r.Header.Get("x-api-key"))
					mu.Unlock()
					if n < len(tt.replies) {
						w.WriteHeader(tt.replies[n].code)
						_, _ = io.WriteString(w, tt.replies[n].body)
						return
					}
					_, _ = io.WriteString(w, served[r.URL.Path])
				}))
				defer srv.Close()

				c := newClient(t, srv.URL)
				var err error
				if path == "/search" {
					_, err = c.Search(context.Background(), web.Query{Text: "q"})
				} else {
					_, err = c.Fetch(context.Background(), page)
				}
				if tt.wantErr {
					must.Error(err)
				} else {
					must.NoError(err)
				}
				mu.Lock()
				defer mu.Unlock()
				must.Len(bodies, tt.wantCalls)
				for i := 1; i < len(bodies); i++ {
					must.Equal(bodies[0], bodies[i], "re-sent request %d body", i)
					must.Equal(testKey, keys[i], "re-sent request %d key", i)
				}
			})
		}
	}
}

func TestRedirect_NeverFollowed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			leaked.Store(true)
		}
	}))
	defer target.Close()
	fake := exatest.NewFakeServer(exatest.WithRedirect(target.URL + "/search?token=redirect-secret"))
	defer fake.Close()

	resp, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "q"})
	must.Error(err, "Search followed a redirect")
	check.False(leaked.Load(), "the key reached the redirect target")
	check.NotContains(err.Error(), "redirect-secret", "error echoes the redirect target's URL")
	check.Equal(web.Spend{Unknown: true}, resp.Spend, "unknown: the request reached Exa before it redirected")
}

func TestOversizedBody_IsErrorWithUnknownSpend(t *testing.T) {
	must := require.New(t)
	fake := exatest.NewFakeServer(exatest.WithOversizedBody(2 << 20))
	defer fake.Close()
	resp, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "q"})
	must.ErrorContains(err, "bound", "the body bound should be named")
	must.Equal(web.Spend{Unknown: true}, resp.Spend, "unknown for a billed reply that could not be read")
}

func TestSearch_TransportFailureSpend(t *testing.T) {
	t.Run("timeout after the request was sent", func(t *testing.T) {
		must := require.New(t)
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()
		defer close(release)
		c := newClient(t, srv.URL, func(o *exa.Options) { o.RequestTimeout = 100 * time.Millisecond })
		resp, err := c.Search(context.Background(), web.Query{Text: "q"})
		must.Error(err, "want a timeout")
		must.Equal(web.Spend{Unknown: true}, resp.Spend, "unknown once the request was sent")
	})
	t.Run("connection refused", func(t *testing.T) {
		must := require.New(t)
		srv := httptest.NewServer(http.NotFoundHandler())
		endpoint := srv.URL
		srv.Close()
		resp, err := newClient(t, endpoint).Search(context.Background(), web.Query{Text: "q"})
		must.Error(err, "want a connection error")
		must.Equal(web.Spend{}, resp.Spend, "zero when nothing was sent")
	})
}

func TestFakeServer_ChallengesKeylessRequest(t *testing.T) {
	must := require.New(t)
	fake := exatest.NewFakeServer()
	defer fake.Close()
	resp, err := http.Post(fake.URL()+"/search", "application/json", strings.NewReader(`{"query":"q"}`))
	must.NoError(err, "POST")
	defer resp.Body.Close()
	var env exa.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&env)
	must.Equal(http.StatusPaymentRequired, resp.StatusCode)
	must.Equal("X402_PAYMENT_REQUIRED", env.Tag)
}

// TestFetch_AcceptsNormalisedStatusID: the one status in a reply belongs to
// the one URL sent, even when Exa reports it in a normalised form.
func TestFetch_AcceptsNormalisedStatusID(t *testing.T) {
	must := require.New(t)
	const sent = "https://example.com/docs"
	raw := `{"results":[{"id":"` + sent + `/","url":"` + sent + `/","text":"page text"}],` +
		`"statuses":[{"id":"` + sent + `/","status":"success"}],"costDollars":{"total":0.001}}`
	fake := exatest.NewFakeServer(exatest.WithRawBody(raw))
	defer fake.Close()

	page, err := newClient(t, fake.URL()).Fetch(context.Background(), sent)
	must.NoError(err, "Fetch")
	must.Equal(sent, page.URL, "URL as sent")
	must.Equal("page text", string(page.Content))
}

// TestFetch_TruncationCountsUTF16Units: a page cut at the bound in UTF-16
// units, fewer runes than the bound when it holds astral characters, still
// reads as truncated.
func TestFetch_TruncationCountsUTF16Units(t *testing.T) {
	const u = "https://example.com/emoji"
	for _, tt := range []struct {
		name string
		text string
		want bool
	}{
		{"cut in UTF-16 units", strings.Repeat("\U0001F600", 5), true},
		{"cut in runes", strings.Repeat("a", 10), true},
		{"short page", strings.Repeat("a", 9), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := exatest.NewFakeServer(exatest.WithPages(map[string]string{u: tt.text}))
			defer fake.Close()
			page, err := newClient(t, fake.URL(), func(o *exa.Options) { o.MaxCharacters = 10 }).Fetch(context.Background(), u)
			must.NoError(err, "Fetch")
			must.Equal(tt.want, page.Truncated)
		})
	}
}

// TestErrors_HostileEnvelopes: a malformed tag or request id is dropped,
// the message reaches the caller as one bounded line of printable text, and
// an envelope cut off by the error-body bound contributes only its status.
func TestErrors_HostileEnvelopes(t *testing.T) {
	t.Run("malformed tag", func(t *testing.T) {
		must := require.New(t)
		e := searchAPIError(t, 400, `{"requestId":"r1","error":"bad","tag":"bad-tag!"}`)
		must.Empty(e.Tag)
		must.Equal("bad", e.Message)
	})
	t.Run("request id with control characters", func(t *testing.T) {
		must := require.New(t)
		e := searchAPIError(t, 400, `{"requestId":"r1\u001b[31m","error":"bad","tag":"INVALID_REQUEST"}`)
		must.Empty(e.RequestID)
	})
	t.Run("message with newlines and escapes", func(t *testing.T) {
		must := require.New(t)
		e := searchAPIError(t, 400, `{"error":"line one\nline two \u001b[31mred\u001b[0m\u0007","tag":"INVALID_REQUEST"}`)
		must.Equal("line one line two [31mred[0m", e.Message)
	})
	t.Run("multibyte message over the bound", func(t *testing.T) {
		must := require.New(t)
		e := searchAPIError(t, 400, `{"error":"`+strings.Repeat("é", 600)+`","tag":"INVALID_REQUEST"}`)
		must.LessOrEqual(len(e.Message), 512, "message length in bytes")
		must.True(utf8.ValidString(e.Message), "message is valid UTF-8")
	})
	t.Run("envelope past the body bound", func(t *testing.T) {
		must := require.New(t)
		e := searchAPIError(t, 401, `{"error":"`+strings.Repeat("x", 10<<10)+`","tag":"INVALID_API_KEY"}`)
		must.Empty(e.Tag)
		must.Empty(e.Message)
	})
}

// searchAPIError runs a search against a fake that replies with status and
// body, and returns the APIError the search fails with.
func searchAPIError(t *testing.T, status int, body string) *exa.APIError {
	t.Helper()
	must := require.New(t)
	fake := exatest.NewFakeServer(exatest.WithStatus(status, body))
	t.Cleanup(func() { fake.Close() })
	_, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "q"})
	var apiErr *exa.APIError
	must.ErrorAs(err, &apiErr)
	must.Equal(status, apiErr.Status)
	return apiErr
}

// TestFetch_ErrorReplySpendAndAccountState: a non-2xx reply to a contents call
// carries the spend its status implies and classifies the account as search
// does.
func TestFetch_ErrorReplySpendAndAccountState(t *testing.T) {
	const u = "https://example.com/a"
	for _, tt := range []struct {
		name         string
		status       int
		body         string
		wantSpend    web.Spend
		wantUnusable bool
	}{
		{"enveloped server error", 500, `{"error":"boom","tag":"INTERNAL_ERROR"}`, web.Spend{Unknown: true}, false},
		{"edge server error", 502, "<html>bad gateway</html>", web.Spend{Unknown: true}, false},
		{"rate limited", 429, `{"error":"slow down","tag":"RATE_LIMIT_EXCEEDED"}`, web.Spend{}, false},
		{"no credits", 402, `{"error":"out","tag":"NO_MORE_CREDITS"}`, web.Spend{}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := exatest.NewFakeServer(exatest.WithStatus(tt.status, tt.body))
			defer fake.Close()
			page, err := newClient(t, fake.URL()).Fetch(context.Background(), u)
			var apiErr *exa.APIError
			must.ErrorAs(err, &apiErr)
			must.Equal(tt.status, apiErr.Status)
			must.Equal(tt.wantSpend, page.Spend)
			must.Equal(tt.wantUnusable, errors.Is(err, web.ErrAccountUnusable))
		})
	}
}

// TestErrors_ClassifiesStatusWithoutEnvelope: a 401 is an authentication
// failure whatever its body, so it ends the run; a 402 or 403 without Exa's
// envelope names no cause and does not.
func TestErrors_ClassifiesStatusWithoutEnvelope(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		want   bool
	}{
		{"401 without envelope is unusable", 401, true},
		{"402 without envelope is not unusable", 402, false},
		{"403 without envelope is not unusable", 403, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := exatest.NewFakeServer(exatest.WithStatus(tt.status, "<html>denied</html>"))
			defer fake.Close()
			_, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "q"})
			must.Equal(tt.want, errors.Is(err, web.ErrAccountUnusable))
		})
	}
}

// TestFetch_ErrorRenderingNamesTagAndStatus: a failed URL names its tag and
// target status when Exa gives well-formed ones, and nothing more otherwise.
func TestFetch_ErrorRenderingNamesTagAndStatus(t *testing.T) {
	const u = "https://example.com/a"
	reply := func(errObj string) string {
		return `{"results":[],"statuses":[{"id":"` + u + `","status":"error","error":` + errObj + `}],"costDollars":{"total":0}}`
	}
	for _, tt := range []struct {
		name  string
		error string
		want  string
	}{
		{"tag and status", `{"tag":"CRAWL_NOT_FOUND","httpStatusCode":404}`, "exa: could not retrieve " + u + ": CRAWL_NOT_FOUND (the page answered HTTP 404)"},
		{"malformed tag", `{"tag":"not a tag","httpStatusCode":404}`, "exa: could not retrieve " + u + " (the page answered HTTP 404)"},
		{"out-of-range status", `{"tag":"CRAWL_UNKNOWN_ERROR","httpStatusCode":9999}`, "exa: could not retrieve " + u + ": CRAWL_UNKNOWN_ERROR"},
		{"null error", `null`, "exa: could not retrieve " + u},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := exatest.NewFakeServer(exatest.WithRawBody(reply(tt.error)))
			defer fake.Close()
			_, err := newClient(t, fake.URL()).Fetch(context.Background(), u)
			var fe *exa.FetchError
			must.ErrorAs(err, &fe)
			must.Equal(tt.want, err.Error())
		})
	}
}

// TestNew_CopiesCallerClient: the caller's client is copied, not mutated,
// and its redirect policy is replaced by the refusal.
func TestNew_CopiesCallerClient(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Store(true) }))
	defer target.Close()
	fake := exatest.NewFakeServer(exatest.WithRedirect(target.URL + "/search"))
	defer fake.Close()

	followAll := func(*http.Request, []*http.Request) error { return nil }
	caller := &http.Client{CheckRedirect: followAll}
	c := newClient(t, fake.URL(), func(o *exa.Options) { o.HTTPClient = caller })
	_, err := c.Search(context.Background(), web.Query{Text: "q"})
	must.Error(err, "the redirect should be refused")
	check.False(followed.Load(), "the redirect target was reached")
	check.NotNil(caller.CheckRedirect, "the caller's redirect policy was cleared")
	check.Nil(caller.Transport, "the caller's transport was set")
	check.Zero(caller.Timeout, "the caller's timeout was set")
}

func TestNew_DefaultUserAgentAndBodyBound(t *testing.T) {
	t.Run("default user agent", func(t *testing.T) {
		must := require.New(t)
		fake := exatest.NewFakeServer()
		defer fake.Close()
		_, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "q"})
		must.NoError(err, "Search")
		must.Equal("chiron/2 (+https://github.com/rxbynerd/chiron; research-only)", fake.Requests()[0].UserAgent)
	})
	t.Run("body bound follows the page bound", func(t *testing.T) {
		must := require.New(t)
		// A million-character page bound raises the body bound past 6 MB, so a
		// 2 MiB reply that the 1 MiB default refuses is read.
		big := exatest.NewFakeServer(exatest.WithOversizedBody(2 << 20))
		defer big.Close()
		c := newClient(t, big.URL(), func(o *exa.Options) { o.MaxCharacters = 1_000_000 })
		_, err := c.Search(context.Background(), web.Query{Text: "q"})
		must.NoError(err, "Search with a large page bound should read the 2 MiB reply")
	})
}

// TestClient_IsSafeForConcurrentUse drives one client from several
// goroutines in both roles; the race detector checks it.
func TestClient_IsSafeForConcurrentUse(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const u = "https://example.com/a"
	fake := exatest.NewFakeServer(
		exatest.WithResults([]web.Result{{Title: "t", URL: u, Snippet: "s"}}),
		exatest.WithPages(map[string]string{u: "text"}),
	)
	defer fake.Close()
	c := newClient(t, fake.URL())
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := c.Search(context.Background(), web.Query{Text: "q" + strings.Repeat("x", i)}); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := c.Fetch(context.Background(), u); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must.NoError(err)
	}
	check.Len(fake.Requests(), 16, "requests sent")
}
