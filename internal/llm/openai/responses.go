package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	types "github.com/gemechischala/awaki/internal/domain"
)

// ResponsesAdapter talks to OpenAI Responses API with previous_response_id chaining.
type ResponsesAdapter struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewResponsesAdapter(baseURL, apiKey string) *ResponsesAdapter {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &ResponsesAdapter{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{},
	}
}

func (a *ResponsesAdapter) SupportsContinuation() bool { return true }

func (a *ResponsesAdapter) Call(
	ctx context.Context,
	model string,
	systemPrompt string,
	messages []types.Message,
	tools []types.ToolDef,
	maxTokens int,
) (*types.Response, error) {
	return a.CallContinue(ctx, model, systemPrompt, "", messages, tools, maxTokens)
}

func (a *ResponsesAdapter) CallContinue(
	ctx context.Context,
	model, systemPrompt, previousID string,
	messages []types.Message,
	tools []types.ToolDef,
	maxTokens int,
) (*types.Response, error) {
	input := messagesToResponsesInput(messages)
	body := map[string]any{
		"model": model,
		"input": input,
		"store": true,
	}
	if previousID != "" {
		body["previous_response_id"] = previousID
	}
	if systemPrompt != "" {
		body["instructions"] = systemPrompt
	}
	if maxTokens > 0 {
		body["max_output_tokens"] = maxTokens
	}
	if len(tools) > 0 {
		var oaiTools []map[string]any
		for _, t := range tools {
			params := normalizeFunctionParameters(t.Parameters, t.Required)
			oaiTools = append(oaiTools, map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
				"parameters":   map[string]any(params),
			})
		}
		body["tools"] = oaiTools
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/responses", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai responses: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openai responses: status %d: %s", resp.StatusCode, truncateStr(string(data), 500))
	}
	return parseResponsesAPI(data)
}

func messagesToResponsesInput(msgs []types.Message) any {
	if len(msgs) == 1 && msgs[0].Role == types.RoleUser {
		var text string
		ok := true
		for _, b := range msgs[0].Content {
			if b.Type == "text" || b.Type == "" {
				text += b.Text
			} else {
				ok = false
				break
			}
		}
		if ok && text != "" {
			return text
		}
	}
	var items []map[string]any
	for _, msg := range msgs {
		role := string(msg.Role)
		if role == "assistant" {
			role = "assistant"
		}
		var text strings.Builder
		for _, b := range msg.Content {
			if b.Type == "text" || b.Type == "" {
				text.WriteString(b.Text)
			}
			if b.Type == "tool_result" {
				items = append(items, map[string]any{
					"type": "function_call_output",
					"call_id": b.ToolUseID,
					"output": b.ToolOutput,
				})
			}
			if b.Type == "tool_use" {
				items = append(items, map[string]any{
					"type": "function_call",
					"name": b.ToolName,
					"arguments": string(b.ToolInput),
					"call_id": b.ToolUseID,
				})
			}
		}
		if text.Len() > 0 {
			items = append(items, map[string]any{
				"type": "message",
				"role": role,
				"content": text.String(),
			})
		}
	}
	if len(items) == 0 {
		return ""
	}
	return items
}

func parseResponsesAPI(body []byte) (*types.Response, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := &types.Response{}
	if id, ok := raw["id"].(string); ok {
		out.ID = id
		out.ContinuationID = id
	}
	if usage, ok := raw["usage"].(map[string]any); ok {
		if v, ok := usage["input_tokens"].(float64); ok {
			out.InputTokens = int(v)
		}
		if v, ok := usage["output_tokens"].(float64); ok {
			out.OutputTokens = int(v)
		}
	}
	// output array
	if outputs, ok := raw["output"].([]any); ok {
		for i, item := range outputs {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "function_call":
				name, _ := m["name"].(string)
				args, _ := m["arguments"].(string)
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID = fmt.Sprintf("call_%d", i)
				}
				out.Content = append(out.Content, types.ToolUseBlock(callID, name, json.RawMessage(args)))
				out.StopReason = "tool_use"
			case "message":
				if content, ok := m["content"].([]any); ok {
					for _, c := range content {
						cm, _ := c.(map[string]any)
						if cm == nil {
							continue
						}
						if tx, ok := cm["text"].(string); ok && tx != "" {
							out.Content = append(out.Content, types.TextBlock(tx))
						}
					}
				}
			}
		}
	}
	if out.StopReason == "" {
		out.StopReason = "end_turn"
	}
	if len(out.Content) == 0 {
		if s, ok := raw["output_text"].(string); ok && s != "" {
			out.Content = append(out.Content, types.TextBlock(s))
		}
	}
	return out, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
