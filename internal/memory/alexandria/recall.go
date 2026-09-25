package alexandria

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rxbynerd/chiron/internal/httpx"
	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/secret"
)

const (
	maxErrorBodyBytes  = 4 << 10
	maxExcerptBytes    = 512
	maxRetryAfterBytes = 64
	maxTitleRunes      = 200
	maxSnippetBytes    = 4 << 10
	mediaTypeMarkdown  = "text/markdown"
	truncatedMarker    = " [truncated]"
)

// Alexandria's ref grammar: a fragment is kb://fragment/<uuid>; a source is
// kb://source/<uuid> with an optional line (#L<a>-L<b>) or character
// (#C<a>+<n>, #C<a>-C<b>) locator. A hit whose ref does not match is dropped,
// so a store cannot make an arbitrary URL citable.
const uuidPattern = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

var (
	fragmentRef = regexp.MustCompile(`^kb://fragment/(` + uuidPattern + `)$`)
	sourceRef   = regexp.MustCompile(`^kb://source/` + uuidPattern +
		`(?:#(?:L[0-9]{1,9}-L[0-9]{1,9}|C[0-9]{1,9}\+[0-9]{1,9}|C[0-9]{1,9}-C[0-9]{1,9}))?$`)
)

// SearchResponse models Alexandria's GET /v1/search response document, as
// served by FakeServer. The client reads results and degraded; unmodelled
// fields are ignored.
type SearchResponse struct {
	Results     []SearchResult `json:"results"`
	AppliedMode string         `json:"applied_mode,omitempty"`
	// Degraded is null, "embedding_unavailable" or "rerank_unavailable".
	Degraded  *string `json:"degraded"`
	Truncated bool    `json:"truncated"`
}

// SearchResult is one hit: a fragment or a source chunk.
type SearchResult struct {
	// Ref is "kb://fragment/<uuid>" or "kb://source/<uuid>#L<a>-L<b>".
	Ref       string  `json:"ref"`
	Unit      string  `json:"unit"`
	ID        string  `json:"id"`
	Space     string  `json:"space"`
	Title     string  `json:"title"`
	Kind      string  `json:"kind"`
	Snippet   string  `json:"snippet"`
	Score     float64 `json:"score"`
	Stale     bool    `json:"stale"`
	UpdatedAt string  `json:"updated_at,omitempty"`
}

// searchEnvelope distinguishes a missing or null results array from an
// empty one.
type searchEnvelope struct {
	Results  *[]SearchResult `json:"results"`
	Degraded *string         `json:"degraded"`
}

// Recall searches Alexandria for memories related to q. A non-empty ns is
// the space slug to search; an empty ns searches every space the key can
// read. At most q.Limit hits are returned (clamped to 1..20, defaulting to
// Options.DefaultLimit). The request is never retried.
func (c *Client) Recall(ctx context.Context, ns memory.Namespace, q memory.Query) ([]memory.Recalled, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, errors.New("alexandria: query text must not be empty")
	}
	if len(ns) > maxSpaceLen {
		return nil, fmt.Errorf("alexandria: namespace of %d bytes exceeds the %d-character space slug limit", len(ns), maxSpaceLen)
	}
	if ns != "" && !spaceSlug.MatchString(string(ns)) {
		return nil, fmt.Errorf("alexandria: namespace %q is not a valid space slug (lowercase letters, digits and inner hyphens)", ns)
	}

	params := url.Values{}
	params.Set("q", q.Text)
	params.Set("mode", "hybrid")
	params.Set("max_tokens", strconv.Itoa(c.maxTokens))
	if ns != "" {
		params.Set("space", string(ns))
	}

	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+searchPath+"?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("alexandria: building request: %s", c.scrub(err.Error()))
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if c.accessClientID != "" {
		req.Header.Set("cf-access-client-id", c.accessClientID)
		req.Header.Set("cf-access-client-secret", c.accessClientSecret)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alexandria: GET %s: %s", searchPath, c.scrub(err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, c.errorFromResponse(resp)
	}

	data, err := httpx.ReadAllBounded(resp.Body, c.maxBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("alexandria: reading response: %s", c.scrub(err.Error()))
	}
	env, err := decodeResponse(data)
	if err != nil {
		return nil, fmt.Errorf("alexandria: decoding response: %s", c.scrub(err.Error()))
	}

	return c.mapResults(ns, env, c.limit(q.Limit)), nil
}

// decodeResponse requires a JSON object carrying a results array.
func decodeResponse(data []byte) (searchEnvelope, error) {
	var env searchEnvelope
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return env, errors.New("response is not a JSON object")
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return env, err
	}
	if env.Results == nil {
		return env, errors.New("response has no results array")
	}
	return env, nil
}

// limit clamps a requested hit count to 1..maxLimit, defaulting a
// non-positive request to the client's default.
func (c *Client) limit(requested int) int {
	switch {
	case requested <= 0:
		return c.defaultLimit
	case requested > maxLimit:
		return maxLimit
	default:
		return requested
	}
}

// mapResults maps the first n usable hits onto memory.Recalled per
// docs/KNOWLEDGE.md §2. A hit whose ref is missing or outside Alexandria's
// ref grammar has no trustworthy handle and is skipped before the limit is
// applied. Titles and snippets are bounded here so no single hit can crowd
// out the rest.
func (c *Client) mapResults(ns memory.Namespace, env searchEnvelope, n int) []memory.Recalled {
	out := make([]memory.Recalled, 0, min(n, len(*env.Results)))
	for _, r := range *env.Results {
		if len(out) == n {
			break
		}
		loc, ok := c.locator(r.Ref)
		if !ok {
			continue
		}
		space := memory.Namespace(r.Space)
		if space == "" {
			space = ns
		}

		labels := map[string]string{}
		setLabel(labels, "kind", r.Kind)
		setLabel(labels, "unit", r.Unit)
		setLabel(labels, "space", string(space))
		setLabel(labels, "updated_at", r.UpdatedAt)
		if r.Stale {
			labels["stale"] = "true"
		}
		if env.Degraded != nil {
			setLabel(labels, "degraded", *env.Degraded)
		}

		out = append(out, memory.Recalled{
			Reference: memory.Reference{
				Namespace: space,
				Digest:    r.Ref,
				Locator:   loc,
			},
			Memory: memory.Memory{
				Text: boundBytes(r.Snippet, maxSnippetBytes),
				Meta: memory.ArtifactMeta{
					Name:      boundRunes(r.Title, maxTitleRunes),
					MediaType: mediaTypeMarkdown,
					Labels:    labels,
				},
			},
			Score: r.Score,
		})
	}
	return out
}

// locator validates ref and returns the hit's locator: the web UI route for
// a fragment ref, built from the validated UUID alone, and the ref itself for
// a source ref. ok is false for any other ref.
func (c *Client) locator(ref string) (loc string, ok bool) {
	if m := fragmentRef.FindStringSubmatch(ref); m != nil {
		return c.origin + "/f/" + m[1], true
	}
	if sourceRef.MatchString(ref) {
		return ref, true
	}
	return "", false
}

// boundBytes cuts s to at most maxBytes, marker included, on a rune boundary.
func boundBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes - len(truncatedMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMarker
}

// boundRunes cuts s to at most n runes.
func boundRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

func setLabel(labels map[string]string, key, value string) {
	if value != "" {
		labels[key] = value
	}
}

// errorFromResponse shapes a non-2xx reply. An auth failure never carries
// the body, which could echo the credential; a rate limit names the
// Retry-After value; anything else carries a bounded, scrubbed excerpt.
func (c *Client) errorFromResponse(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("alexandria: search refused: HTTP %d (check the API key and any Access service token)", resp.StatusCode)
	case http.StatusTooManyRequests:
		if ra := sanitiseHeader(resp.Header.Get("Retry-After")); ra != "" {
			return fmt.Errorf("alexandria: search rate limited: HTTP 429 (Retry-After: %s)", c.scrub(ra))
		}
		return errors.New("alexandria: search rate limited: HTTP 429")
	}
	// Only the head of an error body is reported, so it is read truncated.
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	detail := excerpt(string(data))
	if detail == "" {
		return fmt.Errorf("alexandria: search failed: HTTP %d", resp.StatusCode)
	}
	return fmt.Errorf("alexandria: search failed: HTTP %d: %s", resp.StatusCode, c.scrub(detail))
}

// excerpt collapses whitespace and bounds s to maxExcerptBytes on a rune
// boundary.
func excerpt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxExcerptBytes {
		return s
	}
	cut := maxExcerptBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// sanitiseHeader keeps printable ASCII from a server-supplied header value,
// bounded, so it cannot inject control characters into an error message.
func sanitiseHeader(v string) string {
	var b strings.Builder
	for i := 0; i < len(v) && b.Len() < maxRetryAfterBytes; i++ {
		if v[i] >= 0x20 && v[i] < 0x7f {
			b.WriteByte(v[i])
		}
	}
	return strings.TrimSpace(b.String())
}

// scrub redacts the client's own credentials by exact match, which does not
// depend on them clearing secret.Scrub's heuristics, then applies
// secret.Scrub for any other credential-shaped material.
func (c *Client) scrub(s string) string {
	if c.apiKey != "" {
		s = strings.ReplaceAll(s, c.apiKey, "[REDACTED:alexandria-api-key]")
	}
	if c.accessClientSecret != "" {
		s = strings.ReplaceAll(s, c.accessClientSecret, "[REDACTED:alexandria-access-secret]")
	}
	return secret.Scrub(s)
}
