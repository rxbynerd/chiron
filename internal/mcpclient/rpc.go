package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/rxbynerd/chiron/internal/secret"
)

// JSON-RPC 2.0 envelopes (MCP wire types), all unexported and internal to the
// package. Only the fields this client sends or reads are modelled.

const jsonRPCVersion = "2.0"

// MCP Streamable-HTTP transport headers. A server may set mcpSessionHeader on
// the initialize reply to bind a session; the client echoes it, and sends the
// negotiated mcpProtocolVersionHeader, on every later request.
const (
	mcpSessionHeader         = "Mcp-Session-Id"
	mcpProtocolVersionHeader = "MCP-Protocol-Version"
)

// session is the state initialize establishes for the rest of one CallTool.
type session struct {
	id              string // empty for a stateless server
	protocolVersion string
}

// rpcRequest is a JSON-RPC request or notification. A notification omits ID
// (JSON-RPC distinguishes the two by the presence of the member); Params is
// omitted when nil.
type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int        `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC response, or a server-initiated request or
// notification (which carries Method) read off an SSE stream. Exactly one of
// Result / Error is set on a well-formed reply. ID and Result are kept raw:
// server requests may use string ids, and each method decodes its own result.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// isResponse reports whether the message is a response rather than a
// server-initiated request or notification.
func (r rpcResponse) isResponse() bool {
	return r.Method == ""
}

// answers reports whether r is the reply to the request with the given id. A
// null or absent id is accepted only on an error reply, which JSON-RPC 2.0
// uses when the server could not read the request id.
func (r rpcResponse) answers(id int) bool {
	raw := bytes.TrimSpace(r.ID)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return r.Error != nil
	}
	var got int
	return json.Unmarshal(raw, &got) == nil && got == id
}

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// doRequest POSTs one JSON-RPC request with the session's headers (initialize
// passes the zero session) and returns the reply, whose id must match the
// request's, plus any Mcp-Session-Id the server set. A JSON or SSE reply is
// read under maxBodyBytes and reduced to the one response message.
func (c *Client) doRequest(ctx context.Context, sess session, req rpcRequest) (rpcResponse, string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return rpcResponse{}, "", fmt.Errorf("mcp: encoding %s request: %s", req.Method, c.scrub(err.Error()))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return rpcResponse{}, "", fmt.Errorf("mcp: building %s request: %s", req.Method, c.scrub(err.Error()))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// A Streamable-HTTP client must accept both reply framings so the server
	// may choose either per request (MCP transport spec).
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	c.setSessionHeaders(httpReq, sess)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// The transport error can carry the request URL; the key is never in
		// the URL, but scrub regardless so no diagnostic can leak it.
		return rpcResponse{}, "", fmt.Errorf("mcp: POST %s: %s", req.Method, c.scrub(err.Error()))
	}
	defer resp.Body.Close()

	newSession := resp.Header.Get(mcpSessionHeader)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return rpcResponse{}, newSession, c.errorFromResponse(req.Method, resp)
	}

	// A notification has no reply body to correlate: the server acknowledges
	// with 202 Accepted (or 200 with an empty body). Do not attempt to parse
	// a JSON-RPC response for it.
	if req.ID == nil {
		// Drain a bounded amount so the connection can be reused, then stop.
		_, _ = readBounded(resp.Body, c.maxBodyBytes)
		return rpcResponse{}, newSession, nil
	}

	rpc, err := c.readResponse(resp)
	if err != nil {
		return rpcResponse{}, newSession, err
	}
	if !rpc.answers(*req.ID) {
		return rpcResponse{}, newSession, fmt.Errorf("mcp: %s reply does not answer request id %d", req.Method, *req.ID)
	}
	return rpc, newSession, nil
}

// setSessionHeaders sets the credential and the session's transport headers.
func (c *Client) setSessionHeaders(req *http.Request, sess session) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if sess.id != "" {
		req.Header.Set(mcpSessionHeader, sess.id)
	}
	if sess.protocolVersion != "" {
		req.Header.Set(mcpProtocolVersionHeader, sess.protocolVersion)
	}
}

// endSession sends the DELETE that asks the server to discard the session. It
// is best effort: a server may refuse client-initiated termination with 405,
// and no outcome of the DELETE affects the tool result.
func (c *Client) endSession(ctx context.Context, sess session) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint, nil)
	if err != nil {
		return
	}
	c.setSessionHeaders(req, sess)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = readBounded(resp.Body, maxErrorBodyBytes)
}

// readResponse extracts the single JSON-RPC response from a 2xx reply,
// dispatching on Content-Type. It bounds every read by maxBodyBytes.
func (c *Client) readResponse(resp *http.Response) (rpcResponse, error) {
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch mt {
	case "text/event-stream":
		return c.readEventStream(resp.Body)
	case "", "application/json":
		// Default to JSON: some minimal servers omit Content-Type on a JSON
		// body. text/event-stream is the only framing that needs distinct
		// handling, so anything else is decoded as a single JSON message.
		fallthrough
	default:
		data, err := readBounded(resp.Body, c.maxBodyBytes)
		if err != nil {
			return rpcResponse{}, fmt.Errorf("mcp: reading response: %s", c.scrub(err.Error()))
		}
		var rpc rpcResponse
		if err := json.Unmarshal(data, &rpc); err != nil {
			return rpcResponse{}, fmt.Errorf("mcp: decoding response: %s", c.scrub(err.Error()))
		}
		return rpc, nil
	}
}

// readEventStream returns the first JSON-RPC response on a text/event-stream
// reply without waiting for the stream to end; doRequest checks its id. It is
// a minimal line-oriented reader in the style of internal/interactions.Stream,
// with the whole stream bounded by maxBodyBytes.
func (c *Client) readEventStream(r io.Reader) (rpcResponse, error) {
	// Bound the aggregate stream read the same way a JSON body is bounded: a
	// misbehaving server must not stream unboundedly. The scanner's own
	// per-token cap is set from the same budget.
	limited := io.LimitReader(r, c.maxBodyBytes+1)
	scanner := bufio.NewScanner(limited)
	bufCap := c.maxBodyBytes
	if bufCap > int64(int(^uint(0)>>1)) { // guard the int conversion on 32-bit
		bufCap = int64(int(^uint(0) >> 1))
	}
	scanner.Buffer(make([]byte, 0, 64<<10), int(bufCap))

	var (
		data     []byte
		haveData bool
		read     int64
	)
	dispatch := func() (rpcResponse, bool, error) {
		if !haveData {
			return rpcResponse{}, false, nil
		}
		var rpc rpcResponse
		if err := json.Unmarshal(data, &rpc); err != nil {
			return rpcResponse{}, false, fmt.Errorf("mcp: decoding SSE response: %s", c.scrub(err.Error()))
		}
		// Server-initiated requests and notifications are skipped; this
		// client answers neither and waits for the reply frame.
		if !rpc.isResponse() {
			return rpcResponse{}, false, nil
		}
		return rpc, true, nil
	}

	for scanner.Scan() {
		read += int64(len(scanner.Bytes())) + 1
		if read > c.maxBodyBytes {
			return rpcResponse{}, fmt.Errorf("mcp: SSE stream exceeds %d-byte bound", c.maxBodyBytes)
		}
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "":
			rpc, done, err := dispatch()
			if err != nil {
				return rpcResponse{}, err
			}
			if done {
				return rpc, nil
			}
			data, haveData = nil, false
		case strings.HasPrefix(line, ":"):
			// Comment / keep-alive line.
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			if field == "data" {
				if haveData {
					data = append(data, '\n')
				}
				data = append(data, value...)
				haveData = true
				if int64(len(data)) > c.maxBodyBytes {
					return rpcResponse{}, fmt.Errorf("mcp: SSE event data exceeds %d-byte bound", c.maxBodyBytes)
				}
			}
			// event:/id:/retry: and unknown fields are ignored — the JSON-RPC
			// payload rides the data: field.
		}
	}
	if err := scanner.Err(); err != nil {
		if err == bufio.ErrTooLong {
			return rpcResponse{}, fmt.Errorf("mcp: SSE event exceeds %d-byte bound", c.maxBodyBytes)
		}
		return rpcResponse{}, fmt.Errorf("mcp: reading SSE stream: %s", c.scrub(err.Error()))
	}
	// A trailing frame not terminated by a blank line is still dispatched, so
	// a server that ends the stream without a final newline is tolerated.
	if rpc, done, err := dispatch(); err != nil {
		return rpcResponse{}, err
	} else if done {
		return rpc, nil
	}
	return rpcResponse{}, fmt.Errorf("mcp: SSE stream carried no JSON-RPC response")
}

// errorFromResponse builds an error from a non-2xx response, bounding the
// error-body read and scrubbing it — a server error payload could echo the
// submitted key back. The key is never in the body Chiron sends (it is
// header-only), but the server's echo is outside Chiron's control, so the
// body is scrubbed unconditionally.
func (c *Client) errorFromResponse(method string, resp *http.Response) error {
	data, err := readBounded(resp.Body, maxErrorBodyBytes)
	if err != nil {
		data = nil
	}
	detail := strings.TrimSpace(string(data))
	if detail == "" {
		return fmt.Errorf("mcp: %s failed: HTTP %d", method, resp.StatusCode)
	}
	return fmt.Errorf("mcp: %s failed: HTTP %d: %s", method, resp.StatusCode, c.scrub(detail))
}

// scrub redacts credentials from a diagnostic string. It replaces the
// client's own key by exact match first — a guaranteed redaction that does
// not depend on the key clearing secret.Scrub's entropy heuristics — then
// runs secret.Scrub for any other credential-shaped material. The empty check
// guards against an empty apiKey redacting every empty substring.
func (c *Client) scrub(s string) string {
	if c.apiKey != "" {
		s = strings.ReplaceAll(s, c.apiKey, "[REDACTED:mcp-api-key]")
	}
	return secret.Scrub(s)
}

// readBounded reads at most max bytes, failing — rather than silently
// truncating — if the body is larger, so a misbehaving server cannot exhaust
// memory or smuggle a clipped document through as complete. It mirrors
// internal/interactions.readBounded and model.readBounded.
func readBounded(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("body exceeds %d-byte bound", max)
	}
	return data, nil
}
