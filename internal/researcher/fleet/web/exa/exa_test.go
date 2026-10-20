package exa_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa/exatest"
)

// testKey is lower-case and hyphenated, a shape secret.Scrub's heuristics
// do not recognise, so only the client's exact-match redaction removes it.
const testKey = "exa-test-key-3f2b9c1e-5a7d-4e8f"

func newClient(t *testing.T, endpoint string, mutate ...func(*exa.Options)) *exa.Client {
	t.Helper()
	opts := exa.Options{Endpoint: endpoint, APIKey: testKey}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := exa.New(opts)
	if err != nil {
		t.Fatalf("exa.New: %v", err)
	}
	return c
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return string(data)
}

func decodeBody(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, raw)
	}
	return m
}

func TestNewValidatesOptions(t *testing.T) {
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
			opts := exa.Options{Endpoint: "https://api.exa.ai", APIKey: testKey}
			tt.mutate(&opts)
			if _, err := exa.New(opts); err == nil {
				t.Fatal("New succeeded, want an error")
			}
		})
	}
	if _, err := exa.New(exa.Options{Endpoint: exa.DefaultEndpoint, APIKey: testKey, MaxCharacters: 1_000_000, MaxAgeHours: 720, NumResults: 100}); err != nil {
		t.Errorf("New at the upper bounds: %v", err)
	}
}

func TestSearchRequestShape(t *testing.T) {
	fake := exatest.NewFakeServer(exatest.WithResults([]web.Result{{Title: "t", URL: "https://example.com/a", Snippet: "s"}}))
	defer fake.Close()
	c := newClient(t, fake.URL(), func(o *exa.Options) { o.UserAgent = "chiron-test" })

	if _, err := c.Search(context.Background(), web.Query{Text: "sky colour", Objective: "why is the sky blue"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/search" || r.APIKey != testKey || r.Authorization != "" ||
		r.ContentType != "application/json" || r.UserAgent != "chiron-test" {
		t.Errorf("request = %+v, want POST /search with only x-api-key", r)
	}
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
		if string(got) != string(exp) {
			t.Errorf("body[%s] = %s, want %s", k, got, exp)
		}
	}
	if _, ok := body["objective"]; ok {
		t.Errorf("objective sent with SendObjective off: %s", r.Body)
	}
}

func TestSearchSendsObjectiveCutToFit(t *testing.T) {
	fake := exatest.NewFakeServer()
	defer fake.Close()
	c := newClient(t, fake.URL(), func(o *exa.Options) { o.SendObjective = true })

	// Each emoji is two UTF-16 units, so 2,001 of them must be cut to 2,000.
	long := strings.Repeat("\U0001F600", 2001)
	if _, err := c.Search(context.Background(), web.Query{Text: "q", Objective: long}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	got, _ := decodeBody(t, fake.Requests()[0].Body)["objective"].(string)
	if units := len(utf16.Encode([]rune(got))); units != 4000 || !utf8.ValidString(got) {
		t.Errorf("objective is %d UTF-16 units (valid UTF-8: %v), want 4000", units, utf8.ValidString(got))
	}

	if _, err := c.Search(context.Background(), web.Query{Text: "q", Objective: "short goal"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := decodeBody(t, fake.Requests()[1].Body)["objective"]; got != "short goal" {
		t.Errorf("objective = %v, want it sent unchanged", got)
	}
}

func TestSearchMapsFixtures(t *testing.T) {
	for _, tt := range []struct {
		fixture  string
		wantURLs []string
	}{
		{"search-highlights.json", []string{
			"https://github.com/ericfitz/tmi/blob/main/internal/safehttp/safehttp.go",
			"https://pkg.go.dev/github.com/richardwooding/ssrfguard",
			"https://pkg.go.dev/github.com/cplieger/ssrf/v4",
		}},
		{"search-highlights-max.json", []string{
			"https://github.com/ericfitz/tmi/blob/main/internal/safehttp/safehttp.go",
			"https://pkg.go.dev/github.com/richardwooding/ssrfguard",
			"https://pkg.go.dev/github.com/cplieger/ssrf/v4",
		}},
	} {
		t.Run(tt.fixture, func(t *testing.T) {
			fake := exatest.NewFakeServer(exatest.WithRawBody(fixture(t, tt.fixture)))
			defer fake.Close()

			resp, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "ssrf"})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if resp.Spend != (web.Spend{USD: 0.007}) {
				t.Errorf("spend = %+v, want 0.007 USD", resp.Spend)
			}
			if len(resp.Results) != len(tt.wantURLs) {
				t.Fatalf("results = %d, want %d", len(resp.Results), len(tt.wantURLs))
			}
			for i, r := range resp.Results {
				if r.URL != tt.wantURLs[i] || r.Title == "" || r.Snippet == "" {
					t.Errorf("result %d = %+v, want %s with a title and snippet", i, r, tt.wantURLs[i])
				}
				if !utf8.ValidString(r.Snippet) {
					t.Errorf("result %d snippet is not valid UTF-8", i)
				}
			}
		})
	}
}

func TestSearchResultMapping(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	wantSnippets := []string{"one … two", "page text", "a summary"}
	if len(resp.Results) != 3 {
		t.Fatalf("results = %+v, want the first 3", resp.Results)
	}
	for i, want := range wantSnippets {
		if resp.Results[i].Snippet != want {
			t.Errorf("result %d snippet = %q, want %q", i, resp.Results[i].Snippet, want)
		}
	}
	wantNotes := []string{"site: restriction\nrelaxed", "2", "3", "4", "5", "6"}
	if strings.Join(resp.Notes, "|") != strings.Join(wantNotes, "|") {
		t.Errorf("notes = %q, want %q", resp.Notes, wantNotes)
	}
	if resp.Spend != (web.Spend{USD: 0.007}) {
		t.Errorf("spend = %+v, want 0.007 USD after rounding", resp.Spend)
	}
}

func TestSearchSpendAndShapeFailures(t *testing.T) {
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
			fake := exatest.NewFakeServer(exatest.WithRawBody(tt.raw))
			defer fake.Close()
			resp, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "q"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if resp.Spend != tt.wantSpend {
				t.Errorf("spend = %+v, want %+v", resp.Spend, tt.wantSpend)
			}
		})
	}
}

func TestSearchRefusesBlankQuery(t *testing.T) {
	fake := exatest.NewFakeServer()
	defer fake.Close()
	if _, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "  "}); err == nil {
		t.Fatal("Search succeeded on a blank query")
	}
	if n := len(fake.Requests()); n != 0 {
		t.Errorf("requests = %d, want none", n)
	}
}

func TestFetchRequestShape(t *testing.T) {
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
			fake := exatest.NewFakeServer(exatest.WithPages(map[string]string{page: "text"}))
			defer fake.Close()
			c := newClient(t, fake.URL(), func(o *exa.Options) { o.MaxCharacters = 500; o.MaxAgeHours = tt.maxAgeHours })

			if _, err := c.Fetch(context.Background(), page+"#section"); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			r := fake.Requests()[0]
			if r.Path != "/contents" || r.APIKey != testKey || r.Authorization != "" {
				t.Errorf("request = %+v, want POST /contents with only x-api-key", r)
			}
			body := decodeBody(t, r.Body)
			if urls, _ := json.Marshal(body["urls"]); string(urls) != `["`+page+`"]` {
				t.Errorf("urls = %s, want the URL without its fragment", urls)
			}
			if text, _ := json.Marshal(body["text"]); string(text) != `{"maxCharacters":500}` {
				t.Errorf("text = %s, want the bounded text view", text)
			}
			if body["maxAgeHours"] != tt.wantAge {
				t.Errorf("maxAgeHours = %v, want %v", body["maxAgeHours"], tt.wantAge)
			}
		})
	}
}

func TestFetchMapsFixtures(t *testing.T) {
	for _, tt := range []struct {
		fixture string
		url     string
		prefix  string
	}{
		{"contents-slash.json", "https://go.dev/doc/", "Documentation - The Go Programming Language"},
		{"contents-redirect.json", "http://github.com/rxbynerd/chiron", "# rxbynerd/chiron"},
		{"contents-pdf.json", "https://www.rfc-editor.org/rfc/rfc9421.pdf", "## RFC 9421"},
	} {
		t.Run(tt.fixture, func(t *testing.T) {
			raw := fixture(t, tt.fixture)
			if tt.fixture == "contents-slash.json" {
				// The smoke call asked for two URLs; the client sends one.
				raw = strings.Replace(raw, `,{"id":"https://go.dev/doc","status":"success","source":"cached"}`, "", 1)
			}
			fake := exatest.NewFakeServer(exatest.WithRawBody(raw))
			defer fake.Close()

			page, err := newClient(t, fake.URL(), func(o *exa.Options) { o.MaxCharacters = 500 }).Fetch(context.Background(), tt.url)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if page.URL != tt.url || page.ContentType != "text/markdown; charset=utf-8" {
				t.Errorf("page = %s (%s), want %s as Markdown", page.URL, page.ContentType, tt.url)
			}
			if !strings.HasPrefix(string(page.Content), tt.prefix) {
				t.Errorf("content = %.80q, want it to start %q", page.Content, tt.prefix)
			}
			if !page.Truncated {
				t.Error("a page cut at the 500-character bound is not marked truncated")
			}
			if page.Spend != (web.Spend{USD: 0.001}) {
				t.Errorf("spend = %+v, want 0.001 USD", page.Spend)
			}
		})
	}
}

func TestFetchFailedURLs(t *testing.T) {
	for _, tt := range []struct {
		fixture    string
		url        string
		wantTag    string
		wantStatus int
	}{
		{"contents-404.json", "https://go.dev/this-page-does-not-exist-chiron-smoke", "CRAWL_NOT_FOUND", 404},
		{"contents-cacheonly.json", "https://github.com/rxbynerd/chiron/blob/main/AGENTS.md", "CONTENT_NOT_CACHED", 404},
	} {
		t.Run(tt.fixture, func(t *testing.T) {
			fake := exatest.NewFakeServer(exatest.WithRawBody(fixture(t, tt.fixture)))
			defer fake.Close()

			page, err := newClient(t, fake.URL()).Fetch(context.Background(), tt.url)
			var fe *exa.FetchError
			if !errors.As(err, &fe) || fe.Tag != tt.wantTag || fe.TargetStatus != tt.wantStatus {
				t.Fatalf("err = %v, want a FetchError %s/%d", err, tt.wantTag, tt.wantStatus)
			}
			if page.Spend != (web.Spend{}) {
				t.Errorf("spend = %+v, want zero for an unretrieved page", page.Spend)
			}
		})
	}

	fake := exatest.NewFakeServer(exatest.WithFailedURL("https://example.com/blocked", "CRAWL_HTTP_403"))
	defer fake.Close()
	_, err := newClient(t, fake.URL()).Fetch(context.Background(), "https://example.com/blocked")
	var fe *exa.FetchError
	if !errors.As(err, &fe) || fe.Tag != "CRAWL_HTTP_403" || fe.TargetStatus != 403 {
		t.Errorf("err = %v, want CRAWL_HTTP_403 with target status 403", err)
	}
}

func TestFetchRefusesInconsistentReplies(t *testing.T) {
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
			fake := exatest.NewFakeServer(exatest.WithRawBody(tt.raw))
			defer fake.Close()
			page, err := newClient(t, fake.URL()).Fetch(context.Background(), u)
			if err == nil {
				t.Fatalf("Fetch succeeded with page %+v, want an error", page)
			}
			if page.Spend != tt.wantSpend {
				t.Errorf("spend = %+v, want %+v", page.Spend, tt.wantSpend)
			}
		})
	}
}

func TestFetchURLHygiene(t *testing.T) {
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
			fake := exatest.NewFakeServer()
			defer fake.Close()
			_, err := newClient(t, fake.URL()).Fetch(context.Background(), tt.url)
			if !errors.Is(err, web.ErrRefusedDestination) {
				t.Fatalf("err = %v, want a refused destination", err)
			}
			if strings.Contains(err.Error(), "pass") {
				t.Errorf("error echoes userinfo: %v", err)
			}
			if n := len(fake.Requests()); n != 0 {
				t.Errorf("requests = %d, want nothing sent", n)
			}
		})
	}

	exact := "https://example.com/" + strings.Repeat("a", 2048-len("https://example.com/"))
	for _, ok := range []string{"https://8.8.8.8/dns", "https://xn--fiqs8s.example.com/", "http://example.com:8080/a?b=c", "http://example.com./a", exact} {
		fake := exatest.NewFakeServer(exatest.WithPages(map[string]string{ok: "text"}))
		if _, err := newClient(t, fake.URL()).Fetch(context.Background(), ok); err != nil {
			t.Errorf("Fetch(%s): %v, want it admitted", ok, err)
		}
		fake.Close()
	}
}

func TestErrorsClassifyAccountFailures(t *testing.T) {
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
		{"payment challenge", 402, "", "X402_PAYMENT_REQUIRED", false, web.Spend{}},
		{"team blocked", 403, `{"requestId":"r1","error":"blocked","tag":"TEAM_BLOCKED"}`, "TEAM_BLOCKED", true, web.Spend{}},
		{"feature disabled", 403, `{"requestId":"r1","error":"no","tag":"FEATURE_DISABLED"}`, "FEATURE_DISABLED", false, web.Spend{}},
		{"invalid body", 400, "", "INVALID_REQUEST_BODY", false, web.Spend{}},
		{"rate limited", 429, `{"requestId":"r1","error":"slow down","tag":"RATE_LIMIT_EXCEEDED"}`, "RATE_LIMIT_EXCEEDED", false, web.Spend{}},
		{"server error", 500, `{"requestId":"r1","error":"boom","tag":"INTERNAL_ERROR"}`, "INTERNAL_ERROR", false, web.Spend{Unknown: true}},
		{"deadline", 504, `<html>gateway timeout</html>`, "", false, web.Spend{Unknown: true}},
		{"edge overload", 503, `<html>overloaded</html>`, "", false, web.Spend{Unknown: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := tt.body
			switch tt.name {
			case "payment challenge":
				body = fixture(t, "error-noauth.json")
			case "invalid body":
				body = fixture(t, "error-long-objective.json")
			}
			fake := exatest.NewFakeServer(exatest.WithStatus(tt.status, body))
			defer fake.Close()

			resp, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "q"})
			var apiErr *exa.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != tt.status || apiErr.Tag != tt.wantTag {
				t.Fatalf("err = %v, want an APIError %d %s", err, tt.status, tt.wantTag)
			}
			if got := errors.Is(err, web.ErrAccountUnusable); got != tt.wantUnusable {
				t.Errorf("errors.Is(err, ErrAccountUnusable) = %v, want %v", got, tt.wantUnusable)
			}
			if resp.Spend != tt.wantSpend {
				t.Errorf("spend = %+v, want %+v", resp.Spend, tt.wantSpend)
			}
			if len(fake.Requests()) != 1 {
				t.Errorf("requests = %d, want 1: nothing but an enveloped overload is re-sent", len(fake.Requests()))
			}
			if strings.Contains(err.Error(), "<html>") {
				t.Errorf("error echoes a non-JSON body: %v", err)
			}
		})
	}
}

func TestErrorsNeverCarryTheKeyOrRequestBody(t *testing.T) {
	const query = "the secret research question"
	const objective = "a confidential  objective"
	body := `{"requestId":"r1","error":"bad key ` + testKey + ` for x-api-key: ` + testKey +
		` while searching \"` + query + `\" towards a confidential objective","tag":"INVALID_API_KEY"}`
	fake := exatest.NewFakeServer(exatest.WithStatus(401, body))
	defer fake.Close()

	c := newClient(t, fake.URL(), func(o *exa.Options) { o.SendObjective = true })
	_, err := c.Search(context.Background(), web.Query{Text: query, Objective: objective})
	if err == nil {
		t.Fatal("Search succeeded, want an error")
	}
	for _, leak := range []string{testKey, query, "confidential objective"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
	if !strings.Contains(err.Error(), "[REDACTED:request-text]") || !strings.Contains(err.Error(), "(request r1)") {
		t.Errorf("error = %v, want the redaction marker and the request id", err)
	}
}

// TestErrorRequestIDCarryingTheKeyIsDropped: a request id that is the key
// itself is never echoed.
func TestErrorRequestIDCarryingTheKeyIsDropped(t *testing.T) {
	const key = "abc123def456"
	fake := exatest.NewFakeServer(exatest.WithStatus(500, `{"requestId":"`+key+`","error":"boom","tag":"INTERNAL_ERROR"}`))
	defer fake.Close()
	_, err := newClient(t, fake.URL(), func(o *exa.Options) { o.APIKey = key }).Search(context.Background(), web.Query{Text: "q"})
	var apiErr *exa.APIError
	if !errors.As(err, &apiErr) || apiErr.RequestID != "" || strings.Contains(err.Error(), key) {
		t.Errorf("err = %v, want the request id dropped", err)
	}
}

func TestErrorRequestIDFallsBackToHeader(t *testing.T) {
	fake := exatest.NewFakeServer(exatest.WithStatus(500, "upstream"), exatest.WithHeader("x-request-id", "abc123"))
	defer fake.Close()
	_, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "q"})
	var apiErr *exa.APIError
	if !errors.As(err, &apiErr) || apiErr.RequestID != "abc123" {
		t.Errorf("err = %v, want the header's request id", err)
	}
}

// overloadServer answers with the scripted statuses in order, then 200 with
// an empty search reply, and counts requests.
func overloadServer(t *testing.T, replies ...func(http.ResponseWriter)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		if i < len(replies) {
			replies[i](w)
			return
		}
		_, _ = w.Write([]byte(`{"results":[],"costDollars":{"total":0.007}}`))
	}))
	return srv, &n
}

func status(code int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

func TestOverloadedRequestIsSentOnceMore(t *testing.T) {
	const overloaded = `{"requestId":"r1","error":"over capacity","tag":"SERVICE_OVERLOADED"}`
	for _, tt := range []struct {
		name      string
		replies   []func(http.ResponseWriter)
		wantCalls int32
		wantErr   bool
	}{
		{"overloaded then served", []func(http.ResponseWriter){status(503, overloaded)}, 2, false},
		{"overloaded twice", []func(http.ResponseWriter){status(503, overloaded), status(503, overloaded)}, 2, true},
		{"edge 503 is not re-sent", []func(http.ResponseWriter){status(503, "<html>busy</html>")}, 1, true},
		{"other 503 tag is not re-sent", []func(http.ResponseWriter){status(503, `{"error":"x","tag":"MAINTENANCE"}`)}, 1, true},
		{"500 is not re-sent", []func(http.ResponseWriter){status(500, `{"error":"x","tag":"SERVICE_OVERLOADED"}`)}, 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, calls := overloadServer(t, tt.replies...)
			defer srv.Close()
			resp, err := newClient(t, srv.URL).Search(context.Background(), web.Query{Text: "q"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Errorf("requests = %d, want %d", got, tt.wantCalls)
			}
			if !tt.wantErr && resp.Spend != (web.Spend{USD: 0.007}) {
				t.Errorf("spend = %+v, want the served reply's cost", resp.Spend)
			}
		})
	}
}

func TestRedirectIsNeverFollowed(t *testing.T) {
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
	if err == nil {
		t.Fatal("Search followed a redirect")
	}
	if leaked.Load() {
		t.Error("the key reached the redirect target")
	}
	if strings.Contains(err.Error(), "redirect-secret") {
		t.Errorf("error echoes the redirect target's URL: %v", err)
	}
	if resp.Spend != (web.Spend{Unknown: true}) {
		t.Errorf("spend = %+v, want unknown: the request reached Exa before it redirected", resp.Spend)
	}
}

func TestOversizedBodyIsAnErrorWithUnknownSpend(t *testing.T) {
	fake := exatest.NewFakeServer(exatest.WithOversizedBody(2 << 20))
	defer fake.Close()
	resp, err := newClient(t, fake.URL()).Search(context.Background(), web.Query{Text: "q"})
	if err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("err = %v, want the body bound named", err)
	}
	if resp.Spend != (web.Spend{Unknown: true}) {
		t.Errorf("spend = %+v, want unknown for a billed reply that could not be read", resp.Spend)
	}
}

func TestTransportFailureSpend(t *testing.T) {
	t.Run("timeout after the request was sent", func(t *testing.T) {
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
		if err == nil {
			t.Fatal("Search succeeded, want a timeout")
		}
		if resp.Spend != (web.Spend{Unknown: true}) {
			t.Errorf("spend = %+v, want unknown once the request was sent", resp.Spend)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		endpoint := srv.URL
		srv.Close()
		resp, err := newClient(t, endpoint).Search(context.Background(), web.Query{Text: "q"})
		if err == nil {
			t.Fatal("Search succeeded, want a connection error")
		}
		if resp.Spend != (web.Spend{}) {
			t.Errorf("spend = %+v, want zero when nothing was sent", resp.Spend)
		}
	})
}

func TestFakeChallengesAKeylessRequest(t *testing.T) {
	fake := exatest.NewFakeServer()
	defer fake.Close()
	resp, err := http.Post(fake.URL()+"/search", "application/json", strings.NewReader(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var env exa.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if resp.StatusCode != http.StatusPaymentRequired || env.Tag != "X402_PAYMENT_REQUIRED" {
		t.Errorf("reply = %d %s, want 402 X402_PAYMENT_REQUIRED", resp.StatusCode, env.Tag)
	}
}

// TestFetchAcceptsNormalisedStatusID: the one status in a reply belongs to
// the one URL sent, even when Exa reports it in a normalised form.
func TestFetchAcceptsNormalisedStatusID(t *testing.T) {
	const sent = "https://example.com/docs"
	raw := `{"results":[{"id":"` + sent + `/","url":"` + sent + `/","text":"page text"}],` +
		`"statuses":[{"id":"` + sent + `/","status":"success"}],"costDollars":{"total":0.001}}`
	fake := exatest.NewFakeServer(exatest.WithRawBody(raw))
	defer fake.Close()

	page, err := newClient(t, fake.URL()).Fetch(context.Background(), sent)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if page.URL != sent || string(page.Content) != "page text" {
		t.Errorf("page = %s %q, want the URL as sent and the text", page.URL, page.Content)
	}
}

// TestFetchTruncationCountsUTF16Units: a page cut at the bound in UTF-16
// units, fewer runes than the bound when it holds astral characters, still
// reads as truncated.
func TestFetchTruncationCountsUTF16Units(t *testing.T) {
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
			fake := exatest.NewFakeServer(exatest.WithPages(map[string]string{u: tt.text}))
			defer fake.Close()
			page, err := newClient(t, fake.URL(), func(o *exa.Options) { o.MaxCharacters = 10 }).Fetch(context.Background(), u)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if page.Truncated != tt.want {
				t.Errorf("Truncated = %v, want %v", page.Truncated, tt.want)
			}
		})
	}
}
