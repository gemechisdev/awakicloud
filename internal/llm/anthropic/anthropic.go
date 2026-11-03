package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	types "github.com/gemechischala/awaki/internal/domain"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type Adapter struct {
	client *anthropic.Client
}

func NewAdapter(apiKey, baseURL string) *Adapter {
	httpClient := &http.Client{Timeout: 5 * time.Minute}
	opts := []option.RequestOption{
		option.WithHTTPClient(httpClient),
	}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	api := anthropic.NewClient(opts...)
	return &Adapter{client: &api}
}

func (a *Adapter) Call(
	ctx context.Context,
	model string,
	systemPrompt string,
	messages []types.Message,
	tools []types.ToolDef,
	maxTokens int,
) (*types.Response, error) {
	anthMsgs := messagesToAnthropic(messages)
	anthTools := toolDefsToAnthropic(tools)

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(maxTokens),
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Messages:  anthMsgs,
	}
	if len(anthTools) > 0 {
		params.Tools = anthTools
	}

	start := time.Now()
	msg, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("anthropic call (%s, elapsed %s): %w", model, time.Since(start), err)
	}

	return anthropicToResponse(msg), nil
}

func messagesToAnthropic(msgs []types.Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		var blocks []anthropic.ContentBlockParamUnion
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				blocks = append(blocks, anthropic.NewTextBlock(b.Text))
			case "image":
				blocks = append(blocks, anthropic.NewImageBlockBase64(b.MimeType, b.ImageData))
			case "tool_use":
				// Prefer a decoded object so the API sees a JSON object, not a string.
				var input any = map[string]any{}
				if len(b.ToolInput) > 0 {
					var obj any
					if json.Unmarshal(b.ToolInput, &obj) == nil {
						input = obj
					} else {
						input = b.ToolInput
					}
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(b.ToolUseID, input, b.ToolName))
			case "tool_result":
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolResult: &anthropic.ToolResultBlockParam{
						ToolUseID: b.ToolUseID,
						IsError:   anthropic.Bool(b.ToolIsErr),
						Content: []anthropic.ToolResultBlockParamContentUnion{
							{OfText: &anthropic.TextBlockParam{Text: b.ToolOutput}},
						},
					},
				})
			}
		}

		role := anthropic.MessageParamRoleUser
		if m.Role == types.RoleAssistant {
			role = anthropic.MessageParamRoleAssistant
		}
		out = append(out, anthropic.MessageParam{Role: role, Content: blocks})
	}
	return out
}

func toolDefsToAnthropic(defs []types.ToolDef) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(defs))
	for _, d := range defs {
		// Official shape (https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools):
		//
		//	input_schema: {
		//	  "type": "object",          // required; SDK defaults Type → "object"
		//	  "properties": { ... },
		//	  "required": [ ... ]
		//	}
		//
		// Do NOT pass the whole Parameters map as Properties — that was the
		// original 400-causing nesting bug. Anthropic accepts a broad JSON
		// Schema subset for non-strict tools; we still drop $schema and
		// normalize required from either the schema or ToolDef.Required.
		props := map[string]any{}
		if p, ok := d.Parameters["properties"].(map[string]any); ok {
			props = sanitizeAnthropicProps(p)
		}

		schema := anthropic.ToolInputSchemaParam{
			Properties: props,
		}
		schema.Required = collectRequired(d)

		name := sanitizeAnthropicToolName(d.Name)
		out = append(out, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        name,
				Description: anthropic.String(d.Description),
				InputSchema: schema,
			},
		})
	}
	return out
}

// collectRequired prefers Parameters["required"], then ToolDef.Required.
func collectRequired(d types.ToolDef) []string {
	if req, ok := d.Parameters["required"].([]any); ok {
		out := make([]string, 0, len(req))
		for _, r := range req {
			if s, ok := r.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if req, ok := d.Parameters["required"].([]string); ok && len(req) > 0 {
		return append([]string(nil), req...)
	}
	if len(d.Required) > 0 {
		return append([]string(nil), d.Required...)
	}
	return nil
}

// sanitizeAnthropicProps shallow-copies property defs and strips $schema keys
// that some MCP/tool authors embed. Anthropic is more permissive than Gemini
// but $schema at any level is noise and occasionally confuses validators.
func sanitizeAnthropicProps(props map[string]any) map[string]any {
	out := make(map[string]any, len(props))
	for k, v := range props {
		if m, ok := v.(map[string]any); ok {
			cp := make(map[string]any, len(m))
			for pk, pv := range m {
				if pk == "$schema" {
					continue
				}
				if nested, ok := pv.(map[string]any); ok {
					cp[pk] = stripSchemaKey(nested)
				} else {
					cp[pk] = pv
				}
			}
			out[k] = cp
		} else {
			out[k] = v
		}
	}
	return out
}

func stripSchemaKey(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		if k == "$schema" {
			continue
		}
		if nested, ok := v.(map[string]any); ok {
			cp[k] = stripSchemaKey(nested)
		} else if arr, ok := v.([]any); ok {
			cp[k] = stripSchemaKeyInArray(arr)
		} else {
			cp[k] = v
		}
	}
	return cp
}

func stripSchemaKeyInArray(arr []any) []any {
	out := make([]any, 0, len(arr))
	for _, v := range arr {
		if m, ok := v.(map[string]any); ok {
			out = append(out, stripSchemaKey(m))
		} else {
			out = append(out, v)
		}
	}
	return out
}

// sanitizeAnthropicToolName enforces ^[a-zA-Z0-9_-]{1,64}$ from the official docs.
func sanitizeAnthropicToolName(name string) string {
	if name == "" {
		return "tool"
	}
	b := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	if len(b) == 0 {
		return "tool"
	}
	if len(b) > 64 {
		b = b[:64]
	}
	return string(b)
}

func anthropicToResponse(msg *anthropic.Message) *types.Response {
	resp := &types.Response{
		ID:           msg.ID,
		StopReason:   string(msg.StopReason),
		InputTokens:  int(msg.Usage.InputTokens),
		OutputTokens: int(msg.Usage.OutputTokens),
	}
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			resp.Content = append(resp.Content, types.TextBlock(block.AsText().Text))
		case "tool_use":
			tu := block.AsToolUse()
			raw, _ := json.Marshal(tu.Input)
			resp.Content = append(resp.Content, types.ToolUseBlock(tu.ID, tu.Name, raw))
		}
	}
	return resp
}
