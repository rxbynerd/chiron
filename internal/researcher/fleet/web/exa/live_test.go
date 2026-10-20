package exa_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web/exa"
)

// TestLiveExa proves the client speaks to Exa's real API, not only to the
// fake. It runs only when CHIRON_EXA_LIVE_KEY holds an Exa API key, spends
// about one cent, and never prints the key.
func TestLiveExa(t *testing.T) {
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
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	res, err := client.Search(ctx, web.Query{
		Text:      "Go net/http SSRF guard dial-time pinning",
		Objective: "Find explanations of preventing SSRF in Go HTTP clients.",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Results) == 0 || len(res.Results) > 3 {
		t.Fatalf("Search returned %d results, want 1..3", len(res.Results))
	}
	for _, r := range res.Results {
		if !strings.HasPrefix(r.URL, "http") || r.Title == "" || r.Snippet == "" {
			t.Errorf("result missing a field: %+v", r)
		}
		if len(r.Snippet) > 1000 {
			t.Errorf("snippet of %d bytes exceeds the adapter bound", len(r.Snippet))
		}
	}
	if res.Spend.Unknown || res.Spend.USD <= 0 {
		t.Errorf("search spend = %+v, want a known positive amount", res.Spend)
	}

	page, err := client.Fetch(ctx, "https://go.dev/doc/")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if page.URL != "https://go.dev/doc/" {
		t.Errorf("page URL = %q, want the requested URL", page.URL)
	}
	if n := utf8.RuneCountInString(string(page.Content)); n == 0 || n > 400 {
		t.Errorf("page text is %d runes, want 1..400", n)
	}
	if !page.Truncated {
		t.Error("a 400-rune cap on a long page should report truncation")
	}
	if page.Spend.Unknown || page.Spend.USD <= 0 {
		t.Errorf("fetch spend = %+v, want a known positive amount", page.Spend)
	}

	_, err = client.Fetch(ctx, "https://go.dev/this-page-does-not-exist-chiron-live-test")
	var fe *exa.FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("Fetch of a missing page: err = %v, want *exa.FetchError", err)
	}
	if fe.Tag != "CRAWL_NOT_FOUND" {
		t.Errorf("missing page tag = %q, want CRAWL_NOT_FOUND", fe.Tag)
	}
}
