package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	types "github.com/gemechischala/awaki/internal/domain"
)

// Adapter talks to Google Generative Language API (Gemini).
// Format: POST {base}/{model}:generateContent?key=API_KEY
type Adapter struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewAdapter(baseURL, apiKey string) *Adapter {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &Adapter{
		baseURL: baseURL,
		apiKey:  apiKey,
		client:  &http.Client{Timeout: 5 * time.Minute},
	}
}

func (a *Adapter) Call(
	ctx context.Context,
	model string,
	systemPrompt string,
	messages []types.Message,
	tools []types.ToolDef,
	maxTokens int,
) (*types.Response, error) {
	model = strings.TrimPrefix(model, "models/")
	reqBody := buildRequest(systemPrompt, messages, tools, maxTokens)

	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", a.baseURL, model, a.apiKey)
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("gemini: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini: http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gemini: read body: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gemini: status %d: %s", resp.StatusCode, truncate(string(body), 500))
	}

	return parseResponse(body)
}

// --- request types ---

type genRequest struct {
	SystemInstruction *contentPart  `json:"systemInstruction,omitempty"`
	Contents          []genContent  `json:"contents"`
	Tools             []genTool     `json:"tools,omitempty"`
	GenerationConfig  *genConfig    `json:"generationConfig,omitempty"`
}

type genContent struct {
	Role  string    `json:"role"` // "user" | "model"
	Parts []genPart `json:"parts"`
}

type genPart struct {
	Text             string            `json:"text,omitempty"`
	FunctionCall     *genFunctionCall  `json:"functionCall,omitempty"`
	FunctionResponse *genFunctionResp  `json:"functionResponse,omitempty"`
	InlineData       *genInlineData    `json:"inlineData,omitempty"`
}

type genFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type genFunctionResp struct {
	Name     string `json:"name"`
	Response any    `json:"response"`
}

type genInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64
}

type genTool struct {
	FunctionDeclarations []genFnDecl `json:"functionDeclarations"`
}

type genFnDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type genConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

type contentPart struct {
	Parts []genPart `json:"parts"`
}

func buildRequest(system string, messages []types.Message, tools []types.ToolDef, maxTokens int) genRequest {
	req := genRequest{}
	if system != "" {
		req.SystemInstruction = &contentPart{Parts: []genPart{{Text: system}}}
	}
	if maxTokens > 0 {
		req.GenerationConfig = &genConfig{MaxOutputTokens: maxTokens}
	}
	if len(tools) > 0 {
		decls := make([]genFnDecl, 0, len(tools))
		for _, t := range tools {
			params := t.Parameters
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			// Ensure JSON-schema style type
			if _, ok := params["type"]; !ok {
				params = map[string]any{
					"type":       "object",
					"properties": params,
					"required":   t.Required,
				}
			}
			decls = append(decls, genFnDecl{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			})
		}
		req.Tools = []genTool{{FunctionDeclarations: decls}}
	}

	// Gemini expects alternating user/model. Tool results are user-role functionResponse parts.
	for _, m := range messages {
		role := "user"
		if m.Role == types.RoleAssistant {
			role = "model"
		}
		var parts []genPart
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if b.Text != "" {
					parts = append(parts, genPart{Text: b.Text})
				}
			case "image":
				parts = append(parts, genPart{InlineData: &genInlineData{
					MimeType: b.MimeType,
					Data:     b.ImageData,
				}})
			case "tool_use":
				args := b.ToolInput
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				parts = append(parts, genPart{FunctionCall: &genFunctionCall{
					Name: b.ToolName,
					Args: args,
				}})
				role = "model"
			case "tool_result":
				// Gemini wants functionResponse under user role
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
				parts = append(parts, genPart{FunctionResponse: &genFunctionResp{
					Name:     name,
					Response: resp,
				}})
			}
		}
		if len(parts) == 0 {
			continue
		}
		req.Contents = append(req.Contents, genContent{Role: role, Parts: parts})
	}
	return req
}

// --- response types ---

type genResponse struct {
	Candidates []struct {
		Content struct {
			Parts []genPart `json:"parts"`
			Role  string    `json:"role"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

func parseResponse(body []byte) (*types.Response, error) {
	var gr genResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("gemini: unmarshal: %w", err)
	}
	if len(gr.Candidates) == 0 {
		return nil, fmt.Errorf("gemini: empty candidates: %s", truncate(string(body), 300))
	}
	cand := gr.Candidates[0]
	out := &types.Response{
		InputTokens:  gr.UsageMetadata.PromptTokenCount,
		OutputTokens: gr.UsageMetadata.CandidatesTokenCount,
	}

	hasTool := false
	for i, p := range cand.Content.Parts {
		if p.Text != "" {
			out.Content = append(out.Content, types.TextBlock(p.Text))
		}
		if p.FunctionCall != nil {
			hasTool = true
			id := fmt.Sprintf("call_%d", i)
			args := p.FunctionCall.Args
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			out.Content = append(out.Content, types.ToolUseBlock(id, p.FunctionCall.Name, args))
		}
	}

	switch strings.ToUpper(cand.FinishReason) {
	case "STOP", "":
		if hasTool {
			out.StopReason = "tool_use"
		} else {
			out.StopReason = "end_turn"
		}
	case "MAX_TOKENS":
		out.StopReason = "max_tokens"
	case "SAFETY", "RECITATION", "OTHER":
		out.StopReason = "end_turn"
	default:
		if hasTool {
			out.StopReason = "tool_use"
		} else {
			out.StopReason = "end_turn"
		}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
