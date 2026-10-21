package llm

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gemechischala/awaki/internal/llm/catalog"
)

// Provider is a resolved connection + model used for one agent run.
type Provider struct {
	ID              string        `json:"id"`
	Label           string        `json:"label"`
	Model           string        `json:"model"` // wire model id sent to the API
	BaseURL         string        `json:"base_url,omitempty"`
	APIKey          string        `json:"-"`
	Format          string        `json:"format,omitempty"` // "openai" | "anthropic" | "gemini"
	ContextWindow   int           `json:"context_window"`
	SupportsVision  bool          `json:"supports_vision,omitempty"`
	InputPricePerM  float64       `json:"-"`
	OutputPricePerM float64       `json:"-"`
	Timeout         time.Duration `json:"-"`
}

var aliases = map[string]string{
	"claude":   "claude-sonnet-4-6",
	"deepseek": "deepseek/deepseek-v4-flash",
	"gemini":   "gemini-2.5-flash",
	"grok":     "x-ai/grok-build-0.1",
	"minimax":  "minimax/minimax-m2.5",
	"qwen":     "qwen/qwen3.7-max",
	"llama":    "meta-llama/llama-4-maverick",
	"kimi":     "moonshotai/kimi-k2.5",
	"openai":   "gpt-4o",
}

var anthropicToOpenRouter = map[string]string{
	"claude-opus-4-8":   "anthropic/claude-opus-4.8",
	"claude-opus-4-7":   "anthropic/claude-opus-4.7",
	"claude-opus-4-6":   "anthropic/claude-opus-4.6",
	"claude-sonnet-4-6": "anthropic/claude-sonnet-4.6",
	"claude-opus-4-5":   "anthropic/claude-opus-4.5",
	"claude-sonnet-4-5": "anthropic/claude-sonnet-4.5",
	"claude-haiku-4-5":  "anthropic/claude-haiku-4.5",
	"claude-sonnet-4":   "anthropic/claude-sonnet-4",
	"claude-haiku-3-5":  "anthropic/claude-haiku-3.5",
}

// Built-in Gemini models (static fallback; live list can be fetched later).
var geminiCatalog = map[string]catalog.ModelDef{
	"gemini-2.5-pro": {
		ID: "gemini-2.5-pro", Label: "Gemini 2.5 Pro",
		ContextWindow: 1048576, Vision: true,
	},
	"gemini-2.5-flash": {
		ID: "gemini-2.5-flash", Label: "Gemini 2.5 Flash",
		ContextWindow: 1048576, Vision: true,
	},
	"gemini-2.0-flash": {
		ID: "gemini-2.0-flash", Label: "Gemini 2.0 Flash",
		ContextWindow: 1048576, Vision: true,
	},
	"gemini-1.5-pro": {
		ID: "gemini-1.5-pro", Label: "Gemini 1.5 Pro",
		ContextWindow: 2097152, Vision: true,
	},
	"gemini-1.5-flash": {
		ID: "gemini-1.5-flash", Label: "Gemini 1.5 Flash",
		ContextWindow: 1048576, Vision: true,
	},
}

var (
	anthropicOnce     sync.Once
	anthropicCatalog  map[string]catalog.ModelDef
	openaiOnce        sync.Once
	openaiCatalog     map[string]catalog.ModelDef
	openrouterOnce    sync.Once
	openrouterCatalog map[string]catalog.ModelDef
)

func getAnthropicCatalog() map[string]catalog.ModelDef {
	anthropicOnce.Do(func() { anthropicCatalog = catalog.Anthropic() })
	return anthropicCatalog
}

func getOpenAICatalog() map[string]catalog.ModelDef {
	openaiOnce.Do(func() { openaiCatalog = catalog.OpenAI() })
	return openaiCatalog
}

func getOpenRouterCatalog() map[string]catalog.ModelDef {
	openrouterOnce.Do(func() { openrouterCatalog = catalog.OpenRouter() })
	return openrouterCatalog
}

// ResolveProvider builds a Provider from model ID and provider name.
// Supported providers: anthropic, openai, gemini, openrouter, custom.
func ResolveProvider(modelID, provider string) (*Provider, error) {
	if resolved, ok := aliases[modelID]; ok {
		modelID = resolved
	}
	provider = strings.ToLower(strings.TrimSpace(provider))

	switch provider {
	case "anthropic":
		if strings.Contains(modelID, "/") {
			return nil, fmt.Errorf("model %q looks like an OpenRouter slug — use AWAKI_PROVIDER=openrouter", modelID)
		}
		if _, ok := getAnthropicCatalog()[modelID]; !ok {
			// Allow unknown Anthropic ids (user may have access to newer models)
			return resolveAnthropicUnknown(modelID)
		}
		return resolveAnthropic(modelID)

	case "openai":
		if strings.Contains(modelID, "/") && !strings.HasPrefix(modelID, "gpt-") {
			return nil, fmt.Errorf("model %q looks like an OpenRouter slug — use AWAKI_PROVIDER=openrouter", modelID)
		}
		if _, ok := getOpenAICatalog()[modelID]; !ok {
			return resolveOpenAIUnknown(modelID)
		}
		return resolveOpenAI(modelID)

	case "gemini", "google", "google_gemini":
		return resolveGemini(modelID)

	case "openrouter":
		if _, ok := getAnthropicCatalog()[modelID]; ok {
			if orSlug, ok := anthropicToOpenRouter[modelID]; ok {
				return resolveOpenRouter(modelID, orSlug)
			}
			return nil, fmt.Errorf("model %q has no OpenRouter equivalent", modelID)
		}
		if _, ok := getOpenAICatalog()[modelID]; ok {
			return resolveOpenRouter(modelID, "openai/"+modelID)
		}
		if !strings.Contains(modelID, "/") {
			return nil, fmt.Errorf("unknown model %q — use a full OpenRouter slug (vendor/model)", modelID)
		}
		return resolveOpenRouter(modelID, modelID)

	case "custom", "openai-compatible", "openai_compatible":
		return resolveCustom(modelID)

	default:
		return nil, fmt.Errorf("unknown provider %q — use anthropic, openai, gemini, openrouter, or custom", provider)
	}
}

func resolveAnthropic(model string) (*Provider, error) {
	apiKey := firstEnv("ANTHROPIC_API_KEY", "AWAKI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set for model %q", model)
	}
	base := firstEnv("ANTHROPIC_BASE_URL", "AWAKI_BASE_URL")
	// empty base_url is allowed only if the Anthropic SDK has a built-in default;
	// prefer explicit AWAKI_BASE_URL. Empty still works with anthropic-sdk-go.
	if def, ok := getAnthropicCatalog()[model]; ok {
		return providerFromDef(def, model, apiKey, base, "anthropic"), nil
	}
	return resolveAnthropicUnknown(model)
}

func resolveAnthropicUnknown(model string) (*Provider, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set for model %q", model)
	}
	return &Provider{
		ID: model, Label: model, Model: model,
		APIKey: apiKey, Format: "anthropic",
		ContextWindow: 200000, SupportsVision: true,
	}, nil
}

func resolveOpenAI(model string) (*Provider, error) {
	apiKey := firstEnv("OPENAI_API_KEY", "AWAKI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY not set for model %q", model)
	}
	base := firstEnv("OPENAI_BASE_URL", "AWAKI_BASE_URL")
	if base == "" {
		return nil, fmt.Errorf("OPENAI_BASE_URL (or AWAKI_BASE_URL) required — no hardcoded endpoint (Awaki-style)")
	}
	if def, ok := getOpenAICatalog()[model]; ok {
		return providerFromDef(def, model, apiKey, base, "openai"), nil
	}
	return resolveOpenAIUnknown(model)
}

func resolveOpenAIUnknown(model string) (*Provider, error) {
	apiKey := firstEnv("OPENAI_API_KEY", "AWAKI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY not set for model %q", model)
	}
	base := firstEnv("OPENAI_BASE_URL", "AWAKI_BASE_URL")
	if base == "" {
		return nil, fmt.Errorf("OPENAI_BASE_URL (or AWAKI_BASE_URL) required — no hardcoded endpoint")
	}
	return &Provider{
		ID: model, Label: model, Model: model,
		BaseURL: base, APIKey: apiKey, Format: "openai",
		ContextWindow: 128000, SupportsVision: true,
	}, nil
}

func resolveGemini(model string) (*Provider, error) {
	apiKey := firstEnv("GEMINI_API_KEY", "GOOGLE_API_KEY", "AWAKI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY not set for model %q", model)
	}
	model = strings.TrimPrefix(model, "models/")
	model = strings.TrimPrefix(model, "google/")

	def, ok := geminiCatalog[model]
	if !ok {
		def = catalog.ModelDef{ID: model, Label: model, ContextWindow: 1048576, Vision: true}
	}
	base := firstEnv("GEMINI_BASE_URL", "AWAKI_BASE_URL")
	if base == "" {
		return nil, fmt.Errorf("GEMINI_BASE_URL (or AWAKI_BASE_URL) required — no hardcoded endpoint")
	}
	return &Provider{
		ID: def.ID, Label: def.Label, Model: def.ID,
		BaseURL: base, APIKey: apiKey, Format: "gemini",
		ContextWindow: def.ContextWindow, SupportsVision: def.Vision,
	}, nil
}

func resolveOpenRouter(id, slug string) (*Provider, error) {
	apiKey := firstEnv("OPENROUTER_API_KEY", "AWAKI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("OPENROUTER_API_KEY not set for model %q", id)
	}
	base := firstEnv("OPENROUTER_BASE_URL", "AWAKI_BASE_URL")
	if base == "" {
		return nil, fmt.Errorf("OPENROUTER_BASE_URL (or AWAKI_BASE_URL) required — no hardcoded endpoint")
	}
	p := &Provider{
		ID: id, Model: slug, Label: slug,
		BaseURL: base,
		APIKey:  apiKey, Format: "openai",
		ContextWindow: 128000,
	}
	if def, ok := getOpenRouterCatalog()[slug]; ok {
		p.Label = def.Label
		p.ContextWindow = def.ContextWindow
		p.SupportsVision = def.Vision
		p.InputPricePerM = def.InputPricePerM
		p.OutputPricePerM = def.OutputPricePerM
	}
	return p, nil
}

func resolveCustom(model string) (*Provider, error) {
	base := firstEnv("AWAKI_BASE_URL", "OPENAI_BASE_URL")
	if base == "" {
		return nil, fmt.Errorf("custom provider requires AWAKI_BASE_URL (or OPENAI_BASE_URL)")
	}
	apiKey := firstEnv("AWAKI_API_KEY", "OPENAI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("custom provider requires AWAKI_API_KEY (or OPENAI_API_KEY)")
	}
	if model == "" || model == "default" {
		model = firstEnv("AWAKI_MODEL", "GOGOGOT_MODEL")
		if model == "" {
			model = "default"
		}
	}
	return &Provider{
		ID: model, Label: "custom/" + model, Model: model,
		BaseURL: base, APIKey: apiKey, Format: "openai",
		ContextWindow: 128000, SupportsVision: true,
	}, nil
}

func providerFromDef(def catalog.ModelDef, model, apiKey, baseURL, format string) *Provider {
	return &Provider{
		ID: model, Label: def.Label, Model: model,
		BaseURL: baseURL, APIKey: apiKey, Format: format,
		ContextWindow: def.ContextWindow, SupportsVision: def.Vision,
		InputPricePerM: def.InputPricePerM, OutputPricePerM: def.OutputPricePerM,
	}
}

func catalogKeys(m map[string]catalog.ModelDef) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return strings.Join(keys, ", ")
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
