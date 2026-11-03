// Package imagegen generates images via provider APIs.
//
// Strategy (protocol-aware, not vendor-locked):
//
//  1. Gemini protocol → native generateContent with IMAGE modality
//  2. Anthropic → not supported (no image API)
//  3. OpenAI-compatible (and default):
//     a. POST {base}/images/generations  (OpenAI Images API)
//     b. If that fails, try inference-style endpoints used by many gateways:
//        - POST {aiRoot}/run/{model}  JSON  {prompt,width,height}
//        - POST {aiRoot}/run/{model}  multipart form-data
//     c. Flexible response parsing (OpenAI data[], result.image, image, b64, …)
//
// Chat completions is never used for image generation.
package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/providers"
)

type Result struct {
	Data     []byte
	MimeType string
	Revised  string
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
		res, err := generateGemini(ctx, p, m, prompt)
		if err == nil {
			return res, nil
		}
		// Some Gemini-compatible gateways only expose OpenAI-style images —
		// fall through to the OpenAI-compatible cascade.
		if res, err2 := generateOpenAICompatible(ctx, p, m, prompt, size); err2 == nil {
			return res, nil
		}
		return Result{}, err

	case providers.ProtocolAnthropic:
		return Result{}, fmt.Errorf("Anthropic has no image generation API — configure an OpenAI or Gemini image model as fallback")

	default:
		// OpenAI chat/responses and unknown protocols share the cascade.
		return generateOpenAICompatible(ctx, p, m, prompt, size)
	}
}

// generateOpenAICompatible tries standard Images API first, then inference
// run endpoints used by Workers AI and similar OpenAI-compatible gateways.
func generateOpenAICompatible(ctx context.Context, p providers.Provider, m providers.Model, prompt, size string) (Result, error) {
	var errs []string

	// 1) OpenAI Images API
	if res, err := generateOpenAIImages(ctx, p, m, prompt, size); err == nil {
		return res, nil
	} else {
		errs = append(errs, "images/generations: "+err.Error())
	}

	// 2) Inference run endpoints (JSON then multipart) — used by CF Workers AI,
	//    some local gateways, and other /ai/run-style hosts.
	if runURL, ok := inferenceRunURL(p.BaseURL, m.ModelID); ok {
		w, h := parseSize(size)
		if res, err := generateRunJSON(ctx, runURL, p.APIKey, prompt, w, h); err == nil {
			return res, nil
		} else {
			errs = append(errs, "run/json: "+err.Error())
		}
		if res, err := generateRunMultipart(ctx, runURL, p.APIKey, prompt, w, h); err == nil {
			return res, nil
		} else {
			errs = append(errs, "run/multipart: "+err.Error())
		}
	}

	return Result{}, fmt.Errorf("image generation failed: %s", strings.Join(errs, " | "))
}

func generateOpenAIImages(ctx context.Context, p providers.Provider, m providers.Model, prompt, size string) (Result, error) {
	url := imagesGenerationsURL(p.BaseURL)
	body := map[string]any{
		"model":  m.ModelID,
		"prompt": prompt,
		"n":      1,
		"size":   size,
	}
	mid := strings.ToLower(m.ModelID)
	if strings.Contains(mid, "dall-e") {
		body["response_format"] = "b64_json"
	}

	raw, status, err := postJSON(ctx, url, p.APIKey, body)
	if err != nil {
		return Result{}, err
	}
	if status >= 400 {
		return Result{}, fmt.Errorf("status %d: %s", status, truncate(string(raw), 400))
	}
	return parseImageResponse(ctx, raw)
}

func imagesGenerationsURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/images/generations"
	}
	if strings.Contains(base, "/v1/") {
		// e.g. .../ai/v1 already
		return base + "/images/generations"
	}
	return base + "/v1/images/generations"
}

// inferenceRunURL builds {aiRoot}/run/{model} when the base looks like an
// OpenAI-compatible AI gateway that also exposes a model run path
// (…/ai/v1, …/ai, …/v1 with account-style hosts).
func inferenceRunURL(base, modelID string) (string, bool) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	modelID = strings.TrimSpace(modelID)
	if base == "" || modelID == "" {
		return "", false
	}
	root := base
	if i := strings.Index(root, "/ai/"); i >= 0 {
		root = root[:i+3] // keep ".../ai"
	} else if strings.HasSuffix(root, "/ai") {
		// already
	} else if strings.HasSuffix(root, "/v1") {
		// Generic OpenAI base — some gateways map /v1/run/{model}
		root = root
		return root + "/run/" + modelID, true
	} else {
		return "", false
	}
	return root + "/run/" + modelID, true
}

func generateRunJSON(ctx context.Context, url, apiKey, prompt string, w, h int) (Result, error) {
	body := map[string]any{
		"prompt": prompt,
		"width":  w,
		"height": h,
	}
	// Alternate shapes some gateways accept
	raw, status, err := postJSON(ctx, url, apiKey, body)
	if err != nil {
		return Result{}, err
	}
	if status >= 400 {
		return Result{}, fmt.Errorf("status %d: %s", status, truncate(string(raw), 400))
	}
	return parseImageResponse(ctx, raw)
}

func generateRunMultipart(ctx context.Context, url, apiKey, prompt string, w, h int) (Result, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("prompt", prompt)
	_ = writer.WriteField("width", fmt.Sprintf("%d", w))
	_ = writer.WriteField("height", fmt.Sprintf("%d", h))
	ct := writer.FormDataContentType()
	if err := writer.Close(); err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", ct)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	res, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 30<<20))
	if err != nil {
		return Result{}, err
	}
	if res.StatusCode >= 400 {
		return Result{}, fmt.Errorf("status %d: %s", res.StatusCode, truncate(string(raw), 400))
	}
	return parseImageResponse(ctx, raw)
}

// parseImageResponse accepts common OpenAI-compatible and gateway shapes:
//
//	{"data":[{"b64_json":"..."}]} | {"data":[{"url":"..."}]}
//	{"result":{"image":"..."}} | {"image":"..."} | {"images":["..."]}
//	{"b64_json":"..."} | raw image bytes
func parseImageResponse(ctx context.Context, raw []byte) (Result, error) {
	if len(raw) == 0 {
		return Result{}, fmt.Errorf("empty response")
	}
	// Binary image body
	if mime := sniffMime(raw); mime != "image/png" || raw[0] == 0x89 {
		if isLikelyImage(raw) {
			return Result{Data: raw, MimeType: sniffMime(raw)}, nil
		}
	}

	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		if isLikelyImage(raw) {
			return Result{Data: raw, MimeType: sniffMime(raw)}, nil
		}
		return Result{}, fmt.Errorf("non-json response: %s", truncate(string(raw), 200))
	}

	// Explicit API errors
	if errObj, ok := top["error"].(map[string]any); ok {
		if msg, _ := errObj["message"].(string); msg != "" {
			return Result{}, fmt.Errorf("%s", msg)
		}
	}
	if errs, ok := top["errors"].([]any); ok && len(errs) > 0 {
		if em, ok := errs[0].(map[string]any); ok {
			if msg, _ := em["message"].(string); msg != "" {
				return Result{}, fmt.Errorf("%s", msg)
			}
		}
	}

	// OpenAI: data[].b64_json / data[].url
	if dataArr, ok := top["data"].([]any); ok && len(dataArr) > 0 {
		if item, ok := dataArr[0].(map[string]any); ok {
			if b64, _ := item["b64_json"].(string); b64 != "" {
				return decodeB64Result(b64, strField(item, "revised_prompt"))
			}
			if u, _ := item["url"].(string); u != "" {
				data, err := download(ctx, u)
				if err != nil {
					return Result{}, err
				}
				return Result{Data: data, MimeType: sniffMime(data), Revised: strField(item, "revised_prompt")}, nil
			}
		}
	}

	// result.image / result.b64_json (Workers AI and similar)
	if result, ok := top["result"].(map[string]any); ok {
		if b64 := firstString(result, "image", "b64_json", "base64"); b64 != "" {
			return decodeB64Result(b64, "")
		}
		if u := firstString(result, "url", "image_url"); u != "" {
			data, err := download(ctx, u)
			if err != nil {
				return Result{}, err
			}
			return Result{Data: data, MimeType: sniffMime(data)}, nil
		}
	}

	// Top-level image / b64_json / images[]
	if b64 := firstString(top, "image", "b64_json", "base64"); b64 != "" {
		return decodeB64Result(b64, "")
	}
	if images, ok := top["images"].([]any); ok && len(images) > 0 {
		switch v := images[0].(type) {
		case string:
			return decodeB64Result(v, "")
		case map[string]any:
			if b64 := firstString(v, "b64_json", "image", "base64"); b64 != "" {
				return decodeB64Result(b64, "")
			}
			if u := firstString(v, "url"); u != "" {
				data, err := download(ctx, u)
				if err != nil {
					return Result{}, err
				}
				return Result{Data: data, MimeType: sniffMime(data)}, nil
			}
		}
	}

	return Result{}, fmt.Errorf("unrecognized image response shape: %s", truncate(string(raw), 300))
}

func decodeB64Result(b64, revised string) (Result, error) {
	// Strip data-URL prefix if present
	if i := strings.Index(b64, ","); i >= 0 && strings.Contains(b64[:i], "base64") {
		b64 = b64[i+1:]
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(b64)
		if err != nil {
			return Result{}, fmt.Errorf("b64 decode: %w", err)
		}
	}
	return Result{Data: data, MimeType: sniffMime(data), Revised: revised}, nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func strField(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func isLikelyImage(b []byte) bool {
	if len(b) < 8 {
		return false
	}
	if b[0] == 0x89 && b[1] == 0x50 { // PNG
		return true
	}
	if b[0] == 0xff && b[1] == 0xd8 { // JPEG
		return true
	}
	if string(b[0:4]) == "RIFF" && len(b) >= 12 && string(b[8:12]) == "WEBP" {
		return true
	}
	return false
}

func parseSize(size string) (w, h int) {
	w, h = 1024, 1024
	parts := strings.Split(strings.ToLower(strings.TrimSpace(size)), "x")
	if len(parts) == 2 {
		var a, b int
		if _, err := fmt.Sscanf(parts[0]+" "+parts[1], "%d %d", &a, &b); err == nil && a > 0 && b > 0 {
			return a, b
		}
	}
	return w, h
}

func generateGemini(ctx context.Context, p providers.Provider, m providers.Model, prompt string) (Result, error) {
	base := strings.TrimRight(p.BaseURL, "/")
	modelPath := m.ModelID
	if !strings.HasPrefix(modelPath, "models/") {
		modelPath = "models/" + modelPath
	}
	url := fmt.Sprintf("%s/%s:generateContent", base, modelPath)
	if strings.Contains(base, "generativelanguage.googleapis.com") && !strings.Contains(base, "/v1beta") {
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
	return Result{}, fmt.Errorf("gemini returned no image parts — use an image-capable model")
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
