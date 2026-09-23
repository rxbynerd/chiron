package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// HTTPStatusError reports that the server answered with a non-2xx status.
type HTTPStatusError struct {
	Status int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("HTTP %d", e.Status)
}

// Fetch retrieves rawURL and returns its content, bounded by MaxContentBytes.
// The call, name resolution included, is bounded by RequestTimeout (a tighter
// caller deadline wins). The URL scheme must be http/https, and the
// destination must pass the SSRF guard before the request, on every redirect
// and at every dial.
//
// A refused destination wraps ErrRefusedDestination and a non-2xx response
// wraps an *HTTPStatusError; any other failure is a plain error. Fetch never
// retries. A body larger than MaxContentBytes is NOT an error: Content holds
// the bounded prefix and Page.Truncated is set.
func (c *Client) Fetch(ctx context.Context, rawURL string) (Page, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		// The raw URL may carry userinfo; scrub before echoing it.
		return Page{}, fmt.Errorf("fetch: parsing URL %q: invalid", scrubURLString(rawURL))
	}
	if err := validateScheme(u); err != nil {
		return Page{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	if err := c.guard.checkURL(ctx, u); err != nil {
		return Page{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Page{}, fmt.Errorf("fetch: building request for %s: %v", sanitizeURL(u), err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Page{}, requestError(u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Page{}, fmt.Errorf("fetch: GET %s: %w", sanitizeURL(u), &HTTPStatusError{Status: resp.StatusCode})
	}

	content, truncated, err := readCapped(resp.Body, c.maxContentBytes)
	if err != nil {
		return Page{}, fmt.Errorf("fetch: reading %s: %v", sanitizeURL(u), err)
	}

	// Report the final URL after any redirects (resp.Request.URL), sanitised.
	finalURL := sanitizeURL(u)
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = sanitizeURL(resp.Request.URL)
	}

	return Page{
		URL:         finalURL,
		ContentType: resp.Header.Get("Content-Type"),
		Content:     content,
		Truncated:   truncated,
	}, nil
}

// requestError reports a client.Do failure against the sanitised URL. The
// *url.Error layer names the request URL, userinfo included, so a refusal is
// re-wrapped from the cause beneath it, whose message holds only a host and
// an address; any other failure is flattened and scrubbed.
func requestError(u *url.URL, err error) error {
	cause := err
	var ue *url.Error
	if errors.As(err, &ue) {
		cause = ue.Err
	}
	if errors.Is(cause, ErrRefusedDestination) {
		return fmt.Errorf("fetch: GET %s: %w", sanitizeURL(u), cause)
	}
	return fmt.Errorf("fetch: GET %s: %s", sanitizeURL(u), scrubURLString(err.Error()))
}

// readCapped reads up to max bytes and reports whether the source held more.
// Unlike the model/search readBounded — which errors on an oversize body — a
// truncated page is an acceptable, marked result for web_fetch: it reads
// max+1 to detect the overflow, then returns the first max bytes with
// truncated=true. A genuine read error is still surfaced.
func readCapped(r io.Reader, max int64) (content []byte, truncated bool, err error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > max {
		return data[:max], true, nil
	}
	return data, false, nil
}
