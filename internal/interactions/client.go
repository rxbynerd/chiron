package interactions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const basePath = "/v1beta/interactions"

// Defaults, all overridable via Options. The body bound is generous
// because a completed resource can carry base64 chart images; the point
// is that a misbehaving endpoint cannot exhaust memory, not that normal
// responses ever approach it.
const (
	defaultRequestTimeout = 60 * time.Second
	defaultMaxRetries     = 3
	defaultRetryBaseDelay = 500 * time.Millisecond
	defaultRetryMaxDelay  = 8 * time.Second
	defaultMaxBodyBytes   = 64 << 20
	defaultMaxEventBytes  = 16 << 20
	maxErrorBodyBytes     = 1 << 20
)

// Client is a hand-rolled net/http client for the Interactions API
// (docs/INTERACTIONS-API.md §1). The API key travels only in the
// x-goog-api-key header: it is never embedded in URLs, logged, or
// included in error text.
type Client struct {
	baseURL        string
	apiKey         string
	httpClient     *http.Client
	requestTimeout time.Duration
	maxRetries     int
	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration
	maxBodyBytes   int64
	maxEventBytes  int64
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides DefaultBaseURL — primarily for tests against
// httptest servers.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimSuffix(u, "/") }
}

// WithHTTPClient supplies the underlying *http.Client. Leave its Timeout
// zero: a global timeout would sever long-lived event streams, so unary
// deadlines come from WithRequestTimeout and streams live on their
// context instead.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithRequestTimeout bounds each unary call (Create, Get, Cancel)
// including reading the body. Streams are unaffected. Default 60s.
func WithRequestTimeout(d time.Duration) Option {
	return func(c *Client) { c.requestTimeout = d }
}

// WithMaxRetries sets how many times a transient failure (transport
// error, HTTP 408/429/5xx) is retried after the first attempt. Default 3.
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

// WithRetryDelay sets the exponential backoff's base and ceiling.
// Default 500ms doubling to an 8s cap.
func WithRetryDelay(base, ceiling time.Duration) Option {
	return func(c *Client) { c.retryBaseDelay, c.retryMaxDelay = base, ceiling }
}

// WithMaxBodyBytes bounds how much of a unary response body is read
// before failing. Default 64 MiB.
func WithMaxBodyBytes(n int64) Option {
	return func(c *Client) { c.maxBodyBytes = n }
}

// WithMaxEventBytes bounds a single SSE event. Default 16 MiB (image
// deltas carry base64 chart data).
func WithMaxEventBytes(n int64) Option {
	return func(c *Client) { c.maxEventBytes = n }
}

// New builds a Client. The key is required: failing here beats failing
// minutes into a paid run.
func New(apiKey string, opts ...Option) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("interactions: API key must not be empty")
	}
	c := &Client{
		baseURL:        DefaultBaseURL,
		apiKey:         apiKey,
		httpClient:     &http.Client{},
		requestTimeout: defaultRequestTimeout,
		maxRetries:     defaultMaxRetries,
		retryBaseDelay: defaultRetryBaseDelay,
		retryMaxDelay:  defaultRetryMaxDelay,
		maxBodyBytes:   defaultMaxBodyBytes,
		maxEventBytes:  defaultMaxEventBytes,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Create starts an interaction: POST /v1beta/interactions
// (docs/INTERACTIONS-API.md §3). Deep research must set Background: true
// and Store: true on the request.
func (c *Client) Create(ctx context.Context, req *CreateRequest) (*Interaction, error) {
	if req == nil {
		return nil, errors.New("interactions: nil create request")
	}
	if req.Background && !req.Store {
		// Background requires store (docs/INTERACTIONS-API.md §3);
		// reject locally rather than burning a request to find out.
		return nil, errors.New("interactions: background interactions require store: true")
	}
	var out Interaction
	if err := c.doJSON(ctx, http.MethodPost, basePath, nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get retrieves an interaction in its current state:
// GET /v1beta/interactions/{id}.
func (c *Client) Get(ctx context.Context, id string) (*Interaction, error) {
	path, err := interactionPath(id)
	if err != nil {
		return nil, err
	}
	var out Interaction
	if err := c.doJSON(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Cancel cancels an in-flight interaction:
// POST /v1beta/interactions/{id}/cancel. It returns the interaction as
// the API reports it after cancellation; an empty response body yields a
// zero Interaction.
func (c *Client) Cancel(ctx context.Context, id string) (*Interaction, error) {
	path, err := interactionPath(id)
	if err != nil {
		return nil, err
	}
	var out Interaction
	if err := c.doJSON(ctx, http.MethodPost, path+"/cancel", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func interactionPath(id string) (string, error) {
	if id == "" {
		return "", errors.New("interactions: interaction id must not be empty")
	}
	return basePath + "/" + url.PathEscape(id), nil
}

// doJSON performs one unary JSON exchange under the request timeout:
// marshal, send with retry, bound-read, decode.
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("interactions: encoding request: %w", err)
		}
		body = b
	}
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	resp, err := c.do(ctx, method, path, query, body, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errorFromResponse(resp)
	}
	data, err := readBounded(resp.Body, c.maxBodyBytes)
	if err != nil {
		return fmt.Errorf("interactions: reading response: %w", err)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("interactions: decoding response: %w", err)
	}
	return nil
}

// do sends one request, retrying transient failures — transport errors
// and HTTP 408/429/5xx (docs/INTERACTIONS-API.md §6) — with capped
// exponential backoff. Any other response is returned unconsumed for the
// caller to interpret. The API key is set only as a header, so it cannot
// leak through URLs or wrapped transport errors.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte, accept string) (*http.Response, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		var r io.Reader = http.NoBody
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, r)
		if err != nil {
			return nil, fmt.Errorf("interactions: building request: %w", err)
		}
		req.Header.Set("x-goog-api-key", c.apiKey)
		req.Header.Set("Api-Revision", APIRevision)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := c.httpClient.Do(req)
		switch {
		case err != nil:
			lastErr = fmt.Errorf("interactions: %s %s: %w", method, path, err)
		case retryableStatus(resp.StatusCode):
			data, _ := readBounded(resp.Body, maxErrorBodyBytes)
			resp.Body.Close()
			lastErr = parseAPIError(resp.StatusCode, data)
		default:
			return resp, nil
		}
		if attempt >= c.maxRetries {
			return nil, lastErr
		}
		select {
		case <-time.After(c.backoffDelay(attempt)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// backoffDelay is the wait before retry attempt+1: base * 2^attempt,
// capped at the ceiling.
func (c *Client) backoffDelay(attempt int) time.Duration {
	if attempt > 30 { // avoid shift overflow; far beyond any sane retry count
		return c.retryMaxDelay
	}
	d := c.retryBaseDelay << attempt
	if d <= 0 || d > c.retryMaxDelay {
		d = c.retryMaxDelay
	}
	return d
}

func errorFromResponse(resp *http.Response) error {
	data, err := readBounded(resp.Body, maxErrorBodyBytes)
	if err != nil {
		data = nil
	}
	return parseAPIError(resp.StatusCode, data)
}

// readBounded reads at most max bytes, failing — rather than silently
// truncating — if the body is larger, so a misbehaving server cannot
// exhaust memory or smuggle a clipped document through as complete.
func readBounded(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("body exceeds %d-byte bound", max)
	}
	return data, nil
}
