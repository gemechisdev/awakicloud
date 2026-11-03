package system

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/imagegen"
	"github.com/gemechischala/awaki/internal/providers"
	"github.com/gemechischala/awaki/internal/tools/types"
	"github.com/gemechischala/awaki/internal/transport"
)

// ImageGenTools returns the generate_image tool. The agent writes a detailed
// prompt; this tool calls the configured image model and delivers the result
// to the user as a photo (via MediaEvent).
func ImageGenTools(store *providers.Store) []types.Tool {
	if store == nil {
		return nil
	}
	return []types.Tool{
		{
			Name:  "generate_image",
			Label: "Generating image",
			Description: "Generate an image from a text prompt and deliver it to the user. " +
				"Write a highly detailed, specific prompt yourself — expand the user's intent with subject, composition, lighting, style, mood, and quality cues. " +
				"Keep the prompt concise but rich (1–4 sentences). Prefer this tool whenever the user asks for a picture, illustration, logo, mockup, or visual. " +
				"Do not claim you cannot generate images if this tool is available.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{
						"type":        "string",
						"description": "Detailed image prompt. Expand user intent: subject, setting, style, lighting, camera/angle, colors, mood. No markdown.",
					},
					"size": map[string]any{
						"type":        "string",
						"description": "Optional size for OpenAI-compatible APIs: 1024x1024 (default), 1536x1024, 1024x1536, 1792x1024, 1024x1792.",
						"enum":        []string{"1024x1024", "1536x1024", "1024x1536", "1792x1024", "1024x1792"},
					},
					"caption": map[string]any{
						"type":        "string",
						"description": "Optional short caption shown with the image to the user.",
					},
				},
				"required": []string{"prompt"},
			},
			Required: []string{"prompt"},
			Handler: func(ctx context.Context, input map[string]any) types.Result {
				prompt, _ := input["prompt"].(string)
				prompt = strings.TrimSpace(prompt)
				if prompt == "" {
					return types.Errf("prompt is required")
				}
				size, _ := input["size"].(string)
				caption, _ := input["caption"].(string)

				m, p, err := store.ResolveImageGen()
				if err != nil {
					return types.ErrResult(err)
				}

				res, err := imagegen.Generate(ctx, p, m, prompt, size)
				if err != nil {
					return types.ErrResult(err)
				}
				if len(res.Data) == 0 {
					return types.Errf("empty image data")
				}

				filename := "generated.png"
				switch res.MimeType {
				case "image/jpeg":
					filename = "generated.jpg"
				case "image/webp":
					filename = "generated.webp"
				}

				if sink, ok := transport.SinkFromContext(ctx); ok {
					sink.Emit(transport.MediaEvent{
						Filename: filename,
						MimeType: res.MimeType,
						Data:     res.Data,
						Caption:  caption,
					})
				}

				msg := fmt.Sprintf("Image generated with %s (%s) and sent to the user.", m.DisplayName, m.ModelID)
				if res.Revised != "" {
					msg += "\nRevised prompt: " + res.Revised
				}
				return types.Result{Output: msg}
			},
		},
	}
}
