package interactions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const interactionJSON = `{"id":"v1_abc","status":"completed"}`

// TestCrossHostRedirectRefused pins C1-SEC-2: a redirect to a different
// host is refused before the request is sent, so the x-goog-api-key
// header never reaches the second server.
func TestCrossHostRedirectRefused(t *testing.T) {
	var leaked atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		if r.Header.Get("x-goog-api-key") != "" {
			t.Error("the API key header reached the redirect target")
		}
	}))
	t.Cleanup(second.Close)

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/collect", http.StatusFound)
	}))
	t.Cleanup(first.Close)

	c, err := New("test-api-key", WithBaseURL(first.URL), WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Get(context.Background(), "v1_abc"); err == nil ||
		!strings.Contains(err.Error(), "cross-origin redirect") {
		t.Errorf("Get = %v, want the cross-origin refusal", err)
	}
	if n := leaked.Load(); n != 0 {
		t.Errorf("the redirect target received %d request(s), want none", n)
	}
}

// TestSameHostRedirectFollowed: a same-host redirect is normal API
// behaviour and must still work.
func TestSameHostRedirectFollowed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1beta/interactions/v1_abc", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/moved/v1_abc", http.StatusFound)
	})
	mux.HandleFunc("/moved/v1_abc", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(interactionJSON))
	})
	c := newTestClient(t, mux, WithMaxRetries(0))
	in, err := c.Get(context.Background(), "v1_abc")
	if err != nil {
		t.Fatalf("Get across a same-host redirect: %v", err)
	}
	if in.ID != "v1_abc" {
		t.Errorf("interaction = %+v", in)
	}
}

// TestRedirectLoopRejected: even same-host redirects are capped.
func TestRedirectLoopRejected(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound) // redirect to itself
	}), WithMaxRetries(0))
	if _, err := c.Get(context.Background(), "v1_abc"); err == nil ||
		!strings.Contains(err.Error(), "too many redirects") {
		t.Errorf("Get = %v, want the redirect cap", err)
	}
}
