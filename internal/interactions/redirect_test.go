package interactions

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const interactionJSON = `{"id":"v1_abc","status":"completed"}`

// TestCrossHostRedirectRefused: a redirect to a different host is refused
// before the request is sent, so the x-goog-api-key header never reaches the
// second server.
func TestCrossHostRedirectRefused(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var leaked atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		check.Empty(r.Header.Get("x-goog-api-key"), "the API key header reached the redirect target")
	}))
	t.Cleanup(second.Close)

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/collect", http.StatusFound)
	}))
	t.Cleanup(first.Close)

	c, err := New("test-api-key", WithBaseURL(first.URL), WithMaxRetries(0))
	must.NoError(err)
	_, err = c.Get(context.Background(), "v1_abc")
	must.ErrorContains(err, "cross-origin redirect")
	check.Zero(leaked.Load(), "the redirect target must receive no requests")
}

// TestNew_RedirectPolicy_OverridesCallerClient: New must impose its own
// redirect policy even when the caller supplies an http.Client that already
// carries a permissive one.
func TestNew_RedirectPolicy_OverridesCallerClient(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
	}))
	t.Cleanup(target.Close)

	var originHits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		http.Redirect(w, r, target.URL+"/collect", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	permissive := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	c, err := New("test-api-key", WithBaseURL(origin.URL), WithHTTPClient(permissive), WithMaxRetries(0))
	must.NoError(err)
	_, err = c.Get(context.Background(), "v1_abc")
	must.ErrorContains(err, "cross-origin redirect")
	check.EqualValues(1, originHits.Load(), "origin hits")
	check.Zero(targetHits.Load(), "the redirect target must receive no requests")
}

// TestNew_RedirectPolicy_RefusesHTTPSDowngrade: net/http forwards the key
// header to a same-host target whatever its scheme, so an https base
// redirecting to http on the same host is refused before a second connection
// is opened.
func TestNew_RedirectPolicy_RefusesHTTPSDowngrade(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var hits, conns atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://"+r.Host+r.URL.Path, http.StatusFound)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	c, err := New("test-api-key", WithBaseURL(server.URL), WithHTTPClient(server.Client()), WithMaxRetries(0))
	must.NoError(err)
	_, err = c.Get(context.Background(), "v1_abc")
	must.ErrorContains(err, "downgrade")
	check.EqualValues(1, hits.Load(), "handler hits")
	check.EqualValues(1, conns.Load(), "the cleartext target must never be dialled")
}

// TestSameHostRedirectFollowed: a same-host redirect is normal API behaviour
// and must still work.
func TestSameHostRedirectFollowed(t *testing.T) {
	must := require.New(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1beta/interactions/v1_abc", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/moved/v1_abc", http.StatusFound)
	})
	mux.HandleFunc("/moved/v1_abc", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(interactionJSON))
	})
	c := newTestClient(t, mux, WithMaxRetries(0))
	in, err := c.Get(context.Background(), "v1_abc")
	must.NoError(err, "Get across a same-host redirect")
	must.Equal("v1_abc", in.ID, "interaction = %+v", in)
}

// TestRedirectLoopRejected: even same-host redirects are capped.
func TestRedirectLoopRejected(t *testing.T) {
	must := require.New(t)

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound) // redirect to itself
	}), WithMaxRetries(0))
	_, err := c.Get(context.Background(), "v1_abc")
	must.ErrorContains(err, "too many redirects")
}
