package interactions

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// APIError is the API's structured error: {"error":{code,message}} per
// docs/INTERACTIONS-API.md §6, plus the HTTP status it arrived with.
// Every HTTP-level failure from this package is an *APIError, so callers
// can switch on errors.As.
type APIError struct {
	// Code is a URI identifying the error type.
	Code string `json:"code,omitempty"`
	// Message is the human-readable description.
	Message string `json:"message,omitempty"`
	// HTTPStatus is the response status, or zero when the error came
	// from an SSE error event rather than an HTTP response.
	HTTPStatus int `json:"-"`
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("interactions: API error")
	if e.HTTPStatus != 0 {
		fmt.Fprintf(&b, " (HTTP %d %s)", e.HTTPStatus, http.StatusText(e.HTTPStatus))
	}
	if e.Code != "" {
		fmt.Fprintf(&b, " %s", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	return b.String()
}

// Retryable reports whether the failure is transient and worth retrying
// with backoff: 408, 429 and 5xx (docs/INTERACTIONS-API.md §6). All
// other 4xx are the caller's bug or the server's final word.
func (e *APIError) Retryable() bool {
	return retryableStatus(e.HTTPStatus)
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status >= 500
}

// parseAPIError builds an *APIError from a non-2xx response body. When
// the body is not the documented {"error":{...}} envelope, the error
// carries a bounded snippet of it instead, so the caller still sees what
// the server actually said.
func parseAPIError(status int, body []byte) *APIError {
	var envelope struct {
		Error *APIError `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != nil &&
		(envelope.Error.Code != "" || envelope.Error.Message != "") {
		e := envelope.Error
		e.HTTPStatus = status
		return e
	}
	return &APIError{HTTPStatus: status, Message: snippet(body, 256)}
}

// snippet returns at most max bytes of b as a string, truncated on a
// rune boundary, for inclusion in error messages.
func snippet(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "..."
}
