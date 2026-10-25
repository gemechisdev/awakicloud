package openai

import (
	"context"
	"encoding/json"
	"fmt"
	types "github.com/gemechischala/awaki/internal/domain"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go"
	oaioption "github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"
)

type Adapter struct {
	client         *openai.Client
	supportsVision bool
}

func NewAdapter(baseURL, apiKey string, supportsVision bool) *Adapter {
	httpClient := &http.Client{Timeout: 5 * time.Minute}
	c := openai.NewClient(
		oaioption.WithBaseURL(baseURL),
		oaioption.WithAPIKey(apiKey),
		oaioption.WithHTTPClient(httpClient),
	)
	return &Adapter{client: &c, supportsVision: supportsVision}
}

func (a *Adapter) Call(
	ctx context.Context,
	model string,
	systemPrompt string,
	messages []types.Message,
	tools []types.ToolDef,
	maxTokens int,
) (*types.Response, error) {
	reasoning := isReasoningModel(model)
	oaiMsgs := messagesToOpenAI(a.supportsVision, reasoning, systemPrompt, messages)
	oaiTools := toolDefsToOpenAI(tools)

	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(model),
		Messages: oaiMsgs,
	}
	if reasoning {
		params.MaxCompletionTokens = openai.Int(int64(maxTokens))
	} else {
		params.MaxTokens = openai.Int(int64(maxTokens))
	}
	if len(oaiTools) > 0 {
		params.Tools = oaiTools
	}

	start := time.Now()
	resp, err := a.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("openai call (%s, elapsed %s): %w", model, time.Since(start), err)
	}

	return openaiToResponse(resp), nil
}

func isReasoningModel(model string) bool {
	for _, prefix := range []string{"o1", "o3", "o4"} {
		if model == prefix || strings.HasPrefix(model, prefix+"-") {
			return true
		}
	}
	return false
}

func messagesToOpenAI(supportsVision, reasoning bool, system string, msgs []types.Message) []openai.ChatCompletionMessageParamUnion {
	var out []openai.ChatCompletionMessageParamUnion

	if system != "" {
		if reasoning {
			out = append(out, openai.DeveloperMessage(system))
		} else {
			out = append(out, openai.SystemMessage(system))
		}
	}

	for _, msg := range msgs {
		switch msg.Role {
		case types.RoleUser:
			var parts []openai.ChatCompletionContentPartUnionParam
			var toolResults []openai.ChatCompletionMessageParamUnion
			hasImage := false
			var textOnly string

			for _, b := range msg.Content {
				switch b.Type {
				case "text":
					parts = append(parts, openai.TextContentPart(b.Text))
					textOnly += b.Text
				case "image":
					if supportsVision {
						hasImage = true
						dataURL := fmt.Sprintf("data:%s;base64,%s", b.MimeType, b.ImageData)
						parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
							URL: dataURL,
						}))
					} else {
						placeholder := "[Image omitted: model does not support vision]"
						parts = append(parts, openai.TextContentPart(placeholder))
						textOnly += placeholder + "\n"
					}
				case "tool_result":
					toolResults = append(toolResults, openai.ToolMessage(b.ToolOutput, b.ToolUseID))
				}
			}

			if len(parts) > 0 {
				if hasImage {
					out = append(out, openai.UserMessage(parts))
				} else {
					out = append(out, openai.UserMessage(textOnly))
				}
			}
			out = append(out, toolResults...)

		case types.RoleAssistant:
			var text string
			var toolCalls []openai.ChatCompletionMessageToolCallParam

			for _, b := range msg.Content {
				switch b.Type {
				case "text":
					text += b.Text
				case "tool_use":
					toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallParam{
						ID: b.ToolUseID,
						Function: openai.ChatCompletionMessageToolCallFunctionParam{
							Name:      b.ToolName,
							Arguments: string(b.ToolInput),
						},
					})
				}
			}

			asst := openai.ChatCompletionAssistantMessageParam{}
			if text != "" {
				asst.Content.OfString = openai.String(text)
			}
			if len(toolCalls) > 0 {
				asst.ToolCalls = toolCalls
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &asst})
		}
	}

	return out
}

func toolDefsToOpenAI(defs []types.ToolDef) []openai.ChatCompletionToolParam {
	var out []openai.ChatCompletionToolParam
	for _, d := range defs {
		fd := shared.FunctionDefinitionParam{
			Name:        d.Name,
			Description: openai.String(d.Description),
			Parameters:  normalizeFunctionParameters(d.Parameters, d.Required),
		}
		out = append(out, openai.ChatCompletionToolParam{Function: fd})
	}
	return out
}

// normalizeFunctionParameters accepts either:
//   - a full JSON Schema object: {"type":"object","properties":{...},"required":[...]}
//   - a properties-only map: {"name":{"type":"string"}, ...}  (Awaki tool convention)
// Strict OpenAI-compatible gateways reject the latter when wrongly nested under properties.
func normalizeFunctionParameters(params map[string]any, required []string) shared.FunctionParameters {
	out := shared.FunctionParameters{"type": "object"}
	if params == nil {
		out["properties"] = map[string]any{}
		if len(required) > 0 {
			out["required"] = required
		}
		return out
	}
	// Full schema already?
	if typ, ok := params["type"].(string); ok && typ == "object" {
		for k, v := range params {
			out[k] = v
		}
		if _, ok := out["properties"]; !ok {
			out["properties"] = map[string]any{}
		}
		// Sanitize property schemas (arrays need items for strict validators).
		if props, ok := out["properties"].(map[string]any); ok {
			out["properties"] = sanitizePropertySchemas(props)
		}
		if len(required) > 0 {
			if _, has := out["required"]; !has {
				out["required"] = required
			}
		}
		return out
	}
	out["properties"] = sanitizePropertySchemas(params)
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func sanitizePropertySchemas(props map[string]any) map[string]any {
	clean := make(map[string]any, len(props))
	for name, raw := range props {
		schema, ok := raw.(map[string]any)
		if !ok {
			// Skip invalid entries (e.g. bare strings) that break strict gateways.
			continue
		}
		// Copy so we do not mutate tool definitions in place.
		cp := make(map[string]any, len(schema)+1)
		for k, v := range schema {
			cp[k] = v
		}
		if typ, _ := cp["type"].(string); typ == "array" {
			if _, hasItems := cp["items"]; !hasItems {
				cp["items"] = map[string]any{"type": "string"}
			}
		}
		clean[name] = cp
	}
	return clean
}

func openaiToResponse(resp *openai.ChatCompletion) *types.Response {
	r := &types.Response{
		ID:           resp.ID,
		InputTokens:  int(resp.Usage.PromptTokens),
		OutputTokens: int(resp.Usage.CompletionTokens),
	}

	if len(resp.Choices) == 0 {
		r.StopReason = "end_turn"
		return r
	}

	choice := resp.Choices[0]

	switch choice.FinishReason {
	case "stop":
		r.StopReason = "end_turn"
	case "tool_calls":
		r.StopReason = "tool_use"
	case "length":
		r.StopReason = "max_tokens"
	default:
		r.StopReason = "end_turn"
	}

	if choice.Message.Content != "" {
		r.Content = append(r.Content, types.TextBlock(choice.Message.Content))
	}

	for _, tc := range choice.Message.ToolCalls {
		r.Content = append(r.Content, types.ToolUseBlock(
			tc.ID,
			tc.Function.Name,
			json.RawMessage(tc.Function.Arguments),
		))
	}

	return r
}
