package interactions

import (
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/httpx"
)

// loopbackOnlyTransport fails any request to a host that is not loopback, so
// a test that forgets to point a client at an httptest server cannot reach a
// real service.
type loopbackOnlyTransport struct {
	next http.RoundTripper
}

func (l loopbackOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !httpx.LoopbackHost(req.URL.Hostname()) {
		return nil, fmt.Errorf("tests must not contact a non-loopback host: %s", req.URL.Host)
	}
	return l.next.RoundTrip(req)
}

// TestMain installs the guard on the default transport, which clients built
// with no transport of their own use.
func TestMain(m *testing.M) {
	http.DefaultTransport = loopbackOnlyTransport{next: http.DefaultTransport}
	os.Exit(m.Run())
}

// TestDefaultTransport_RefusesNonLoopbackHosts: the guard refuses before any
// connection is attempted.
func TestDefaultTransport_RefusesNonLoopbackHosts(t *testing.T) {
	must := require.New(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, DefaultBaseURL, nil)
	must.NoError(err)
	resp, err := http.DefaultClient.Do(req)
	if resp != nil {
		must.NoError(resp.Body.Close())
	}
	must.ErrorContains(err, "non-loopback host")
}
