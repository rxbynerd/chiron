package search

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

// mcpSessionHeader is the response header a Streamable-HTTP MCP server may set
// on the initialize reply to bind the session; it must be echoed on every
// subsequent request (MCP transport spec).
const mcpSessionHeader = "Mcp-Session-Id"

// rpcRequest is a JSON-RPC request or notification. A notification omits ID
// (JSON-RPC distinguishes the two by the presence of the member); Params is
// omitted when nil.
type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int        `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC response. Exactly one of Result / Error is set on
// a well-formed reply. Result is kept raw so each method decodes its own
// shape.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
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

// doRequest POSTs one JSON-RPC request and returns the matching response plus
// any Mcp-Session-Id the server set. sessionID, when non-empty, is echoed on
// the request. The reply may be application/json (a single JSON-RPC message)
// or text/event-stream (SSE frames carrying JSON-RPC messages); both are read
// under maxBodyBytes and reduced to the one response message.
func (c *Client) doRequest(ctx context.Context, sessionID string, req rpcRequest) (rpcResponse, string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return rpcResponse{}, "", fmt.Errorf("search: encoding %s request: %s", req.Method, c.scrub(err.Error()))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return rpcResponse{}, "", fmt.Errorf("search: building %s request: %s", req.Method, c.scrub(err.Error()))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// A Streamable-HTTP client must accept both reply framings so the server
	// may choose either per request (MCP transport spec).
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if sessionID != "" {
		httpReq.Header.Set(mcpSessionHeader, sessionID)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// The transport error can carry the request URL; the key is never in
		// the URL, but scrub regardless so no diagnostic can leak it.
		return rpcResponse{}, "", fmt.Errorf("search: POST %s: %s", req.Method, c.scrub(err.Error()))
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
	return rpc, newSession, nil
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
			return rpcResponse{}, fmt.Errorf("search: reading response: %s", c.scrub(err.Error()))
		}
		var rpc rpcResponse
		if err := json.Unmarshal(data, &rpc); err != nil {
			return rpcResponse{}, fmt.Errorf("search: decoding response: %s", c.scrub(err.Error()))
		}
		return rpc, nil
	}
}

// readEventStream reads a text/event-stream reply and returns the first
// JSON-RPC response message it carries — the one correlating to the request
// just sent. It mirrors the SSE reading discipline in
// internal/interactions.Stream (line-oriented scan, data: accumulation across
// continuation lines, blank-line frame dispatch) but is reimplemented
// minimally: this transport only needs the one response frame, not a
// reconnecting event feed. The whole stream is bounded by maxBodyBytes.
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
			return rpcResponse{}, false, fmt.Errorf("search: decoding SSE response: %s", c.scrub(err.Error()))
		}
		// A JSON-RPC message on the stream that is not a response (e.g. a
		// server-initiated request or notification, which have a "method")
		// is skipped: we want the reply frame. A response has no method and
		// carries result or error.
		if rpc.Method() {
			return rpcResponse{}, false, nil
		}
		return rpc, true, nil
	}

	for scanner.Scan() {
		read += int64(len(scanner.Bytes())) + 1
		if read > c.maxBodyBytes {
			return rpcResponse{}, fmt.Errorf("search: SSE stream exceeds %d-byte bound", c.maxBodyBytes)
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
					return rpcResponse{}, fmt.Errorf("search: SSE event data exceeds %d-byte bound", c.maxBodyBytes)
				}
			}
			// event:/id:/retry: and unknown fields are ignored — the JSON-RPC
			// payload rides the data: field.
		}
	}
	if err := scanner.Err(); err != nil {
		if err == bufio.ErrTooLong {
			return rpcResponse{}, fmt.Errorf("search: SSE event exceeds %d-byte bound", c.maxBodyBytes)
		}
		return rpcResponse{}, fmt.Errorf("search: reading SSE stream: %s", c.scrub(err.Error()))
	}
	// A trailing frame not terminated by a blank line is still dispatched, so
	// a server that ends the stream without a final newline is tolerated.
	if rpc, done, err := dispatch(); err != nil {
		return rpcResponse{}, err
	} else if done {
		return rpc, nil
	}
	return rpcResponse{}, fmt.Errorf("search: SSE stream carried no JSON-RPC response")
}

// Method reports whether the decoded message is a request/notification rather
// than a response — used to skip non-reply frames on an SSE stream. A message
// with neither result nor error and no id is treated as a non-response.
func (r rpcResponse) Method() bool {
	return r.Result == nil && r.Error == nil && r.ID == nil
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
		return fmt.Errorf("search: %s failed: HTTP %d", method, resp.StatusCode)
	}
	return fmt.Errorf("search: %s failed: HTTP %d: %s", method, resp.StatusCode, c.scrub(detail))
}

// scrub redacts credentials from a diagnostic string. It replaces the
// client's own key by exact match first — a guaranteed redaction that does
// not depend on the key clearing secret.Scrub's entropy heuristics — then
// runs secret.Scrub for any other credential-shaped material. The empty check
// guards against an empty apiKey redacting every empty substring.
func (c *Client) scrub(s string) string {
	if c.apiKey != "" {
		s = strings.ReplaceAll(s, c.apiKey, "[REDACTED:search-api-key]")
	}
	return secret.Scrub(s)
}

// readBounded reads at most max bytes, failing — rather than silently
// truncating — if the body is larger, so a misbehaving server cannot exhaust
// memory or smuggle a clipped document through as complete. This mirrors
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
