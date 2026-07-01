package fleet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// newFetchPage returns a loopback httptest server serving the given text as a
// plain-text page, for the worker's web_fetch to retrieve. The worker's fetch
// client reaches it because tests build the client with AllowLoopback true.
func newFetchPage(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
}

// newBlockingModelServer returns a bare httptest server that hangs on every
// request until release is closed, then answers 500. It stands in for a model
// endpoint whose turn never completes, so a test can prove Start does not block
// on the run and Await respects its context. The server is not a model.Client
// fake — it deliberately bypasses the scripted fake to control timing.
func newBlockingModelServer(t *testing.T, release <-chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusInternalServerError)
	}))
}

// newLeakySearchServer returns an MCP search server that completes the
// handshake but rejects tools/call with a JSON-RPC error whose message echoes
// the given key — the provider-echo path on the search side. It is used to
// prove the worker scrubs a search credential out of the failed Interaction.
func newLeakySearchServer(t *testing.T, key string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(req.ID) + `,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"leaky","version":"0"}}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			// A tool-level rejection echoing the key in the error message.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(req.ID) + `,"error":{"code":-32000,"message":"auth failed for key ` + key + `"}}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
}

// itoa renders an optional JSON-RPC id as a JSON number, defaulting to 0 for a
// nil id (a notification has none).
func itoa(id *int) string {
	if id == nil {
		return "0"
	}
	return strconv.Itoa(*id)
}
