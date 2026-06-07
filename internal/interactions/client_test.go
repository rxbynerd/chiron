package interactions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const sampleInteraction = `{"id":"v1_abc","object":"interaction","agent":"deep-research-preview-04-2026","status":"in_progress","created":"2026-06-07T12:00:00Z"}`

// newTestClient wires a Client to an httptest server with fast retry
// delays. No test in this package touches the real network.
func newTestClient(t *testing.T, h http.Handler, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base := []Option{
		WithBaseURL(srv.URL),
		WithRetryDelay(time.Millisecond, 2*time.Millisecond),
	}
	c, err := New("test-api-key", append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("writing response: %v", err)
	}
}

func TestNewRequiresAPIKey(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("New(\"\") succeeded, want error")
	}
}

func TestCreateSendsWireRequest(t *testing.T) {
	var gotMethod, gotPath, gotKey, gotRevision, gotContentType string
	var gotBody map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		gotRevision = r.Header.Get("Api-Revision")
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		writeJSON(t, w, sampleInteraction)
	}))

	in, err := c.Create(context.Background(), &CreateRequest{
		Agent: AgentDeepResearch,
		Input: "Research query text",
		AgentConfig: &AgentConfig{
			Type:              AgentConfigDeepResearch,
			ThinkingSummaries: ThinkingSummariesAuto,
		},
		Background: true,
		Store:      true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if in.ID != "v1_abc" || in.Status != StatusInProgress {
		t.Errorf("decoded interaction = %+v", in)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1beta/interactions" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotKey != "test-api-key" {
		t.Errorf("x-goog-api-key = %q", gotKey)
	}
	if gotRevision != APIRevision {
		t.Errorf("Api-Revision = %q, want %q", gotRevision, APIRevision)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotBody["input"] != "Research query text" || gotBody["agent"] != AgentDeepResearch {
		t.Errorf("request body = %+v", gotBody)
	}
	for _, key := range []string{"background", "store", "stream"} {
		if _, ok := gotBody[key]; !ok {
			t.Errorf("request body missing explicit %q", key)
		}
	}
}

func TestCreateValidation(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be reached for invalid create requests")
	}))
	if _, err := c.Create(context.Background(), nil); err == nil {
		t.Error("Create(nil) succeeded, want error")
	}
	_, err := c.Create(context.Background(), &CreateRequest{Background: true, Store: false})
	if err == nil || !strings.Contains(err.Error(), "store") {
		t.Errorf("Create(background without store) error = %v, want store complaint", err)
	}
}

func TestGetAndCancelPaths(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		call       func(*Client) (*Interaction, error)
		wantMethod string
		wantPath   string
	}{
		{"get", func(c *Client) (*Interaction, error) { return c.Get(ctx, "v1_abc") },
			http.MethodGet, "/v1beta/interactions/v1_abc"},
		{"cancel", func(c *Client) (*Interaction, error) { return c.Cancel(ctx, "v1_abc") },
			http.MethodPost, "/v1beta/interactions/v1_abc/cancel"},
		{"get escapes id", func(c *Client) (*Interaction, error) { return c.Get(ctx, "v1/odd id") },
			http.MethodGet, "/v1beta/interactions/v1%2Fodd%20id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotMethod, gotPath string
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.EscapedPath()
				writeJSON(t, w, sampleInteraction)
			}))
			if _, err := tt.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			if gotMethod != tt.wantMethod || gotPath != tt.wantPath {
				t.Errorf("request = %s %s, want %s %s", gotMethod, gotPath, tt.wantMethod, tt.wantPath)
			}
		})
	}
}

func TestEmptyIDRejected(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be reached for empty ids")
	}))
	for name, call := range map[string]func() error{
		"get":    func() error { _, err := c.Get(context.Background(), ""); return err },
		"cancel": func() error { _, err := c.Cancel(context.Background(), ""); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s with empty id succeeded, want error", name)
		}
	}
}

func TestAPIErrorMapping(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"https://errors.example/not-found","message":"no such interaction"}}`)
	}))
	_, err := c.Get(context.Background(), "v1_missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want *APIError", err, err)
	}
	if apiErr.HTTPStatus != http.StatusNotFound ||
		apiErr.Code != "https://errors.example/not-found" ||
		apiErr.Message != "no such interaction" {
		t.Errorf("APIError = %+v", apiErr)
	}
	if apiErr.Retryable() {
		t.Error("404 reported as retryable")
	}
}

func TestNonJSONErrorBody(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "plain text denial")
	}))
	_, err := c.Get(context.Background(), "v1_abc")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want *APIError", err, err)
	}
	if apiErr.HTTPStatus != http.StatusForbidden || !strings.Contains(apiErr.Message, "plain text denial") {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestRetryPolicy(t *testing.T) {
	tests := []struct {
		name          string
		statuses      []int // per attempt; the last value repeats
		wantAttempts  int32
		wantErrStatus int // 0 means success expected
	}{
		{"retries 500 then succeeds", []int{500, 200}, 2, 0},
		{"retries 429 twice", []int{429, 429, 200}, 3, 0},
		{"retries 408", []int{408, 200}, 2, 0},
		{"retries 503", []int{503, 200}, 2, 0},
		{"does not retry 400", []int{400}, 1, 400},
		{"does not retry 404", []int{404}, 1, 404},
		{"exhausts retries", []int{503}, 4, 503}, // default 3 retries = 4 attempts
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := int(attempts.Add(1)) - 1
				status := tt.statuses[min(n, len(tt.statuses)-1)]
				if status == 200 {
					writeJSON(t, w, sampleInteraction)
					return
				}
				w.WriteHeader(status)
			}))
			_, err := c.Get(context.Background(), "v1_abc")
			if tt.wantErrStatus == 0 {
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
			} else {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.HTTPStatus != tt.wantErrStatus {
					t.Fatalf("error = %v, want APIError with status %d", err, tt.wantErrStatus)
				}
			}
			if got := attempts.Load(); got != tt.wantAttempts {
				t.Errorf("attempts = %d, want %d", got, tt.wantAttempts)
			}
		})
	}
}

func TestRetryOnTransportError(t *testing.T) {
	var attempts atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			panic(http.ErrAbortHandler) // sever the connection mid-response
		}
		writeJSON(t, w, sampleInteraction)
	}))
	in, err := c.Get(context.Background(), "v1_abc")
	if err != nil {
		t.Fatalf("Get after transport error: %v", err)
	}
	if in.ID != "v1_abc" {
		t.Errorf("interaction = %+v", in)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

func TestBoundedResponseRead(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"id":"`+strings.Repeat("x", 1024)+`"}`)
	}), WithMaxBodyBytes(64))
	_, err := c.Get(context.Background(), "v1_abc")
	if err == nil || !strings.Contains(err.Error(), "bound") {
		t.Errorf("error = %v, want bounded-read failure", err)
	}
}

func TestAPIKeyNeverInErrors(t *testing.T) {
	const key = "super-secret-api-key"

	// HTTP-level failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c, err := New(key, WithBaseURL(srv.URL), WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Get(context.Background(), "v1_abc"); err == nil || strings.Contains(err.Error(), key) {
		t.Errorf("HTTP error leaks key or is nil: %v", err)
	}

	// Transport-level failure: point at a closed server.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	c2, err := New(key, WithBaseURL(deadURL), WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c2.Get(context.Background(), "v1_abc"); err == nil || strings.Contains(err.Error(), key) {
		t.Errorf("transport error leaks key or is nil: %v", err)
	}
}

func TestCancelToleratesEmptyBody(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	in, err := c.Cancel(context.Background(), "v1_abc")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if in.ID != "" {
		t.Errorf("interaction = %+v, want zero value", in)
	}
}
