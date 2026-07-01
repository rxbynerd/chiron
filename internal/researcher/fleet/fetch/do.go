package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Fetch retrieves rawURL and returns its content, bounded by MaxContentBytes.
// The call is bounded by RequestTimeout (a tighter caller deadline wins). The
// URL scheme must be http/https, and the destination host must pass the SSRF
// guard both before the request and after every redirect.
//
// A reader-side error (dial/read failure) is returned to the caller; reads are
// idempotent, so a caller may retry knowingly, but Fetch itself does not retry
// — keeping the cost and time ceiling of one call obvious. A body larger than
// MaxContentBytes is NOT an error: Content holds the bounded prefix and
// Page.Truncated is set (docs/DECISIONS.md), because partial page text is
// useful research input.
func (c *Client) Fetch(ctx context.Context, rawURL string) (Page, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		// The raw URL may carry userinfo; scrub before echoing it.
		return Page{}, fmt.Errorf("fetch: parsing URL %q: invalid", scrubURLString(rawURL))
	}
	if err := validateScheme(u); err != nil {
		return Page{}, err
	}
	if err := guardHost(u, c.allowLoopback); err != nil {
		return Page{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Page{}, fmt.Errorf("fetch: building request for %s: %v", sanitizeURL(u), err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A transport/redirect error can wrap the request URL (with userinfo);
		// scrub it before surfacing.
		return Page{}, fmt.Errorf("fetch: GET %s: %s", sanitizeURL(u), scrubURLString(err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Page{}, fmt.Errorf("fetch: GET %s: HTTP %d", sanitizeURL(u), resp.StatusCode)
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
