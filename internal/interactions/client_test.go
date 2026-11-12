package interactions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/httpx"
)

const sampleInteraction = `{"id":"v1_abc","object":"interaction","agent":"deep-research-preview-04-2026","status":"in_progress","created":"2026-06-07T12:00:00Z"}`

// newTestClient wires a Client to an httptest server with fast retry
// delays. No test in this package touches the real network.
func newTestClient(t *testing.T, h http.Handler, opts ...Option) *Client {
	t.Helper()
	must := require.New(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base := []Option{
		WithBaseURL(srv.URL),
		WithRetryDelay(time.Millisecond, 2*time.Millisecond),
	}
	c, err := New("test-api-key", append(base, opts...)...)
	must.NoError(err)
	return c
}

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	check := assert.New(t)
	w.Header().Set("Content-Type", "application/json")
	_, err := io.WriteString(w, body)
	check.NoError(err, "writing response")
}

func TestNew_RequiresAPIKey(t *testing.T) {
	must := require.New(t)
	_, err := New("")
	must.Error(err)
}

// TestNew_ValidatesBaseURL: the effective base URL, default or overridden,
// must satisfy httpx.ParseEndpoint before the client exists, and the error
// never echoes the rejected value.
func TestNew_ValidatesBaseURL(t *testing.T) {
	const secret = "Winter2026-credential"
	tests := []struct {
		name    string
		opts    []Option
		wantErr string // "" means accepted
	}{
		{"default", nil, ""},
		{"https override", []Option{WithBaseURL("https://gemini-proxy.example.com/")}, ""},
		{"loopback http override", []Option{WithBaseURL("http://127.0.0.1:9999")}, ""},
		{"empty override", []Option{WithBaseURL("")}, "must not be empty"},
		{"cleartext non-loopback", []Option{WithBaseURL("http://evil.example.com/" + secret)}, "absolute https://"},
		{"userinfo", []Option{WithBaseURL("https://user:" + secret + "@gemini.example.com")}, "userinfo"},
		{"query", []Option{WithBaseURL("https://gemini.example.com/?key=" + secret)}, "query or fragment"},
		{"fragment", []Option{WithBaseURL("https://gemini.example.com/#" + secret)}, "query or fragment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			c, err := New("test-api-key", tt.opts...)
			if tt.wantErr == "" {
				must.NoError(err)
				must.NotNil(c)
				return
			}
			must.Error(err)
			must.True(strings.HasPrefix(err.Error(), "interactions: base URL "), "error %q lacks the base URL prefix", err.Error())
			must.ErrorContains(err, tt.wantErr)
			must.ErrorIs(err, httpx.ErrInvalidEndpoint)
			must.NotContains(err.Error(), secret, "error must not echo the rejected value")
		})
	}
}

func TestCreateSendsWireRequest(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var gotMethod, gotPath, gotKey, gotRevision, gotContentType string
	var gotBody map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		gotRevision = r.Header.Get("Api-Revision")
		gotContentType = r.Header.Get("Content-Type")
		err := json.NewDecoder(r.Body).Decode(&gotBody)
		check.NoError(err, "decoding request body")
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
	must.NoError(err)
	check.Equal("v1_abc", in.ID, "interaction id")
	check.Equal(StatusInProgress, in.Status, "interaction status")
	check.Equal(http.MethodPost, gotMethod, "request method")
	check.Equal("/v1beta/interactions", gotPath, "request path")
	check.Equal("test-api-key", gotKey, "x-goog-api-key header")
	check.Equal(APIRevision, gotRevision, "Api-Revision header")
	check.Equal("application/json", gotContentType, "Content-Type header")
	check.Equal("Research query text", gotBody["input"], "request input")
	check.Equal(AgentDeepResearch, gotBody["agent"], "request agent")
	for _, key := range []string{"background", "store", "stream"} {
		check.Contains(gotBody, key, "request body must set %q explicitly", key)
	}
}

func TestCreateValidation(t *testing.T) {
	check := assert.New(t)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Fail("server should not be reached for invalid create requests")
	}))

	t.Run("nil request", func(t *testing.T) {
		must := require.New(t)
		_, err := c.Create(context.Background(), nil)
		must.Error(err)
	})
	t.Run("background without store", func(t *testing.T) {
		must := require.New(t)
		_, err := c.Create(context.Background(), &CreateRequest{Background: true, Store: false})
		must.ErrorContains(err, "store")
	})
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
			must := require.New(t)
			var gotMethod, gotPath string
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.EscapedPath()
				writeJSON(t, w, sampleInteraction)
			}))
			_, err := tt.call(c)
			must.NoError(err)
			must.Equal(tt.wantMethod, gotMethod, "method")
			must.Equal(tt.wantPath, gotPath, "path")
		})
	}
}

func TestClient_RejectsEmptyID(t *testing.T) {
	check := assert.New(t)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Fail("server should not be reached for empty ids")
	}))
	tests := []struct {
		name string
		call func() error
	}{
		{"get", func() error { _, err := c.Get(context.Background(), ""); return err }},
		{"cancel", func() error { _, err := c.Cancel(context.Background(), ""); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			err := tt.call()
			must.Error(err)
		})
	}
}

func TestAPIError_MapsWireBody(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"https://errors.example/not-found","message":"no such interaction"}}`)
	}))
	_, err := c.Get(context.Background(), "v1_missing")
	var apiErr *APIError
	must.ErrorAs(err, &apiErr)
	check.Equal(http.StatusNotFound, apiErr.HTTPStatus, "HTTPStatus")
	check.Equal("https://errors.example/not-found", apiErr.Code, "Code")
	check.Equal("no such interaction", apiErr.Message, "Message")
	check.False(apiErr.Retryable(), "a 404 must not be retryable")
}

func TestNonJSONErrorBody(t *testing.T) {
	must := require.New(t)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "plain text denial")
	}))
	_, err := c.Get(context.Background(), "v1_abc")
	var apiErr *APIError
	must.ErrorAs(err, &apiErr)
	must.Equal(http.StatusForbidden, apiErr.HTTPStatus)
	must.Contains(apiErr.Message, "plain text denial")
}

func TestRetry_ByStatus(t *testing.T) {
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
			must := require.New(t)
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
				must.NoError(err)
			} else {
				var apiErr *APIError
				must.ErrorAs(err, &apiErr)
				must.Equal(tt.wantErrStatus, apiErr.HTTPStatus)
			}
			must.Equal(tt.wantAttempts, attempts.Load(), "attempts")
		})
	}
}

func TestRetry_OnTransportError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var attempts atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			panic(http.ErrAbortHandler) // sever the connection mid-response
		}
		writeJSON(t, w, sampleInteraction)
	}))
	in, err := c.Get(context.Background(), "v1_abc")
	must.NoError(err)
	check.Equal("v1_abc", in.ID, "interaction id")
	check.Equal(int32(2), attempts.Load(), "attempts")
}

func TestClient_BoundsResponseRead(t *testing.T) {
	must := require.New(t)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"id":"`+strings.Repeat("x", 1024)+`"}`)
	}), WithMaxBodyBytes(64))
	_, err := c.Get(context.Background(), "v1_abc")
	must.ErrorContains(err, "bound")
}

func TestAPIKeyNeverInErrors(t *testing.T) {
	const key = "super-secret-api-key"

	t.Run("http failure", func(t *testing.T) {
		must := require.New(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		c, err := New(key, WithBaseURL(srv.URL), WithMaxRetries(0))
		must.NoError(err)
		_, err = c.Get(context.Background(), "v1_abc")
		must.Error(err)
		must.NotContains(err.Error(), key, "error must not leak the API key")
	})

	t.Run("transport failure", func(t *testing.T) {
		must := require.New(t)
		dead := httptest.NewServer(http.NotFoundHandler())
		deadURL := dead.URL
		dead.Close()
		c, err := New(key, WithBaseURL(deadURL), WithMaxRetries(0))
		must.NoError(err)
		_, err = c.Get(context.Background(), "v1_abc")
		must.Error(err)
		must.NotContains(err.Error(), key, "error must not leak the API key")
	})
}

func TestCancel_ToleratesEmptyBody(t *testing.T) {
	must := require.New(t)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	in, err := c.Cancel(context.Background(), "v1_abc")
	must.NoError(err)
	must.Empty(in.ID, "interaction must be the zero value")
}

// An oversize non-JSON error body is clipped from the front to the snippet
// bound: the message keeps the head of the body, drops the tail, and marks the cut.
func TestAPIError_TruncatesNonJSONBody(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	long := strings.Repeat("front-", 50) + "TAIL-MARKER" // 311 bytes, > the 256 snippet bound
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, long)
	}))
	_, err := c.Get(context.Background(), "v1_abc")
	var apiErr *APIError
	must.ErrorAs(err, &apiErr)
	check.LessOrEqual(len(apiErr.Message), 256+len("..."), "message must be clipped to the 256-byte snippet bound")
	check.True(strings.HasPrefix(apiErr.Message, "front-"), "message must carry the head of the body")
	check.NotContains(apiErr.Message, "TAIL-MARKER", "message must not carry tail-only content past the bound")
	check.True(strings.HasSuffix(apiErr.Message, "..."), "message must mark the truncation")
}

// Requests go through the http.Client supplied via WithHTTPClient.
func TestWithHTTPClient_IsUsed(t *testing.T) {
	must := require.New(t)
	var used atomic.Int64
	base := http.DefaultTransport
	custom := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		used.Add(1)
		return base.RoundTrip(req)
	})}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"v1_abc","status":"completed"}`))
	}), WithHTTPClient(custom))
	_, err := c.Get(context.Background(), "v1_abc")
	must.NoError(err)
	must.NotZero(used.Load(), "the request bypassed the client supplied via WithHTTPClient")
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// A unary call against a stalled server fails with DeadlineExceeded at the
// request timeout instead of hanging.
func TestWithRequestTimeout_BoundsUnaryCalls(t *testing.T) {
	must := require.New(t)
	release := make(chan struct{})
	defer close(release)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // stall until the test ends
	}), WithRequestTimeout(20*time.Millisecond), WithMaxRetries(0))
	_, err := c.Get(context.Background(), "v1_abc")
	must.ErrorIs(err, context.DeadlineExceeded)
}
