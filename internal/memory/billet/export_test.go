package billet

import "github.com/rxbynerd/chiron/internal/mcpclient"

// Exported for billet_test, the external test package that also drives
// billettest's fake and so cannot see these unexported symbols directly.
const (
	SaveTool        = saveTool
	MaxToolErrBytes = maxToolErrBytes
)

// ToolError exposes toolError, so a test can compare its output against the
// same failure surfaced through the fake-driven transport.
func ToolError(tool string, result mcpclient.ToolResult) error { return toolError(tool, result) }

// FirstLine exposes firstLine for direct unit testing.
func FirstLine(s string) string { return firstLine(s) }

// MCPClient exposes a Client's underlying mcpclient.Client for tests
// exercising transport-level behaviour the Recall/Remember API does not
// surface.
func MCPClient(c *Client) *mcpclient.Client { return c.mcp }
