package providers

import "strings"

// Protocol is the wire format used to talk to a provider endpoint.
// Mirrors com.awaki.settings.model.LLMProtocol — providers are pure connection
// configuration; models reference a provider and inherit its transport.
type Protocol string

const (
	ProtocolOpenAIChat      Protocol = "openai_chat_completions"
	ProtocolOpenAIResponses Protocol = "openai_responses"
	ProtocolAnthropic       Protocol = "anthropic_messages"
	ProtocolGemini          Protocol = "google_gemini"
)

func (p Protocol) DisplayName() string {
	switch p {
	case ProtocolOpenAIChat:
		return "OpenAI Chat Completions"
	case ProtocolOpenAIResponses:
		return "OpenAI Responses"
	case ProtocolAnthropic:
		return "Anthropic Messages"
	case ProtocolGemini:
		return "Google Gemini"
	default:
		return string(p)
	}
}

// Format maps protocol → llm adapter format string used by internal/llm.
func (p Protocol) Format() string {
	switch p {
	case ProtocolAnthropic:
		return "anthropic"
	case ProtocolGemini:
		return "gemini"
	default:
		// OpenAI chat completions and responses both use the OpenAI-compatible adapter for now
		return "openai"
	}
}

func ParseProtocol(raw string) (Protocol, bool) {
	switch Protocol(raw) {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolAnthropic, ProtocolGemini:
		return Protocol(raw), true
	}
	// aliases
	switch raw {
	case "openai", "openai_chat", "chat_completions":
		return ProtocolOpenAIChat, true
	case "anthropic", "claude":
		return ProtocolAnthropic, true
	case "gemini", "google", "google_gemini":
		return ProtocolGemini, true
	case "responses", "openai_responses":
		return ProtocolOpenAIResponses, true
	}
	return "", false
}

// Capabilities mirrors ModelCapabilities from Awaki.
type Capabilities struct {
	Tools              bool `json:"tools"`
	Images             bool `json:"images"`
	ParallelToolCalls  bool `json:"parallel_tool_calls"`
	PromptCaching      bool `json:"prompt_caching"`
	MaxTokensParameter bool `json:"max_tokens_parameter"`
	Streaming          bool `json:"streaming"`
}

// Provider is pure connection configuration (Awaki AIProvider + stored API key).
// BaseURL and APIKey are never hardcoded by protocol — the operator sets them.
type Provider struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	BaseURL  string   `json:"base_url"`
	Protocol Protocol `json:"protocol"`
	APIKey   string   `json:"api_key"` // stored in data dir; treat as secret
}

// Model is a selectable model record bound to a provider (Awaki AIModel).
type Model struct {
	ID              string       `json:"id"`
	ProviderID      string       `json:"provider_id"`
	ModelID         string       `json:"model_id"` // wire id sent to the API
	DisplayName     string       `json:"display_name"`
	ContextWindow   int          `json:"context_window,omitempty"`
	MaxOutputTokens int          `json:"max_output_tokens,omitempty"`
	Capabilities    Capabilities `json:"capabilities"`
}

// ConfigFile is the on-disk JSON document.
type ConfigFile struct {
	ActiveModelID string     `json:"active_model_id"`
	Providers     []Provider `json:"providers"`
	Models        []Model    `json:"models"`
}

// MaskKey redacts an API key for display (never log or show full secrets).
func MaskKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "(empty)"
	}
	if len(key) <= 8 {
		return "••••"
	}
	return key[:4] + "…" + key[len(key)-4:]
}

// AllProtocols lists supported wire protocols for UI selection.
func AllProtocols() []Protocol {
	return []Protocol{
		ProtocolOpenAIChat,
		ProtocolOpenAIResponses,
		ProtocolAnthropic,
		ProtocolGemini,
	}
}
