package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/tools/types"
)

// AgentTools exposes MCP management + a generic call_mcp_tool entry point.
// Individual remote tools can also be registered dynamically via BridgeTools.
func AgentTools(m *Manager) []types.Tool {
	if m == nil {
		return nil
	}
	return []types.Tool{
		{
			Name:  "mcp_list",
			Label: "Listing MCP",
			Description: "List configured and connected MCP servers and their tools. " +
				"Use before call_mcp_tool to see available capabilities.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler: func(ctx context.Context, input map[string]any) types.Result {
				list, err := m.ListConfigured()
				if err != nil {
					return types.Errf("%v", err)
				}
				live := m.LiveTools()
				var b strings.Builder
				b.WriteString("Configured MCP servers:\n")
				if len(list) == 0 {
					b.WriteString("(none)\n")
				}
				for _, s := range list {
					b.WriteString(fmt.Sprintf("- %s [%s]", s.Name, s.Transport()))
					if s.Disabled {
						b.WriteString(" disabled")
					}
					if s.Command != "" {
						b.WriteString(" cmd=" + s.Command)
					}
					if s.URL != "" {
						b.WriteString(" url=" + s.URL)
					}
					b.WriteByte('\n')
				}
				b.WriteString("\nConnected tools:\n")
				if len(live) == 0 {
					b.WriteString("(none — connect servers with /mcp or Settings → MCP)\n")
				}
				for _, t := range live {
					b.WriteString(fmt.Sprintf("- %s — %s\n", t.Qualified, t.Tool.Description))
				}
				return types.Result{Output: b.String()}
			},
		},
		{
			Name:  "call_mcp_tool",
			Label: "Calling MCP tool",
			Description: "Call a tool on a connected MCP server. " +
				"Pass the qualified name from mcp_list (mcp__server__tool) or server+tool separately.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Qualified tool name mcp__server__tool",
					},
					"server": map[string]any{
						"type":        "string",
						"description": "Server name (alternative to qualified name)",
					},
					"tool": map[string]any{
						"type":        "string",
						"description": "Tool name on that server",
					},
					"arguments": map[string]any{
						"type":        "object",
						"description": "Arguments object for the tool",
					},
				},
			},
			Handler: func(ctx context.Context, input map[string]any) types.Result {
				args, _ := input["arguments"].(map[string]any)
				if args == nil {
					args = map[string]any{}
				}
				if q, _ := input["name"].(string); q != "" {
					out, err := m.CallQualified(ctx, q, args)
					if err != nil {
						return types.Errf("%v", err)
					}
					return types.TruncateOutput(out)
				}
				server, _ := input["server"].(string)
				tool, _ := input["tool"].(string)
				if server == "" || tool == "" {
					return types.Errf("provide name (mcp__server__tool) or server+tool")
				}
				out, err := m.Call(ctx, server, tool, args)
				if err != nil {
					return types.Errf("%v", err)
				}
				return types.TruncateOutput(out)
			},
		},
	}
}

// BridgeTools builds one agent tool per live MCP tool (optional bulk register).
func BridgeTools(m *Manager) []types.Tool {
	if m == nil {
		return nil
	}
	live := m.LiveTools()
	out := make([]types.Tool, 0, len(live))
	for _, lt := range live {
		lt := lt
		params := map[string]any{"type": "object", "properties": map[string]any{}}
		if lt.Tool.InputSchema != nil {
			params = lt.Tool.InputSchema
		}
		desc := lt.Tool.Description
		if desc == "" {
			desc = "MCP tool from " + lt.Server
		} else {
			desc = fmt.Sprintf("[MCP:%s] %s", lt.Server, desc)
		}
		out = append(out, types.Tool{
			Name:        lt.Qualified,
			Label:       "MCP " + lt.Tool.Name,
			Description: desc,
			Parameters:  params,
			Handler: func(ctx context.Context, input map[string]any) types.Result {
				res, err := m.Call(ctx, lt.Server, lt.Tool.Name, input)
				if err != nil {
					return types.Errf("%v", err)
				}
				return types.TruncateOutput(res)
			},
		})
	}
	return out
}
