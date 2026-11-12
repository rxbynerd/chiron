package exa_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa/exatest"
)

// postFake sends body to the fake at path with a key and returns the reply.
func postFake(t *testing.T, fake *exatest.FakeServer, path, body string) []byte {
	t.Helper()
	must := require.New(t)
	req, err := http.NewRequest(http.MethodPost, fake.URL()+path, strings.NewReader(body))
	must.NoError(err)
	req.Header.Set("x-api-key", testKey)
	resp, err := http.DefaultClient.Do(req)
	must.NoError(err, "POST %s", path)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	must.NoError(err)
	return data
}

// TestFakeServer_DocumentsDecodeWithClientEnvelopes: every document the fake
// serves decodes through the client's own envelopes with every scripted
// field intact, so the fake's wire types cannot drift from the client's.
func TestFakeServer_DocumentsDecodeWithClientEnvelopes(t *testing.T) {
	t.Run("search", func(t *testing.T) {
		must := require.New(t)
		fake := exatest.NewFakeServer(
			exatest.WithResults([]web.Result{
				{Title: "One", URL: "https://example.com/1", Snippet: "first"},
				{Title: "Two", URL: "https://example.com/2"},
			}),
			exatest.WithCostDollars(0.02),
		)
		defer fake.Close()
		env, err := exa.DecodeSearchReply(postFake(t, fake, "/search", `{"query":"q","numResults":10}`))
		must.NoError(err)
		must.NotNil(env.Results, "want a results array")

		got := *env.Results
		must.Len(got, 2)
		must.Equal("One", got[0].Title)
		must.Equal("https://example.com/1", got[0].URL)
		must.Len(got[0].Highlights, 1)
		must.Equal("first", got[0].Highlights[0])
		must.Empty(got[1].Highlights)

		must.NotNil(env.CostDollars)
		must.NotNil(env.CostDollars.Total)
		must.InDelta(0.02, *env.CostDollars.Total, 0)
	})

	t.Run("contents", func(t *testing.T) {
		must := require.New(t)
		fake := exatest.NewFakeServer(
			exatest.WithPages(map[string]string{"https://example.com/a": "abcdef"}),
			exatest.WithFailedURL("https://example.com/b", "CRAWL_HTTP_403"),
		)
		defer fake.Close()
		env, err := exa.DecodeContentsReply(postFake(t, fake, "/contents",
			`{"urls":["https://example.com/a","https://example.com/b","https://example.com/c"],"text":{"maxCharacters":3}}`))
		must.NoError(err)

		must.Len(env.Results, 1)
		must.Equal("abc", env.Results[0].Text)
		must.Equal("https://example.com/a", env.Results[0].URL)

		want := []struct {
			name       string
			id         string
			status     string
			tag        string
			httpStatus int
		}{
			{"successful page", "https://example.com/a", "success", "", 0},
			{"http 403 error", "https://example.com/b", "error", "CRAWL_HTTP_403", 403},
			{"not found error", "https://example.com/c", "error", "CRAWL_NOT_FOUND", 404},
		}
		must.Len(env.Statuses, len(want))
		for i, w := range want {
			t.Run(w.name, func(t *testing.T) {
				must := require.New(t)
				st := env.Statuses[i]
				must.Equal(w.id, st.ID)
				must.Equal(w.status, st.Status)
				if w.tag == "" {
					return
				}
				must.NotNil(st.Error)
				must.Equal(w.tag, st.Error.Tag)
				must.NotNil(st.Error.HTTPStatusCode)
				must.Equal(w.httpStatus, *st.Error.HTTPStatusCode)
			})
		}

		must.NotNil(env.CostDollars)
		must.NotNil(env.CostDollars.Total)
		must.InDelta(0.001, *env.CostDollars.Total, 0)
	})
}
