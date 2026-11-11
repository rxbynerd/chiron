package exa_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa/exatest"
)

// postFake sends body to the fake at path with a key and returns the reply.
func postFake(t *testing.T, fake *exatest.FakeServer, path, body string) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, fake.URL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestFakeDocumentsDecodeAsTheClientReadsThem: every document the fake
// serves decodes through the client's own envelopes with every scripted
// field intact, so the fake's wire types cannot drift from the client's.
func TestFakeDocumentsDecodeAsTheClientReadsThem(t *testing.T) {
	t.Run("search", func(t *testing.T) {
		fake := exatest.NewFakeServer(
			exatest.WithResults([]web.Result{
				{Title: "One", URL: "https://example.com/1", Snippet: "first"},
				{Title: "Two", URL: "https://example.com/2"},
			}),
			exatest.WithCostDollars(0.02),
		)
		defer fake.Close()
		env, err := exa.DecodeSearchReply(postFake(t, fake, "/search", `{"query":"q","numResults":10}`))
		if err != nil || env.Results == nil {
			t.Fatalf("decode = %+v, %v; want a results array", env, err)
		}
		got := *env.Results
		if len(got) != 2 || got[0].Title != "One" || got[0].URL != "https://example.com/1" ||
			len(got[0].Highlights) != 1 || got[0].Highlights[0] != "first" || len(got[1].Highlights) != 0 {
			t.Errorf("results = %+v, want the scripted results", got)
		}
		if env.CostDollars == nil || env.CostDollars.Total == nil || *env.CostDollars.Total != 0.02 {
			t.Errorf("cost = %+v, want 0.02", env.CostDollars)
		}
	})
	t.Run("contents", func(t *testing.T) {
		fake := exatest.NewFakeServer(
			exatest.WithPages(map[string]string{"https://example.com/a": "abcdef"}),
			exatest.WithFailedURL("https://example.com/b", "CRAWL_HTTP_403"),
		)
		defer fake.Close()
		env, err := exa.DecodeContentsReply(postFake(t, fake, "/contents",
			`{"urls":["https://example.com/a","https://example.com/b","https://example.com/c"],"text":{"maxCharacters":3}}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(env.Results) != 1 || env.Results[0].Text != "abc" || env.Results[0].URL != "https://example.com/a" {
			t.Errorf("results = %+v, want the page cut to 3 characters", env.Results)
		}
		want := []struct {
			id, status, tag string
			target          int
		}{
			{"https://example.com/a", "success", "", 0},
			{"https://example.com/b", "error", "CRAWL_HTTP_403", 403},
			{"https://example.com/c", "error", "CRAWL_NOT_FOUND", 404},
		}
		if len(env.Statuses) != len(want) {
			t.Fatalf("statuses = %+v, want %d", env.Statuses, len(want))
		}
		for i, w := range want {
			st := env.Statuses[i]
			if st.ID != w.id || st.Status != w.status {
				t.Errorf("status %d = %+v, want %s %s", i, st, w.id, w.status)
			}
			if w.tag == "" {
				continue
			}
			if st.Error == nil || st.Error.Tag != w.tag || st.Error.HTTPStatusCode == nil || *st.Error.HTTPStatusCode != w.target {
				t.Errorf("status %d error = %+v, want %s %d", i, st.Error, w.tag, w.target)
			}
		}
		if env.CostDollars == nil || env.CostDollars.Total == nil || *env.CostDollars.Total != 0.001 {
			t.Errorf("cost = %+v, want 0.001 for the one page", env.CostDollars)
		}
	})
}
