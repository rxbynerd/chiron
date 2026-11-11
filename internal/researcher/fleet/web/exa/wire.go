package exa

// The exported types below model the parts of Exa's response documents the
// client reads, so exatest.FakeServer can serve them. Decoding is lenient:
// unmodelled fields such as entities, image, favicon, author and
// resolvedSearchType are ignored.

// SearchResponse is the body of a POST /search reply.
type SearchResponse struct {
	RequestID   string         `json:"requestId,omitempty"`
	Results     []SearchResult `json:"results"`
	Notices     []Notice       `json:"notices,omitempty"`
	CostDollars *CostDollars   `json:"costDollars,omitempty"`
}

// SearchResult is one document in a search or contents reply.
type SearchResult struct {
	ID         string   `json:"id,omitempty"`
	Title      string   `json:"title"`
	URL        string   `json:"url"`
	Highlights []string `json:"highlights,omitempty"`
	Text       string   `json:"text,omitempty"`
	Summary    string   `json:"summary,omitempty"`
}

// Notice is a message Exa addresses to the model, such as a query
// restriction it relaxed to find results.
type Notice struct {
	Code    string `json:"code,omitempty"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

// CostDollars is Exa's estimate of a request's cost in US dollars.
type CostDollars struct {
	Total float64 `json:"total"`
}

// ContentsResponse is the body of a POST /contents reply. Statuses holds one
// entry per requested URL, its ID the URL exactly as sent; a URL Exa could
// not retrieve has an error status and no entry in Results.
type ContentsResponse struct {
	RequestID   string          `json:"requestId,omitempty"`
	Results     []SearchResult  `json:"results"`
	Statuses    []ContentStatus `json:"statuses"`
	CostDollars *CostDollars    `json:"costDollars,omitempty"`
}

// ContentStatus is the outcome for one requested URL: Status is "success"
// or "error", and Error is set only for an error.
type ContentStatus struct {
	ID     string        `json:"id"`
	Status string        `json:"status"`
	Source string        `json:"source,omitempty"`
	Error  *ContentError `json:"error,omitempty"`
}

// ContentError describes why one URL could not be retrieved. HTTPStatusCode
// is the target page's status, not Exa's, and may be null.
type ContentError struct {
	Tag            string `json:"tag"`
	HTTPStatusCode *int   `json:"httpStatusCode"`
}

// ErrorResponse is the envelope of every request-level error.
type ErrorResponse struct {
	RequestID string `json:"requestId,omitempty"`
	Error     string `json:"error"`
	Tag       string `json:"tag"`
}

// searchRequest is the POST /search body.
type searchRequest struct {
	Query      string         `json:"query"`
	Type       string         `json:"type"`
	NumResults int            `json:"numResults"`
	Contents   searchContents `json:"contents"`
	Objective  string         `json:"objective,omitempty"`
}

type searchContents struct {
	Highlights highlightsOptions `json:"highlights"`
}

type highlightsOptions struct {
	MaxCharacters int `json:"maxCharacters"`
}

// contentsRequest is the POST /contents body. The text view is always
// requested with a bound, because without one Exa returns the whole page.
type contentsRequest struct {
	URLs        []string    `json:"urls"`
	Text        textOptions `json:"text"`
	MaxAgeHours int         `json:"maxAgeHours,omitempty"`
}

type textOptions struct {
	MaxCharacters int `json:"maxCharacters"`
}

// searchEnvelope and contentsEnvelope decode replies, telling a missing
// results array or cost from an empty one.
type searchEnvelope struct {
	Results     *[]SearchResult `json:"results"`
	Notices     []Notice        `json:"notices"`
	CostDollars *costEnvelope   `json:"costDollars"`
}

type contentsEnvelope struct {
	Results     []SearchResult  `json:"results"`
	Statuses    []ContentStatus `json:"statuses"`
	CostDollars *costEnvelope   `json:"costDollars"`
}

type costEnvelope struct {
	Total *float64 `json:"total"`
}
