package mcpclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/mcpclient"
	"github.com/rxbynerd/chiron/internal/mcpclient/mcpclienttest"
)

// decodeRPC reads and decodes one JSON-RPC request body in full. It runs on
// server goroutines, so it reports with assert rather than require.
func decodeRPC(t *testing.T, r *http.Request) rpcRequest {
	t.Helper()
	check := assert.New(t)
	var req rpcRequest
	body, err := io.ReadAll(r.Body)
	if !check.NoError(err, "reading request body") {
		return req
	}
	check.NoError(json.Unmarshal(body, &req), "decoding request body")
	return req
}

// writeInitialize answers initialize with the client's protocol version,
// issuing sessionID when it is non-empty.
func writeInitialize(w http.ResponseWriter, id int, sessionID string) {
	if sessionID != "" {
		w.Header().Set("Mcp-Session-Id", sessionID)
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q}}`, id, mcpclient.ProtocolVersion)
}

// writeToolText answers a tools/call with one text block.
func writeToolText(w http.ResponseWriter, id int, text string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%q}]}}`, id, text)
}

// awaitSignal fails the test if ch is not closed within five seconds.
func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	must := require.New(t)
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		must.Fail("timed out waiting for " + what)
	}
}

// awaitErr returns the error sent on errc, failing the test if none arrives
// within five seconds.
func awaitErr(t *testing.T, errc <-chan error, what string) error {
	t.Helper()
	must := require.New(t)
	select {
	case err := <-errc:
		return err
	case <-time.After(5 * time.Second):
		must.Fail("timed out waiting for " + what)
		return nil
	}
}

// deleteLog records the session named by each DELETE a test server receives.
type deleteLog struct {
	mu  sync.Mutex
	ids []string
}

// record logs r's session and answers 204.
func (d *deleteLog) record(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.ids = append(d.ids, r.Header.Get("Mcp-Session-Id"))
	d.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// sessions returns the sessions DELETEd so far, in arrival order.
func (d *deleteLog) sessions() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.ids)
}

func TestCallTool_ReusesSession(t *testing.T) {
	// One handshake serves every call on a Client, and every request id is
	// new: MCP forbids reusing one within a session.
	tests := []struct {
		name    string
		opts    []mcpclienttest.FakeOption
		session string
	}{
		{"stateless", nil, ""},
		{"stateful", []mcpclienttest.FakeOption{mcpclienttest.WithSessionID("sess-reuse")}, "sess-reuse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := mcpclienttest.NewFakeServer(textResult("ok"), tt.opts...)
			defer fake.Close()

			c := newClient(t, fake.URL())
			for range 3 {
				_, err := call(c)
				must.NoError(err)
			}
			must.Equal(1, fake.InitializeCount())
			must.Equal(3, fake.ToolCallCount())
			must.Zero(fake.DeleteCount(), "DELETE count before Close")

			reqs := fake.Requests()
			must.Len(reqs, 5, "want initialize, initialized, and three tools/call requests")
			for i, r := range reqs[1:] {
				must.Equal(tt.session, r.SessionID, "request[%d] session", i+1)
				must.Equal(mcpclient.ProtocolVersion, r.ProtocolVersion, "request[%d] protocol version", i+1)
			}
			seen := map[int]bool{}
			for _, r := range reqs {
				if r.Method == "notifications/initialized" {
					continue
				}
				must.NotZero(r.ID, "%s request id", r.Method)
				must.NotContains(seen, r.ID, "%s request id %d is reused", r.Method, r.ID)
				seen[r.ID] = true
			}
		})
	}
}

func TestCallTool_ConcurrentCallsShareOneHandshake(t *testing.T) {
	// Callers arriving while the handshake is in flight wait for it rather
	// than start their own.
	must := require.New(t)
	check := assert.New(t)
	const callers = 8
	var inits, toolCalls atomic.Int32
	arrived := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRPC(t, r)
		switch req.Method {
		case "initialize":
			if inits.Add(1) == 1 {
				close(arrived)
			}
			<-release
			writeInitialize(w, deref(req.ID), "sess-shared")
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			toolCalls.Add(1)
			writeToolText(w, deref(req.ID), "ok")
		}
	}))
	defer server.Close()
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()

	c := newClient(t, server.URL)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			_, err := call(c)
			errs <- err
		}()
	}
	awaitSignal(t, arrived, "the first initialize")
	// The pause only widens the window in which callers join the pending
	// handshake; the assertions hold however the callers interleave.
	time.Sleep(20 * time.Millisecond)
	releaseOnce()
	for range callers {
		must.NoError(<-errs)
	}
	check.EqualValues(1, inits.Load())
	check.EqualValues(callers, toolCalls.Load())
}

func TestCallTool_ReinitialisesAfterSessionExpiry(t *testing.T) {
	// A 404 on a request bearing the session id means the server dropped the
	// session without processing the call: one re-initialisation, one resend,
	// and no DELETE for the session that is already gone.
	must := require.New(t)
	check := assert.New(t)
	fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithSessionID("sess-x"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := call(c)
	must.NoError(err)
	fake.ExpireSession()
	got, err := call(c)
	must.NoError(err)
	check.Equal("ok", mcpclient.FirstText(got.Content), "result %+v", got)

	want := []struct{ method, session string }{
		{"initialize", ""},
		{"notifications/initialized", "sess-x"},
		{"tools/call", "sess-x"},
		{"tools/call", "sess-x"},
		{"initialize", ""},
		{"notifications/initialized", "sess-x-2"},
		{"tools/call", "sess-x-2"},
	}
	reqs := fake.Requests()
	must.Len(reqs, len(want), "requests: %+v", reqs)
	for i, w := range want {
		check.Equal(w.method, reqs[i].Method, "request[%d] method", i)
		check.Equal(w.session, reqs[i].SessionID, "request[%d] session", i)
	}

	must.NoError(c.Close())
	check.Equal(1, fake.DeleteCount())
	all := fake.Requests()
	must.Greater(len(all), len(want), "no DELETE request was recorded")
	last := all[len(want)]
	check.Equal(http.MethodDelete, last.HTTPMethod, "last request method")
	check.Equal("sess-x-2", last.SessionID, "last request session")
}

func TestCallTool_ConcurrentCallsReinitialiseOnce(t *testing.T) {
	// Callers that all find the session gone share one re-initialisation; a
	// caller still holding the stale session never discards the fresh one.
	must := require.New(t)
	check := assert.New(t)
	const callers = 8
	fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithSessionID("sess-old"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := call(c)
	must.NoError(err)
	fake.ExpireSession()

	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			_, err := call(c)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must.NoError(err)
	}
	check.Equal(2, fake.InitializeCount(), "the first session and one re-initialisation")
	// Each caller sends its call once, plus once more if it held the expired
	// session; at least one did.
	n := fake.ToolCallCount()
	check.GreaterOrEqual(n, callers+2)
	check.LessOrEqual(n, 2*callers+1)
}

func TestCallTool_RepeatedSessionExpiryFails(t *testing.T) {
	// A second 404 in one call fails it rather than looping, and the session
	// is dropped so the next call starts afresh, under the same bound.
	must := require.New(t)
	check := assert.New(t)
	var inits, toolCalls, deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		req := decodeRPC(t, r)
		switch req.Method {
		case "initialize":
			writeInitialize(w, deref(req.ID), fmt.Sprintf("sess-gone-%d", inits.Add(1)))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			toolCalls.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newClient(t, server.URL)
	for i := int32(1); i <= 2; i++ {
		_, err := call(c)
		must.ErrorContains(err, "tools/call failed: HTTP 404", "call %d", i)
		check.NotContains(err.Error(), "sess-gone", "error carries the session id")
		check.Equal(2*i, inits.Load(), "after call %d initialize count", i)
		check.Equal(2*i, toolCalls.Load(), "after call %d tools/call count", i)
	}
	check.Zero(deletes.Load(), "DELETE count for sessions the server already dropped")
}

func TestCallTool_FailedReinitialisationAfterSessionExpiry(t *testing.T) {
	// When the handshake after a session 404 fails, CallTool returns that
	// failure rather than the 404, does not re-send the call, and ends any
	// session the failed handshake was issued. The next call starts afresh.
	tests := []struct {
		name        string
		retryInit   func(w http.ResponseWriter, id int)
		wantErr     string
		wantDeletes []string
	}{
		{
			name:      "http 500",
			retryInit: func(w http.ResponseWriter, _ int) { w.WriteHeader(http.StatusInternalServerError) },
			wantErr:   "initialize failed: HTTP 500",
		},
		{
			name: "unsupported version",
			retryInit: func(w http.ResponseWriter, id int) {
				w.Header().Set("Mcp-Session-Id", "sess-2")
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05"}}`, id)
			},
			wantErr:     "unsupported protocol version",
			wantDeletes: []string{"sess-2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			var deletes deleteLog
			var inits, toolCalls atomic.Int32
			var expired atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deletes.record(w, r)
					return
				}
				req := decodeRPC(t, r)
				switch req.Method {
				case "initialize":
					n := inits.Add(1)
					if n == 2 {
						tt.retryInit(w, deref(req.ID))
						return
					}
					writeInitialize(w, deref(req.ID), fmt.Sprintf("sess-%d", n))
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				default:
					toolCalls.Add(1)
					if expired.Load() && r.Header.Get("Mcp-Session-Id") == "sess-1" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					writeToolText(w, deref(req.ID), "ok")
				}
			}))
			defer server.Close()

			c := newClient(t, server.URL)
			_, err := call(c)
			must.NoError(err, "first CallTool")
			expired.Store(true)
			_, err = call(c)
			must.ErrorContains(err, tt.wantErr)
			must.NotContains(err.Error(), "HTTP 404", "the expired session's 404 must not be reported")
			must.EqualValues(2, toolCalls.Load(), "the call is not re-sent after a failed re-initialisation")
			must.Equal(tt.wantDeletes, deletes.sessions())

			_, err = call(c)
			must.NoError(err, "CallTool after the failed re-initialisation")
			must.EqualValues(3, inits.Load())
		})
	}
}

func TestCallTool_NotFoundWithoutSessionNotRetried(t *testing.T) {
	// A 404 on a request that carried no session id is an ordinary failure:
	// nothing expired, so nothing is re-sent.
	tests := []struct {
		name          string
		initStatus    int
		wantErr       string
		wantToolCalls int32
	}{
		{"tools/call on a stateless server", http.StatusOK, "tools/call failed: HTTP 404", 1},
		{"initialize", http.StatusNotFound, "initialize failed: HTTP 404", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			var inits, toolCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := decodeRPC(t, r)
				switch req.Method {
				case "initialize":
					inits.Add(1)
					if tt.initStatus != http.StatusOK {
						w.WriteHeader(tt.initStatus)
						return
					}
					writeInitialize(w, deref(req.ID), "")
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				default:
					toolCalls.Add(1)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			_, err := call(newClient(t, server.URL))
			must.ErrorContains(err, tt.wantErr)
			must.EqualValues(1, inits.Load())
			must.Equal(tt.wantToolCalls, toolCalls.Load())
		})
	}
}

func TestCallTool_FailedHandshakeNotCached(t *testing.T) {
	// A failed handshake is not kept: the next call starts a new one. A
	// session issued by the failed handshake is ended.
	tests := []struct {
		name        string
		fail        string
		wantErr     string
		wantDeletes int32
	}{
		{"initialize error", "initialize", "initialize failed: HTTP 500", 0},
		{"initialize error issuing a session", "initialize-session", "initialize failed: HTTP 500", 1},
		{"unsupported version", "version", "unsupported protocol version", 1},
		{"initialized notification error", "initialized", "notifications/initialized failed: HTTP 500", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			var inits, notifies, deletes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					check.Equal("sess-1", r.Header.Get("Mcp-Session-Id"), "DELETE of the failed handshake's session")
					deletes.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				req := decodeRPC(t, r)
				switch req.Method {
				case "initialize":
					n := inits.Add(1)
					switch {
					case n == 1 && tt.fail == "initialize":
						w.WriteHeader(http.StatusInternalServerError)
					case n == 1 && tt.fail == "initialize-session":
						w.Header().Set("Mcp-Session-Id", "sess-1")
						w.WriteHeader(http.StatusInternalServerError)
					case n == 1 && tt.fail == "version":
						w.Header().Set("Mcp-Session-Id", "sess-1")
						fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05"}}`, deref(req.ID))
					default:
						writeInitialize(w, deref(req.ID), fmt.Sprintf("sess-%d", n))
					}
				case "notifications/initialized":
					if notifies.Add(1) == 1 && tt.fail == "initialized" {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusAccepted)
				default:
					writeToolText(w, deref(req.ID), "ok")
				}
			}))
			defer server.Close()

			c := newClient(t, server.URL)
			_, err := call(c)
			must.ErrorContains(err, tt.wantErr, "first call")
			_, err = call(c)
			must.NoError(err, "second call")
			must.EqualValues(2, inits.Load())
			must.Equal(tt.wantDeletes, deletes.Load())
		})
	}
}

func TestCallTool_ConcurrentCallersShareHandshakeFailure(t *testing.T) {
	// Waiters share the failure of the handshake they joined instead of each
	// starting another; the failure is not kept for later calls.
	must := require.New(t)
	check := assert.New(t)
	const callers = 6
	var inits atomic.Int32
	arrived := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRPC(t, r)
		switch req.Method {
		case "initialize":
			if inits.Add(1) == 1 {
				close(arrived)
				<-release
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeInitialize(w, deref(req.ID), "")
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			writeToolText(w, deref(req.ID), "ok")
		}
	}))
	defer server.Close()
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()

	c := newClient(t, server.URL)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			_, err := call(c)
			errs <- err
		}()
	}
	awaitSignal(t, arrived, "the first initialize")
	time.Sleep(20 * time.Millisecond)
	releaseOnce()

	failed := 0
	for range callers {
		err := <-errs
		if err == nil {
			continue
		}
		failed++
		must.ErrorContains(err, "initialize failed: HTTP 500")
	}
	// Callers that joined the failing handshake share its error; any that
	// arrived after it ended started the one further handshake.
	wantInits := int32(1)
	if failed < callers {
		wantInits = 2
	}
	check.NotZero(failed, "no caller saw the failed handshake")
	check.Equal(wantInits, inits.Load(), "with %d of %d callers failed", failed, callers)

	_, err := call(c)
	must.NoError(err, "a call after the failed handshake")
	check.Equal(int32(2), inits.Load())
}

func TestCallTool_WaiterHonoursOwnContext(t *testing.T) {
	// A caller waiting on another's handshake gives up at its own deadline
	// and leaves that handshake running.
	must := require.New(t)
	check := assert.New(t)
	var inits atomic.Int32
	arrived := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRPC(t, r)
		switch req.Method {
		case "initialize":
			if inits.Add(1) == 1 {
				close(arrived)
			}
			<-release
			writeInitialize(w, deref(req.ID), "")
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			writeToolText(w, deref(req.ID), "ok")
		}
	}))
	defer server.Close()
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()

	c := newClient(t, server.URL)
	leaderErr := make(chan error, 1)
	go func() {
		_, err := call(c)
		leaderErr <- err
	}()
	awaitSignal(t, arrived, "the leader's initialize")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.CallTool(ctx, "lookup", nil)
	must.ErrorIs(err, context.DeadlineExceeded, "the waiter should fail at its own deadline")
	check.LessOrEqual(time.Since(start), time.Second, "the waiter should give up at its 50ms deadline")

	releaseOnce()
	must.NoError(<-leaderErr, "leader")
	check.EqualValues(1, inits.Load())
}

func TestCallTool_LeaderCancellationDoesNotFailWaiters(t *testing.T) {
	// A handshake that fails only because its leader's deadline passed is not
	// shared: a waiter with time left runs the handshake itself.
	must := require.New(t)
	check := assert.New(t)
	var inits atomic.Int32
	arrived := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRPC(t, r)
		switch req.Method {
		case "initialize":
			if inits.Add(1) == 1 {
				close(arrived)
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
				}
				return
			}
			writeInitialize(w, deref(req.ID), "")
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			writeToolText(w, deref(req.ID), "ok")
		}
	}))
	defer server.Close()

	c := newClient(t, server.URL)
	leaderCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	leaderErr := make(chan error, 1)
	go func() {
		_, err := c.CallTool(leaderCtx, "lookup", nil)
		leaderErr <- err
	}()
	awaitSignal(t, arrived, "the leader's initialize")

	_, err := call(c)
	must.NoError(err, "waiter should run its own handshake after the leader's deadline")
	must.Error(<-leaderErr, "leader should fail at its deadline")
	check.EqualValues(2, inits.Load())
}

func TestCallTool_CancelledHandshakeEndsSession(t *testing.T) {
	// A handshake cut short by its caller's context after initialize issued a
	// session still ends that session: the DELETE runs on its own context.
	must := require.New(t)
	check := assert.New(t)
	var deletes deleteLog
	notifying := make(chan struct{})
	notifyOnce := sync.OnceFunc(func() { close(notifying) })
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.record(w, r)
			return
		}
		req := decodeRPC(t, r)
		switch req.Method {
		case "initialize":
			writeInitialize(w, deref(req.ID), "sess-cut")
		case "notifications/initialized":
			notifyOnce()
			select {
			case <-r.Context().Done():
			case <-release:
			}
		default:
			check.Failf("unexpected request", "%s sent after the handshake was cancelled", req.Method)
		}
	}))
	defer server.Close()
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()

	c := newClient(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := c.CallTool(ctx, "lookup", nil)
		errc <- err
	}()
	awaitSignal(t, notifying, "the initialized notification")
	cancel()

	err := awaitErr(t, errc, "the cancelled CallTool")
	must.ErrorContains(err, "context canceled")
	must.Equal([]string{"sess-cut"}, deletes.sessions(), "DELETEs for the cancelled handshake's session")
}

func TestClose_EndsSessionOnce(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithSessionID("sess-close"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	_, err := call(c)
	must.NoError(err, "CallTool")
	for i := range 2 {
		must.NoError(c.Close(), "Close #%d", i+1)
	}
	must.Equal(1, fake.DeleteCount(), "DELETE count")
	end := fake.Requests()[3]
	check.Equal(http.MethodDelete, end.HTTPMethod, "request[3] method")
	check.Equal("sess-close", end.SessionID, "request[3] session")
	check.Equal("Bearer "+testKey, end.Authorization, "DELETE authorization")
	check.Equal(mcpclient.ProtocolVersion, end.ProtocolVersion, "DELETE protocol version")

	before := fake.CallCount()
	_, err = call(c)
	must.ErrorIs(err, mcpclient.ErrClosed, "CallTool after Close")
	check.NotContains(err.Error(), "sess-close", "error carries the session id")
	check.Equal(before, fake.CallCount(), "requests sent after Close")
}

func TestClose_WithoutSessionSendsNoDelete(t *testing.T) {
	tests := []struct {
		name  string
		opts  []mcpclienttest.FakeOption
		calls int
	}{
		{"before any call", []mcpclienttest.FakeOption{mcpclienttest.WithSessionID("sess-unused")}, 0},
		{"stateless server", nil, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := mcpclienttest.NewFakeServer(textResult("ok"), tt.opts...)
			defer fake.Close()

			c := newClient(t, fake.URL())
			for range tt.calls {
				_, err := call(c)
				must.NoError(err, "CallTool")
			}
			must.NoError(c.Close(), "Close")
			must.Zero(fake.DeleteCount(), "DELETE count after Close")
			_, err := call(c)
			must.ErrorIs(err, mcpclient.ErrClosed, "CallTool after Close")
		})
	}
}

func TestClose_BoundedByRequestTimeout(t *testing.T) {
	// Close takes no context, so its DELETE runs on a fresh one bounded by
	// RequestTimeout: a server that never answers cannot hang shutdown.
	must := require.New(t)
	check := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		req := decodeRPC(t, r)
		switch req.Method {
		case "initialize":
			writeInitialize(w, deref(req.ID), "sess-slow")
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			writeToolText(w, deref(req.ID), "ok")
		}
	}))
	defer server.Close()

	c := newClient(t, server.URL, func(o *mcpclient.Options) { o.RequestTimeout = 200 * time.Millisecond })
	_, err := call(c)
	must.NoError(err, "CallTool")
	start := time.Now()
	must.NoError(c.Close(), "Close")
	check.LessOrEqual(time.Since(start), 1500*time.Millisecond, "the 200ms RequestTimeout should bound the DELETE")
}

func TestClose_ConcurrentWithCallTool(t *testing.T) {
	// Close may race any number of calls: each call succeeds or fails with
	// ErrClosed, and every session established is ended exactly once.
	must := require.New(t)
	check := assert.New(t)
	const callers = 8
	fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithSessionID("sess-race"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			for range 5 {
				_, err := call(c)
				if err != nil {
					check.ErrorIs(err, mcpclient.ErrClosed, "CallTool should succeed or fail with ErrClosed")
				}
			}
		})
	}
	wg.Go(func() {
		check.NoError(c.Close(), "Close")
	})
	wg.Wait()

	_, err := call(c)
	must.ErrorIs(err, mcpclient.ErrClosed, "CallTool after Close")
	inits, deletes := fake.InitializeCount(), fake.DeleteCount()
	check.Equal(inits, deletes, "every session should be ended exactly once")
	check.LessOrEqual(inits, 1, "at most one session should be initialized")
}

func TestClose_DuringHandshakeEndsLateSession(t *testing.T) {
	// A handshake that completes after Close has returned yields ErrClosed,
	// not a tool result, and the session it was issued is ended at once.
	must := require.New(t)
	check := assert.New(t)
	var deletes deleteLog
	var toolCalls atomic.Int32
	arrived := make(chan struct{})
	arriveOnce := sync.OnceFunc(func() { close(arrived) })
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.record(w, r)
			return
		}
		req := decodeRPC(t, r)
		switch req.Method {
		case "initialize":
			arriveOnce()
			<-release
			writeInitialize(w, deref(req.ID), "sess-late")
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			toolCalls.Add(1)
			writeToolText(w, deref(req.ID), "ok")
		}
	}))
	defer server.Close()
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()

	c := newClient(t, server.URL)
	errc := make(chan error, 1)
	go func() {
		_, err := call(c)
		errc <- err
	}()
	awaitSignal(t, arrived, "the initialize")
	must.NoError(c.Close(), "Close")
	must.Empty(deletes.sessions(), "DELETEs before any session was issued")

	releaseOnce()
	err := awaitErr(t, errc, "the CallTool racing Close")
	must.ErrorIs(err, mcpclient.ErrClosed, "CallTool racing Close")
	check.Zero(toolCalls.Load(), "tools/call count after Close")
	check.Equal([]string{"sess-late"}, deletes.sessions(), "DELETEs for the session issued after Close")
}

func TestCallTool_SessionIDNeverInErrors(t *testing.T) {
	// The session id is redacted from any server-supplied text an error or a
	// tool error carries.
	const sessionID = "sess-secret-4f2a9c"
	tests := []struct {
		name       string
		initialize func(w http.ResponseWriter, id int)
		toolCall   func(w http.ResponseWriter, id int)
		wantErr    string
	}{
		{
			name: "initialize error body",
			initialize: func(w http.ResponseWriter, _ int) {
				w.Header().Set("Mcp-Session-Id", sessionID)
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, "cannot start %s", sessionID)
			},
			wantErr: "initialize failed: HTTP 500",
		},
		{
			name: "version echoing the session",
			initialize: func(w http.ResponseWriter, id int) {
				w.Header().Set("Mcp-Session-Id", sessionID)
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q}}`, id, sessionID)
			},
			wantErr: "unsupported protocol version",
		},
		{
			name: "404 body after re-initialisation",
			toolCall: func(w http.ResponseWriter, _ int) {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, "no session %s", sessionID)
			},
			wantErr: "tools/call failed: HTTP 404",
		},
		{
			name: "500 body",
			toolCall: func(w http.ResponseWriter, _ int) {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, "session %s broke", sessionID)
			},
			wantErr: "tools/call failed: HTTP 500",
		},
		{
			name: "JSON-RPC error",
			toolCall: func(w http.ResponseWriter, id int) {
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":"bad session %s"}}`, id, sessionID)
			},
			wantErr: "tools/call rejected",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				req := decodeRPC(t, r)
				switch {
				case req.Method == "initialize" && tt.initialize != nil:
					tt.initialize(w, deref(req.ID))
				case req.Method == "initialize":
					writeInitialize(w, deref(req.ID), sessionID)
				case req.Method == "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				default:
					tt.toolCall(w, deref(req.ID))
				}
			}))
			defer server.Close()

			_, err := call(newClient(t, server.URL))
			must.ErrorContains(err, tt.wantErr)
			must.NotContains(err.Error(), sessionID, "error carries the session id")
		})
	}

	t.Run("tool error text", func(t *testing.T) {
		must := require.New(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req := decodeRPC(t, r)
			switch req.Method {
			case "initialize":
				writeInitialize(w, deref(req.ID), sessionID)
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
			default:
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"session %s over quota"}],"isError":true}}`, deref(req.ID), sessionID)
			}
		}))
		defer server.Close()

		got, err := call(newClient(t, server.URL))
		must.NoError(err, "CallTool")
		text := mcpclient.FirstText(got.Content)
		must.NotContains(text, sessionID, "tool error text should redact the session id")
		must.Contains(text, "over quota", "tool error text should keep its message")
	})
}

func TestCallTool_InvalidSessionIDRefused(t *testing.T) {
	// A session id must be at most 1024 bytes of visible ASCII. Any other
	// fails the handshake at initialize, and the id is never echoed: not on a
	// later request, not in a DELETE, not in the error.
	tests := []struct {
		name string
		id   string
	}{
		{"over 1024 bytes", strings.Repeat("s", 1025)},
		{"contains a space", "sess 1"},
		{"contains a tab", "sess\t1"},
		{"non-ASCII", "sess-é"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithSessionID(tt.id))
			defer fake.Close()

			c := newClient(t, fake.URL())
			for i := range 2 {
				_, err := call(c)
				must.ErrorContains(err, "invalid Mcp-Session-Id", "call %d", i+1)
				must.NotContains(err.Error(), tt.id, "call %d error carries the session id", i+1)
			}
			must.NoError(c.Close(), "Close")
			for i, r := range fake.Requests() {
				must.Equal(http.MethodPost, r.HTTPMethod, "request[%d] HTTP method", i)
				must.Equal("initialize", r.Method, "request[%d] method", i)
				must.Empty(r.SessionID, "request[%d] session", i)
			}
			must.Equal(2, fake.InitializeCount(), "initialize count")
		})
	}

	t.Run("1024 bytes accepted", func(t *testing.T) {
		must := require.New(t)
		id := strings.Repeat("s", 1024)
		fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithSessionID(id))
		defer fake.Close()

		c := newClient(t, fake.URL())
		_, err := call(c)
		must.NoError(err, "CallTool")
		must.NoError(c.Close(), "Close")
		reqs := fake.Requests()
		must.Len(reqs, 4, "want initialize, initialized, tools/call, and DELETE requests")
		for i, r := range reqs[1:] {
			must.Equal(id, r.SessionID, "request[%d] (%s %s) did not echo the 1024-byte session id", i+1, r.HTTPMethod, r.Method)
		}
	})

	t.Run("oversized id on a failed initialize", func(t *testing.T) {
		must := require.New(t)
		var deletes deleteLog
		id := strings.Repeat("s", 1025)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				deletes.record(w, r)
				return
			}
			w.Header().Set("Mcp-Session-Id", id)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		_, err := call(newClient(t, server.URL))
		must.ErrorContains(err, "initialize failed: HTTP 500")
		must.Empty(deletes.sessions(), "DELETEs for a session id that is never echoed")
	})
}
