package fleet

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Handlers for the loopback servers the worker tests stand up. Each test
// creates and closes its own httptest.Server around one of these, so the
// call site owns the server lifecycle.

// plainTextPage serves body as a text/plain page for the worker's web_fetch.
// The worker's fetch client reaches it because tests build the client with
// AllowLoopback true.
func plainTextPage(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
}

// blockingModel hangs on every request until release is closed, then answers
// 500. It stands in for a model endpoint whose turn never completes, so a test
// can prove Start does not block on the run and Await respects its context. It
// deliberately bypasses the scripted model fake to control timing.
func blockingModel(release <-chan struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusInternalServerError)
	})
}

// leakySearch is an MCP search server that completes the handshake but
// rejects tools/call with a JSON-RPC error whose message echoes the given key,
// the provider-echo path on the search side. It proves the worker scrubs a
// search credential out of the failed Interaction.
func leakySearch(key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})
}

// itoa renders an optional JSON-RPC id as a JSON number, defaulting to 0 for a
// nil id (a notification has none).
func itoa(id *int) string {
	if id == nil {
		return "0"
	}
	return strconv.Itoa(*id)
}
