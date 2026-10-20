package exa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/rxbynerd/chiron/internal/httpx"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
	"github.com/rxbynerd/chiron/internal/secret"
	"github.com/rxbynerd/chiron/internal/types"
)

const (
	// maxErrorBodyBytes bounds the error body read for its envelope. Exa's
	// payment challenge is about 6 KiB.
	maxErrorBodyBytes = 8 << 10
	maxMessageBytes   = 512
	tagOverloaded     = "SERVICE_OVERLOADED"
	// minRedactedBytes is the shortest request text redacted from an error
	// message; replacing a shorter one would mangle the message for little
	// gain.
	minRedactedBytes = 4
)

// tagPattern and requestIDPattern admit the server-supplied identifiers an
// error carries; anything else is dropped rather than echoed.
var (
	tagPattern       = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// APIError is a non-2xx reply from Exa. Tag is Exa's machine-readable error
// tag, Message its bounded and scrubbed human message, and RequestID the id
// Exa assigned; each is empty when the reply carried none, as for a
// non-JSON body from Exa's edge.
type APIError struct {
	Status    int
	Tag       string
	Message   string
	RequestID string
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "exa: HTTP %d", e.Status)
	if e.Tag != "" {
		b.WriteString(" " + e.Tag)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request %s)", e.RequestID)
	}
	return b.String()
}

// Is matches web.ErrAccountUnusable for the replies after which every call
// on the account fails: a rejected key (401), no credit or an exhausted key
// or team budget (402), and a blocked team (403). A 402 payment challenge
// (X402_PAYMENT_REQUIRED) means no key was sent, a client fault, so it does
// not match.
func (e *APIError) Is(target error) bool {
	if target != web.ErrAccountUnusable {
		return false
	}
	switch e.Status {
	case http.StatusUnauthorized:
		return true
	case http.StatusPaymentRequired:
		switch e.Tag {
		case "NO_MORE_CREDITS", "API_KEY_BUDGET_EXCEEDED", "TEAM_BUDGET_EXCEEDED":
			return true
		}
	case http.StatusForbidden:
		return e.Tag == "TEAM_BLOCKED"
	}
	return false
}

// post sends payload to path and returns the 2xx body. Every outcome carries
// the spend it implies: zero when nothing can have been billed, Unknown when
// the request may have been billed and its cost is unreadable. A 503 with
// Exa's SERVICE_OVERLOADED envelope is re-sent once; nothing else is. Each
// of redact, request text the payload carries, is removed from an error
// message Exa returns.
func (c *Client) post(ctx context.Context, path string, payload []byte, redact ...string) ([]byte, web.Spend, error) {
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	data, spend, overloaded, err := c.attempt(ctx, path, payload, redact)
	if overloaded {
		data, spend, _, err = c.attempt(ctx, path, payload, redact)
	}
	return data, spend, err
}

// attempt makes one request. overloaded reports a 503 that Exa says it did
// not process.
func (c *Client) attempt(ctx context.Context, path string, payload []byte, redact []string) (data []byte, spend web.Spend, overloaded bool, err error) {
	// The transport reports the write from its own goroutine.
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wrote.Store(true) },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return nil, web.Spend{}, false, fmt.Errorf("exa: building request: %s", c.scrub(err.Error()))
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A request that reached Exa may have been processed and billed.
		// The *url.Error layer names the last URL requested, which for a
		// refused redirect is the redirect target, so only its cause is kept.
		cause := err
		var ue *url.Error
		if errors.As(err, &ue) {
			cause = ue.Err
		}
		return nil, web.Spend{Unknown: wrote.Load()}, false, fmt.Errorf("exa: POST %s: %s", path, c.scrub(cause.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr, enveloped := c.errorFromResponse(resp, redact)
		overloaded = resp.StatusCode == http.StatusServiceUnavailable && enveloped && apiErr.Tag == tagOverloaded
		return nil, errorSpend(resp.StatusCode, overloaded), overloaded, apiErr
	}
	data, err = httpx.ReadAllBounded(resp.Body, c.maxBodyBytes)
	if err != nil {
		return nil, web.Spend{Unknown: true}, false, fmt.Errorf("exa: reading %s response: %s", path, c.scrub(err.Error()))
	}
	return data, web.Spend{}, false, nil
}

// errorSpend is the spend a non-2xx reply implies. A 4xx rejects the request
// before work, and Exa documents its SERVICE_OVERLOADED 503 as not billed;
// any other 5xx, an edge proxy's 503 included, may follow work already done.
func errorSpend(status int, overloaded bool) web.Spend {
	return web.Spend{Unknown: status >= 500 && !overloaded}
}

// errorFromResponse builds the APIError for a non-2xx reply from a bounded
// read of its body. enveloped reports that the body was Exa's JSON error
// envelope; a body that is not (an edge proxy's HTML page, say) contributes
// nothing but the status. The message has the key and each of redact
// removed, and the request id is dropped unless it survives the key scrub
// unchanged.
func (c *Client) errorFromResponse(resp *http.Response, redact []string) (*APIError, bool) {
	apiErr := &APIError{Status: resp.StatusCode}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	var env ErrorResponse
	trimmed := bytes.TrimSpace(data)
	enveloped := len(trimmed) > 0 && trimmed[0] == '{' && json.Unmarshal(trimmed, &env) == nil && env.Tag != ""
	if enveloped {
		if tagPattern.MatchString(env.Tag) {
			apiErr.Tag = env.Tag
		}
		apiErr.Message = boundBytes(c.scrub(redactText(env.Error, redact)), maxMessageBytes)
		apiErr.RequestID = env.RequestID
	}
	if !c.validRequestID(apiErr.RequestID) {
		apiErr.RequestID = resp.Header.Get("x-request-id")
	}
	if !c.validRequestID(apiErr.RequestID) {
		apiErr.RequestID = ""
	}
	return apiErr, enveloped
}

// validRequestID reports whether id has the shape of a request id and is
// unchanged by the key scrub.
func (c *Client) validRequestID(id string) bool {
	return requestIDPattern.MatchString(id) && c.scrub(id) == id
}

// redactText flattens msg to one line and replaces each of redact, as sent
// and as flattened, when it is at least minRedactedBytes long.
func redactText(msg string, redact []string) string {
	for _, r := range redact {
		if len(r) >= minRedactedBytes {
			msg = strings.ReplaceAll(msg, r, "[REDACTED:request-text]")
		}
	}
	msg = oneLine(msg)
	for _, r := range redact {
		if r = oneLine(r); len(r) >= minRedactedBytes {
			msg = strings.ReplaceAll(msg, r, "[REDACTED:request-text]")
		}
	}
	return msg
}

// spendFrom reads a 200 reply's reported cost, rounded to the micro-dollar.
// A missing or unusable figure is Unknown, never zero: the reply was billed.
func spendFrom(cost *costEnvelope) web.Spend {
	if cost == nil || cost.Total == nil {
		return web.Spend{Unknown: true}
	}
	total := *cost.Total
	if math.IsNaN(total) || math.IsInf(total, 0) || total < 0 {
		return web.Spend{Unknown: true}
	}
	return web.Spend{USD: types.RoundUSD(total)}
}

// scrub redacts the key by exact match, which does not depend on it
// clearing secret.Scrub's heuristics, then applies secret.Scrub.
func (c *Client) scrub(s string) string {
	if c.apiKey != "" {
		s = strings.ReplaceAll(s, c.apiKey, "[REDACTED:exa-api-key]")
	}
	return secret.Scrub(s)
}

// oneLine collapses s to one whitespace-normalised line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// boundBytes cuts s to at most n bytes on a rune boundary.
func boundBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
