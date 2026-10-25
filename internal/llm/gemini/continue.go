package gemini

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

// SupportsContinuation uses Gemini Interactions API (previous_interaction_id).
func (a *Adapter) SupportsContinuation() bool { return true }

// CallContinue creates an Interaction, optionally chained from previousID.
// When previousID is set, only newMessages are sent as input (not full history).
func (a *Adapter) CallContinue(
	ctx context.Context,
	model, systemPrompt, previousID string,
	newMessages []types.Message,
	tools []types.ToolDef,
	maxTokens int,
) (*types.Response, error) {
	base := strings.TrimRight(a.baseURL, "/")
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	// Interactions live under v1beta/interactions
	url := base + "/v1beta/interactions?key=" + a.apiKey

	input := messagesToInteractionInput(newMessages)
	body := map[string]any{
		"model": model,
		"input": input,
		"store": true,
	}
	if previousID != "" {
		body["previous_interaction_id"] = previousID
	}
	if systemPrompt != "" {
		body["system_instruction"] = systemPrompt
	}
	if maxTokens > 0 {
		body["generation_config"] = map[string]any{"max_output_tokens": maxTokens}
	}
	if len(tools) > 0 {
		decls := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			decls = append(decls, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":   t.Parameters,
			})
		}
		body["tools"] = []map[string]any{{"type": "function", "functions": decls}}
		// Also try classic functionDeclarations shape for compatibility
		body["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini interactions: http: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gemini interactions: status %d: %s", resp.StatusCode, truncate(string(data), 500))
	}
	return parseInteractionResponse(data)
}

func messagesToInteractionInput(msgs []types.Message) any {
	// Prefer simple string when single user text turn; else structured contents-like list.
	if len(msgs) == 1 && msgs[0].Role == types.RoleUser {
		var onlyText string
		onlyTextOK := true
		for _, b := range msgs[0].Content {
			if b.Type == "text" || b.Type == "" {
				onlyText += b.Text
			} else {
				onlyTextOK = false
				break
			}
		}
		if onlyTextOK && onlyText != "" {
			return onlyText
		}
	}
	// Structured: mirror generateContent contents for multi-part / tool turns.
	var contents []genContent
	for _, msg := range msgs {
		role := "user"
		if msg.Role == types.RoleAssistant {
			role = "model"
		}
		var parts []genPart
		for _, b := range msg.Content {
			switch b.Type {
			case "text", "":
				if b.Text != "" {
					parts = append(parts, genPart{Text: b.Text})
				}
			case "tool_use":
				args := b.ToolInput
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				parts = append(parts, genPart{FunctionCall: &genFunctionCall{Name: b.ToolName, Args: args}})
				role = "model"
			case "tool_result":
				role = "user"
				name := b.ToolName
				if name == "" {
					name = "tool"
				}
				var resp any
				if err := json.Unmarshal([]byte(b.ToolOutput), &resp); err != nil {
					resp = map[string]any{"result": b.ToolOutput}
					if b.ToolIsErr {
						resp = map[string]any{"error": b.ToolOutput}
					}
				}
				parts = append(parts, genPart{FunctionResponse: &genFunctionResp{Name: name, Response: resp}})
			}
		}
		if len(parts) > 0 {
			contents = append(contents, genContent{Role: role, Parts: parts})
		}
	}
	if len(contents) == 0 {
		return ""
	}
	return contents
}

type interactionResponse struct {
	ID         string `json:"id"`
	OutputText string `json:"output_text"`
	Outputs    []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"outputs"`
	Steps []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"steps"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		// legacy names
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usage"`
}

func parseInteractionResponse(body []byte) (*types.Response, error) {
	var ir interactionResponse
	if err := json.Unmarshal(body, &ir); err != nil {
		return nil, fmt.Errorf("gemini interactions unmarshal: %w", err)
	}
	out := &types.Response{
		ID:             ir.ID,
		ContinuationID: ir.ID,
		InputTokens:    ir.Usage.InputTokens,
		OutputTokens:   ir.Usage.OutputTokens,
	}
	if out.InputTokens == 0 {
		out.InputTokens = ir.Usage.PromptTokenCount
	}
	if out.OutputTokens == 0 {
		out.OutputTokens = ir.Usage.CandidatesTokenCount
	}

	hasTool := false
	// Prefer outputs[], then steps, then output_text
	for i, o := range ir.Outputs {
		switch o.Type {
		case "function_call", "tool_call":
			hasTool = true
			args := o.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			out.Content = append(out.Content, types.ToolUseBlock(fmt.Sprintf("call_%d", i), o.Name, args))
		default:
			for _, c := range o.Content {
				if c.Text != "" {
					out.Content = append(out.Content, types.TextBlock(c.Text))
				}
			}
		}
	}
	if len(out.Content) == 0 {
		for i, s := range ir.Steps {
			if s.Type == "function_call" || s.Type == "tool_call" || s.Name != "" && s.Arguments != nil {
				hasTool = true
				args := s.Arguments
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				name := s.Name
				if name == "" {
					name = "tool"
				}
				out.Content = append(out.Content, types.ToolUseBlock(fmt.Sprintf("call_%d", i), name, args))
			}
			for _, c := range s.Content {
				if c.Text != "" {
					out.Content = append(out.Content, types.TextBlock(c.Text))
				}
			}
		}
	}
	if len(out.Content) == 0 && ir.OutputText != "" {
		out.Content = append(out.Content, types.TextBlock(ir.OutputText))
	}
	if hasTool {
		out.StopReason = "tool_use"
	} else {
		out.StopReason = "end_turn"
	}
	return out, nil
}
