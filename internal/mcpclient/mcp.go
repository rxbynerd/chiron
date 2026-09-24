package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
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

// initialize performs the MCP initialize handshake and returns the session
// for later requests: any Mcp-Session-Id the server assigned (empty when the
// server is stateless) and the protocol version it chose, falling back to
// ProtocolVersion when the result names none.
func (c *Client) initialize(ctx context.Context) (session, error) {
	id := 1
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
	if err != nil {
		return session{}, err
	}
	if rpc.Error != nil {
		return session{}, fmt.Errorf("mcp: initialize rejected: %s", c.errorText(rpc.Error.Error()))
	}
	sess := session{id: sessionID, protocolVersion: ProtocolVersion}
	var result initializeResult
	if json.Unmarshal(rpc.Result, &result) == nil && result.ProtocolVersion != "" {
		sess.protocolVersion = result.ProtocolVersion
	}
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
	id := 2
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
		return ToolResult{}, fmt.Errorf("mcp: tools/call rejected: %s", c.errorText(rpc.Error.Error()))
	}

	var result ToolResult
	if err := json.Unmarshal(rpc.Result, &result); err != nil {
		return ToolResult{}, fmt.Errorf("mcp: decoding tool result: %s", c.scrub(err.Error()))
	}
	if result.IsError {
		for i := range result.Content {
			result.Content[i].Text = c.errorText(result.Content[i].Text)
		}
	}
	return result, nil
}
