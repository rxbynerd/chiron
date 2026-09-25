package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// initialize / tools/call parameter and result shapes. Only the fields this
// client sends or reads are modelled; unknown fields are ignored on decode.

type initializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    clientCapabilities `json:"capabilities"`
	ClientInfo      clientInfo         `json:"clientInfo"`
}

// clientCapabilities is deliberately empty: this client neither offers
// sampling nor consumes roots/prompts. An empty object is a valid, minimal
// capability advertisement.
type clientCapabilities struct{}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type callToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// establish runs initialize and the initialized notification and returns the
// new session. When the handshake fails after the server issued a session id
// it still holds, that session is ended with a best-effort DELETE, even when
// the failure is ctx ending.
func (c *Client) establish(ctx context.Context) (session, error) {
	sess, err := c.initialize(ctx)
	if err == nil {
		err = c.notifyInitialized(ctx, sess)
	}
	if err != nil {
		if sess.id != "" && !sessionGone(err, sess) {
			c.endSession(sess)
		}
		return session{}, err
	}
	return sess, nil
}

// initialize performs the MCP initialize request and returns the session for
// later requests: any Mcp-Session-Id the server assigned (empty when the
// server is stateless) and the protocol version it chose, which must be a
// supported one. An issued id that fails validSessionID fails the handshake
// and is dropped, so it is never echoed. On any other failure the returned
// session still carries the issued id, with no protocol version, so the
// caller can end it.
func (c *Client) initialize(ctx context.Context) (session, error) {
	id := c.nextID()
	rpc, sessionID, err := c.doRequest(ctx, session{}, rpcRequest{
		JSONRPC: jsonRPCVersion,
		ID:      &id,
		Method:  "initialize",
		Params: initializeParams{
			ProtocolVersion: ProtocolVersion,
			Capabilities:    clientCapabilities{},
			ClientInfo:      clientInfo{Name: c.clientName, Version: c.clientVersion},
		},
	})
	if !validSessionID(sessionID) {
		if err == nil {
			err = errInvalidSessionID
		}
		return session{}, err
	}
	sess := session{id: sessionID}
	if err != nil {
		return sess, err
	}
	if rpc.Error != nil {
		return sess, fmt.Errorf("mcp: initialize rejected: %s", c.errorText(rpc.Error.Error(), sess))
	}
	var result initializeResult
	if err := json.Unmarshal(rpc.Result, &result); err != nil {
		return sess, fmt.Errorf("mcp: decoding initialize result: %s", c.scrub(err.Error()))
	}
	if !slices.Contains(supportedProtocolVersions, result.ProtocolVersion) {
		return sess, &ProtocolVersionError{Version: c.excerpt(result.ProtocolVersion, maxVersionEchoBytes, sess)}
	}
	sess.protocolVersion = result.ProtocolVersion
	return sess, nil
}

// notifyInitialized sends the notifications/initialized notification that
// completes the handshake. A notification has no reply to correlate.
func (c *Client) notifyInitialized(ctx context.Context, sess session) error {
	_, _, err := c.doRequest(ctx, sess, rpcRequest{
		JSONRPC: jsonRPCVersion,
		Method:  "notifications/initialized",
	})
	return err
}

// callTool issues the tools/call and decodes its result envelope. The text of
// a tool-level error is scrubbed and bounded here, where the key is known.
func (c *Client) callTool(ctx context.Context, sess session, name string, args map[string]any) (ToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	id := c.nextID()
	rpc, _, err := c.doRequest(ctx, sess, rpcRequest{
		JSONRPC: jsonRPCVersion,
		ID:      &id,
		Method:  "tools/call",
		Params:  callToolParams{Name: name, Arguments: args},
	})
	if err != nil {
		return ToolResult{}, err
	}
	if rpc.Error != nil {
		return ToolResult{}, fmt.Errorf("mcp: tools/call rejected: %s", c.errorText(rpc.Error.Error(), sess))
	}

	var result ToolResult
	if err := json.Unmarshal(rpc.Result, &result); err != nil {
		return ToolResult{}, fmt.Errorf("mcp: decoding tool result: %s", c.scrub(err.Error()))
	}
	if result.IsError {
		for i := range result.Content {
			result.Content[i].Text = c.errorText(result.Content[i].Text, sess)
		}
	}
	return result, nil
}
