package mcpclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rxbynerd/chiron/internal/mcpclient"
	"github.com/rxbynerd/chiron/internal/mcpclient/mcpclienttest"
)

// decodeRPC reads and decodes one JSON-RPC request body in full.
func decodeRPC(t *testing.T, r *http.Request) rpcRequest {
	t.Helper()
	var req rpcRequest
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("reading request body: %v", err)
		return req
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Errorf("decoding request body: %v", err)
	}
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
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestCallToolReusesSession(t *testing.T) {
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
			fake := mcpclienttest.NewFakeServer(textResult("ok"), tt.opts...)
			defer fake.Close()

			c := newClient(t, fake.URL())
			for range 3 {
				if _, err := call(c); err != nil {
					t.Fatalf("CallTool: %v", err)
				}
			}
			if n := fake.InitializeCount(); n != 1 {
				t.Errorf("initialize count = %d, want 1", n)
			}
			if n := fake.ToolCallCount(); n != 3 {
				t.Errorf("tools/call count = %d, want 3", n)
			}
			if n := fake.DeleteCount(); n != 0 {
				t.Errorf("DELETE count = %d before Close, want 0", n)
			}
			reqs := fake.Requests()
			if len(reqs) != 5 {
				t.Fatalf("request count = %d, want 5 (initialize, initialized, three tools/call)", len(reqs))
			}
			for i, r := range reqs[1:] {
				if r.SessionID != tt.session || r.ProtocolVersion != mcpclient.ProtocolVersion {
					t.Errorf("request[%d] session %q version %q, want %q and %q", i+1, r.SessionID, r.ProtocolVersion, tt.session, mcpclient.ProtocolVersion)
				}
			}
			seen := map[int]bool{}
			for _, r := range reqs {
				if r.Method == "notifications/initialized" {
					continue
				}
				if r.ID == 0 || seen[r.ID] {
					t.Errorf("%s request id %d is missing or reused", r.Method, r.ID)
				}
				seen[r.ID] = true
			}
		})
	}
}

func TestCallToolConcurrentCallsShareOneHandshake(t *testing.T) {
	// Callers arriving while the handshake is in flight wait for it rather
	// than start their own.
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
		if err := <-errs; err != nil {
			t.Errorf("CallTool: %v", err)
		}
	}
	if n := inits.Load(); n != 1 {
		t.Errorf("initialize count = %d, want 1", n)
	}
	if n := toolCalls.Load(); n != callers {
		t.Errorf("tools/call count = %d, want %d", n, callers)
	}
}

func TestCallToolReinitialisesAfterSessionExpiry(t *testing.T) {
	// A 404 on a request bearing the session id means the server dropped the
	// session without processing the call: one re-initialisation, one resend,
	// and no DELETE for the session that is already gone.
	fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithSessionID("sess-x"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := call(c); err != nil {
		t.Fatalf("first CallTool: %v", err)
	}
	fake.ExpireSession()
	got, err := call(c)
	if err != nil {
		t.Fatalf("CallTool after expiry: %v", err)
	}
	if mcpclient.FirstText(got.Content) != "ok" {
		t.Errorf("result = %+v, want the scripted text", got)
	}

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
	if len(reqs) != len(want) {
		t.Fatalf("request count = %d, want %d: %+v", len(reqs), len(want), reqs)
	}
	for i, w := range want {
		if reqs[i].Method != w.method || reqs[i].SessionID != w.session {
			t.Errorf("request[%d] = %q on %q, want %q on %q", i, reqs[i].Method, reqs[i].SessionID, w.method, w.session)
		}
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := fake.DeleteCount(); n != 1 {
		t.Errorf("DELETE count = %d, want 1", n)
	}
	if last := fake.Requests()[len(want)]; last.HTTPMethod != http.MethodDelete || last.SessionID != "sess-x-2" {
		t.Errorf("last request = %s on %q, want a DELETE of the fresh session", last.HTTPMethod, last.SessionID)
	}
}

func TestCallToolConcurrentCallsReinitialiseOnce(t *testing.T) {
	// Callers that all find the session gone share one re-initialisation; a
	// caller still holding the stale session never discards the fresh one.
	const callers = 8
	fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithSessionID("sess-old"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := call(c); err != nil {
		t.Fatalf("first CallTool: %v", err)
	}
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
		if err != nil {
			t.Errorf("CallTool: %v", err)
		}
	}
	if n := fake.InitializeCount(); n != 2 {
		t.Errorf("initialize count = %d, want 2 (the first session and one re-initialisation)", n)
	}
	// Each caller sends its call once, plus once more if it held the expired
	// session; at least one did.
	if n := fake.ToolCallCount(); n < callers+2 || n > 2*callers+1 {
		t.Errorf("tools/call count = %d, want between %d and %d", n, callers+2, 2*callers+1)
	}
}

func TestCallToolRepeatedSessionExpiryFails(t *testing.T) {
	// A second 404 in one call fails it rather than looping, and the session
	// is dropped so the next call starts afresh, under the same bound.
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
		if err == nil || !strings.Contains(err.Error(), "tools/call failed: HTTP 404") {
			t.Fatalf("call %d error = %v, want the tools/call 404", i, err)
		}
		if strings.Contains(err.Error(), "sess-gone") {
			t.Errorf("error carries the session id: %v", err)
		}
		if n := inits.Load(); n != 2*i {
			t.Errorf("after call %d initialize count = %d, want %d", i, n, 2*i)
		}
		if n := toolCalls.Load(); n != 2*i {
			t.Errorf("after call %d tools/call count = %d, want %d", i, n, 2*i)
		}
	}
	if n := deletes.Load(); n != 0 {
		t.Errorf("DELETE count = %d, want 0 for sessions the server already dropped", n)
	}
}

func TestCallToolNotFoundWithoutSessionNotRetried(t *testing.T) {
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
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if n := inits.Load(); n != 1 {
				t.Errorf("initialize count = %d, want 1", n)
			}
			if n := toolCalls.Load(); n != tt.wantToolCalls {
				t.Errorf("tools/call count = %d, want %d", n, tt.wantToolCalls)
			}
		})
	}
}

func TestCallToolFailedHandshakeNotCached(t *testing.T) {
	// A failed handshake is not kept: the next call starts a new one. A
	// session issued by the failed handshake is ended.
	tests := []struct {
		name        string
		fail        string
		wantErr     string
		wantDeletes int32
	}{
		{"initialize error", "initialize", "initialize failed: HTTP 500", 0},
		{"unsupported version", "version", "unsupported protocol version", 1},
		{"initialized notification error", "initialized", "notifications/initialized failed: HTTP 500", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var inits, notifies, deletes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					if r.Header.Get("Mcp-Session-Id") != "sess-1" {
						t.Errorf("DELETE of %q, want the failed handshake's session", r.Header.Get("Mcp-Session-Id"))
					}
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
			if _, err := call(c); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("first call error = %v, want %q", err, tt.wantErr)
			}
			if _, err := call(c); err != nil {
				t.Fatalf("second call: %v", err)
			}
			if n := inits.Load(); n != 2 {
				t.Errorf("initialize count = %d, want 2", n)
			}
			if n := deletes.Load(); n != tt.wantDeletes {
				t.Errorf("DELETE count = %d, want %d", n, tt.wantDeletes)
			}
		})
	}
}

func TestCallToolConcurrentCallersShareHandshakeFailure(t *testing.T) {
	// Waiters share the failure of the handshake they joined instead of each
	// starting another; the failure is not kept for later calls.
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
		if !strings.Contains(err.Error(), "initialize failed: HTTP 500") {
			t.Errorf("CallTool error = %v, want the shared initialize failure", err)
		}
	}
	// Callers that joined the failing handshake share its error; any that
	// arrived after it ended started the one further handshake.
	wantInits := int32(1)
	if failed < callers {
		wantInits = 2
	}
	if failed == 0 {
		t.Error("no caller saw the failed handshake")
	}
	if n := inits.Load(); n != wantInits {
		t.Errorf("initialize count = %d, want %d with %d of %d callers failed", n, wantInits, failed, callers)
	}

	if _, err := call(c); err != nil {
		t.Fatalf("a call after the failed handshake: %v", err)
	}
	if n := inits.Load(); n != 2 {
		t.Errorf("initialize count = %d, want 2", n)
	}
}

func TestCallToolWaiterHonoursOwnContext(t *testing.T) {
	// A caller waiting on another's handshake gives up at its own deadline
	// and leaves that handshake running.
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
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiter error = %v, want its own deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waiter took %s; it should give up at its 50ms deadline", elapsed)
	}

	releaseOnce()
	if err := <-leaderErr; err != nil {
		t.Errorf("leader: %v", err)
	}
	if n := inits.Load(); n != 1 {
		t.Errorf("initialize count = %d, want 1", n)
	}
}

func TestCallToolLeaderCancellationDoesNotFailWaiters(t *testing.T) {
	// A handshake that fails only because its leader's deadline passed is not
	// shared: a waiter with time left runs the handshake itself.
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

	if _, err := call(c); err != nil {
		t.Fatalf("waiter should run its own handshake after the leader's deadline, got: %v", err)
	}
	if err := <-leaderErr; err == nil {
		t.Error("leader succeeded, want a failure at its deadline")
	}
	if n := inits.Load(); n != 2 {
		t.Errorf("initialize count = %d, want 2", n)
	}
}

func TestCloseEndsSessionOnce(t *testing.T) {
	fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithSessionID("sess-close"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	if _, err := call(c); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	for i := range 2 {
		if err := c.Close(); err != nil {
			t.Errorf("Close #%d: %v", i+1, err)
		}
	}
	if n := fake.DeleteCount(); n != 1 {
		t.Fatalf("DELETE count = %d, want 1", n)
	}
	end := fake.Requests()[3]
	if end.HTTPMethod != http.MethodDelete || end.SessionID != "sess-close" {
		t.Errorf("request[3] = %s on %q, want the session DELETE", end.HTTPMethod, end.SessionID)
	}
	if end.Authorization != "Bearer "+testKey || end.ProtocolVersion != mcpclient.ProtocolVersion {
		t.Errorf("DELETE Authorization %q version %q, want the bearer key and %q", end.Authorization, end.ProtocolVersion, mcpclient.ProtocolVersion)
	}

	before := fake.CallCount()
	_, err := call(c)
	if !errors.Is(err, mcpclient.ErrClosed) {
		t.Errorf("CallTool after Close = %v, want ErrClosed", err)
	}
	if err != nil && strings.Contains(err.Error(), "sess-close") {
		t.Errorf("error carries the session id: %v", err)
	}
	if n := fake.CallCount(); n != before {
		t.Errorf("CallTool after Close sent %d requests, want none", n-before)
	}
}

func TestCloseWithoutSessionSendsNoDelete(t *testing.T) {
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
			fake := mcpclienttest.NewFakeServer(textResult("ok"), tt.opts...)
			defer fake.Close()

			c := newClient(t, fake.URL())
			for range tt.calls {
				if _, err := call(c); err != nil {
					t.Fatalf("CallTool: %v", err)
				}
			}
			if err := c.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if n := fake.DeleteCount(); n != 0 {
				t.Errorf("DELETE count = %d, want 0", n)
			}
			if _, err := call(c); !errors.Is(err, mcpclient.ErrClosed) {
				t.Errorf("CallTool after Close = %v, want ErrClosed", err)
			}
		})
	}
}

func TestCloseBoundedByRequestTimeout(t *testing.T) {
	// Close takes no context, so its DELETE runs on a fresh one bounded by
	// RequestTimeout: a server that never answers cannot hang shutdown.
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
	if _, err := call(c); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("Close took %s; the 200ms RequestTimeout should bound its DELETE", elapsed)
	}
}

func TestCloseConcurrentWithCallTool(t *testing.T) {
	// Close may race any number of calls: each call succeeds or fails with
	// ErrClosed, and every session established is ended exactly once.
	const callers = 8
	fake := mcpclienttest.NewFakeServer(textResult("ok"), mcpclienttest.WithSessionID("sess-race"))
	defer fake.Close()

	c := newClient(t, fake.URL())
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			for range 5 {
				if _, err := call(c); err != nil && !errors.Is(err, mcpclient.ErrClosed) {
					t.Errorf("CallTool = %v, want success or ErrClosed", err)
				}
			}
		})
	}
	wg.Go(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	wg.Wait()

	if _, err := call(c); !errors.Is(err, mcpclient.ErrClosed) {
		t.Errorf("CallTool after Close = %v, want ErrClosed", err)
	}
	if inits, deletes := fake.InitializeCount(), fake.DeleteCount(); inits != deletes || inits > 1 {
		t.Errorf("initialize count %d, DELETE count %d; want at most one session, ended exactly once", inits, deletes)
	}
}

func TestSessionIDNeverInErrors(t *testing.T) {
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
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), sessionID) {
				t.Errorf("error carries the session id: %v", err)
			}
		})
	}

	t.Run("tool error text", func(t *testing.T) {
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
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if text := mcpclient.FirstText(got.Content); strings.Contains(text, sessionID) || !strings.Contains(text, "over quota") {
			t.Errorf("tool error text = %q, want it kept with the session id redacted", text)
		}
	})
}
