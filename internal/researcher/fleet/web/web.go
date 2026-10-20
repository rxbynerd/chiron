// Package web is the seam between a research worker and the providers that
// search the web and fetch pages for it. It declares the two roles, Searcher
// and Fetcher, and the values they exchange; the adapters (the MCP search
// client, the direct fetch client, and the vendor providers under this
// package) implement them, and the composition root binds one per role.
//
// Every call returns the spend it incurred. Spend is meaningful even when the
// call also returns an error, as io.Reader's n is beside its err: a provider
// may bill a request that then fails, so a caller records Spend before it
// inspects the error. The zero Spend means nothing billable happened.
//
// Notes and every Result and Page field are untrusted provider text. Notes
// are meant for the model and are rendered to it inside the tool-result
// fence like titles and snippets.
//
// The package imports only the standard library.
package web

import (
	"context"
	"errors"
)

// Query is one web search.
type Query struct {
	// Text is the search query. It must not be blank.
	Text string
	// Objective is the broader goal the search serves. A provider may
	// ignore it, and one that can send it does so only when configured to.
	Objective string
}

// Result is one search hit.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// SearchResponse is the outcome of one search.
type SearchResponse struct {
	Results []Result
	// Notes are provider messages addressed to the model, such as a
	// restriction the provider relaxed to find results.
	Notes []string
	Spend Spend
}

// Page is a fetched document.
type Page struct {
	// URL is the final URL the provider reports, or the requested URL when
	// it reports none, with any userinfo stripped.
	URL string
	// ContentType is the media type of Content.
	ContentType string
	// Content is the page body, bounded by the provider.
	Content []byte
	// Truncated reports that the source exceeded the bound and Content is a
	// prefix.
	Truncated bool
	Spend     Spend
}

// Spend is what one call cost. The zero value means nothing was billed.
type Spend struct {
	// USD is the provider-reported cost in US dollars. It is ignored when
	// Unknown is set.
	USD float64
	// Unknown reports that the call was, or may have been, billed and the
	// amount is not known.
	Unknown bool
}

// Searcher runs a web search.
type Searcher interface {
	Search(ctx context.Context, q Query) (SearchResponse, error)
}

// Fetcher retrieves one page.
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (Page, error)
}

// ErrRefusedDestination is wrapped by every error that refuses a URL before
// or while fetching it because its destination is internal or otherwise
// unfit to fetch.
var ErrRefusedDestination = errors.New("web: refused destination")

// ErrAccountUnusable is wrapped by an error meaning the provider will refuse
// every further call on this account: a rejected key, no credit, an
// exhausted budget or a blocked team. Retrying cannot help.
var ErrAccountUnusable = errors.New("web: provider account unusable")
