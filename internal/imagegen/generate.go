// Package imagegen calls OpenAI-compatible Images API and Gemini native
// image generation endpoints. Anthropic has no native image API — use a
// fallback OpenAI/Gemini model instead.
package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/providers"
)

type Result struct {
	Data     []byte
	MimeType string
	Revised  string // optional revised prompt from provider
}

// Generate produces one image from prompt using the given provider+model.
func Generate(ctx context.Context, p providers.Provider, m providers.Model, prompt string, size string) (Result, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return Result{}, fmt.Errorf("prompt is required")
	}
	if size == "" {
		size = "1024x1024"
	}
	switch p.Protocol {
	case providers.ProtocolGemini:
		return generateGemini(ctx, p, m, prompt)
	case providers.ProtocolOpenAIChat, providers.ProtocolOpenAIResponses:
		return generateOpenAI(ctx, p, m, prompt, size)
	case providers.ProtocolAnthropic:
		return Result{}, fmt.Errorf("Anthropic has no image generation API — configure an OpenAI or Gemini image model as fallback")
	default:
		// Try OpenAI-style first (many gateways expose /images/generations)
		return generateOpenAI(ctx, p, m, prompt, size)
	}
}

func generateOpenAI(ctx context.Context, p providers.Provider, m providers.Model, prompt, size string) (Result, error) {
	base := strings.TrimRight(p.BaseURL, "/")
	// Accept bases that already include /v1
	url := base + "/images/generations"
	if strings.HasSuffix(base, "/v1") {
		url = base + "/images/generations"
	} else if !strings.Contains(base, "/v1") {
		url = base + "/v1/images/generations"
	}

	body := map[string]any{
		"model":  m.ModelID,
		"prompt": prompt,
		"n":      1,
		"size":   size,
	}
	// Older DALL·E models accept response_format; GPT Image returns b64 by default.
	mid := strings.ToLower(m.ModelID)
	if strings.Contains(mid, "dall-e") {
		body["response_format"] = "b64_json"
	}

	raw, status, err := postJSON(ctx, url, p.APIKey, body)
	if err != nil {
		return Result{}, err
	}
	if status >= 400 {
		return Result{}, fmt.Errorf("image API status %d: %s", status, truncate(string(raw), 500))
	}

	var resp struct {
		Data []struct {
			B64JSON       string `json:"b64_json"`
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Result{}, fmt.Errorf("decode image response: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return Result{}, fmt.Errorf("image API: %s", resp.Error.Message)
	}
	if len(resp.Data) == 0 {
		return Result{}, fmt.Errorf("image API returned no data")
	}
	item := resp.Data[0]
	var data []byte
	if item.B64JSON != "" {
		data, err = base64.StdEncoding.DecodeString(item.B64JSON)
		if err != nil {
			return Result{}, fmt.Errorf("b64 decode: %w", err)
		}
	} else if item.URL != "" {
		data, err = download(ctx, item.URL)
		if err != nil {
			return Result{}, err
		}
	} else {
		return Result{}, fmt.Errorf("image API entry has neither b64_json nor url")
	}
	mime := sniffMime(data)
	return Result{Data: data, MimeType: mime, Revised: item.RevisedPrompt}, nil
}

func generateGemini(ctx context.Context, p providers.Provider, m providers.Model, prompt string) (Result, error) {
	base := strings.TrimRight(p.BaseURL, "/")
	// Standard Generative Language API path
	modelPath := m.ModelID
	if !strings.HasPrefix(modelPath, "models/") {
		modelPath = "models/" + modelPath
	}
	url := fmt.Sprintf("%s/%s:generateContent", base, modelPath)
	if strings.Contains(base, "generativelanguage.googleapis.com") && !strings.Contains(base, "/v1beta") {
		// ensure v1beta
		if strings.HasSuffix(base, "/v1") {
			url = strings.TrimSuffix(base, "/v1") + "/v1beta/" + modelPath + ":generateContent"
		} else {
			url = base + "/v1beta/" + modelPath + ":generateContent"
		}
	}

	body := map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]any{{"text": prompt}}},
		},
		"generationConfig": map[string]any{
			"responseModalities": []string{"TEXT", "IMAGE"},
		},
	}

	raw, status, err := postJSONGemini(ctx, url, p.APIKey, body)
	if err != nil {
		return Result{}, err
	}
	if status >= 400 {
		return Result{}, fmt.Errorf("gemini image status %d: %s", status, truncate(string(raw), 500))
	}

	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text       string `json:"text"`
					InlineData *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Result{}, fmt.Errorf("decode gemini response: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return Result{}, fmt.Errorf("gemini: %s", resp.Error.Message)
	}
	for _, c := range resp.Candidates {
		for _, part := range c.Content.Parts {
			if part.InlineData != nil && part.InlineData.Data != "" {
				data, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
				if err != nil {
					return Result{}, fmt.Errorf("gemini b64: %w", err)
				}
				mime := part.InlineData.MimeType
				if mime == "" {
					mime = sniffMime(data)
				}
				return Result{Data: data, MimeType: mime}, nil
			}
		}
	}
	return Result{}, fmt.Errorf("gemini returned no image parts — is this an image-capable model (e.g. gemini-2.5-flash-image / nano-banana)?")
}

func postJSON(ctx context.Context, url, apiKey string, body any) ([]byte, int, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 30<<20))
	return raw, res.StatusCode, err
}

func postJSONGemini(ctx context.Context, url, apiKey string, body any) ([]byte, int, error) {
	b, _ := json.Marshal(body)
	// Gemini often uses ?key=
	if apiKey != "" && !strings.Contains(url, "key=") {
		if strings.Contains(url, "?") {
			url += "&key=" + apiKey
		} else {
			url += "?key=" + apiKey
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("x-goog-api-key", apiKey)
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 30<<20))
	return raw, res.StatusCode, err
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("download status %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 30<<20))
}

func sniffMime(data []byte) string {
	if len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}) {
		return "image/png"
	}
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return "image/jpeg"
	}
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	return "image/png"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
