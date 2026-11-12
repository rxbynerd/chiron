package exa_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa"
)

// TestLiveExa proves the client speaks to Exa's real API, not only to the
// fake. It runs only when CHIRON_EXA_LIVE_KEY holds an Exa API key, spends
// about one cent, and never prints the key.
func TestLiveExa(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	key := os.Getenv("CHIRON_EXA_LIVE_KEY")
	if key == "" {
		t.Skip("set CHIRON_EXA_LIVE_KEY to an Exa API key to run the live Exa test")
	}
	client, err := exa.New(exa.Options{
		Endpoint:       exa.DefaultEndpoint,
		APIKey:         key,
		RequestTimeout: 45 * time.Second,
		NumResults:     3,
		MaxCharacters:  400,
		SendObjective:  true,
	})
	must.NoError(err, "exa.New")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	res, err := client.Search(ctx, web.Query{
		Text:      "Go net/http SSRF guard dial-time pinning",
		Objective: "Find explanations of preventing SSRF in Go HTTP clients.",
	})
	must.NoError(err, "Search")
	must.NotEmpty(res.Results, "search returned no results")
	must.LessOrEqual(len(res.Results), 3, "search returned more results than requested")
	for i, r := range res.Results {
		t.Run(fmt.Sprintf("result %d", i), func(t *testing.T) {
			must := require.New(t)
			must.True(strings.HasPrefix(r.URL, "http") && r.Title != "" && r.Snippet != "",
				"result missing a field: %+v", r)
			must.True(utf8.ValidString(r.Snippet), "snippet is not valid UTF-8: %q", r.Snippet)
		})
	}
	check.False(res.Spend.Unknown, "search spend = %+v, want a known amount", res.Spend)
	check.Positive(res.Spend.USD, "search spend = %+v, want a positive amount", res.Spend)

	page, err := client.Fetch(ctx, "https://go.dev/doc/")
	must.NoError(err, "Fetch")
	check.Equal("https://go.dev/doc/", page.URL, "page URL should be the requested URL")
	n := utf8.RuneCountInString(string(page.Content))
	check.Positive(n, "page text should not be empty")
	check.LessOrEqual(n, 400, "page text is %d runes, want at most 400", n)
	check.True(page.Truncated, "a 400-rune cap on a long page should report truncation")
	check.False(page.Spend.Unknown, "fetch spend = %+v, want a known amount", page.Spend)
	check.Positive(page.Spend.USD, "fetch spend = %+v, want a positive amount", page.Spend)

	_, err = client.Fetch(ctx, "https://go.dev/this-page-does-not-exist-chiron-live-test")
	var fe *exa.FetchError
	must.ErrorAs(err, &fe, "fetch of a missing page: want *exa.FetchError, got %v", err)
	check.Equal("CRAWL_NOT_FOUND", fe.Tag, "missing page tag")
}
