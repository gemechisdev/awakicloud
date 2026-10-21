package providers

import (
	"fmt"
	"time"

	"github.com/gemechischala/awaki/internal/llm"
)

// ToLLMProvider builds an llm.Provider strictly from stored connection config.
// No base URL or API key is invented from the protocol name — both must be on
// the Provider record (same rule as the Awaki Android app).
func ToLLMProvider(p Provider, m Model, timeout time.Duration) (*llm.Provider, error) {
	if p.BaseURL == "" {
		return nil, fmt.Errorf("provider %q has empty base_url", p.Name)
	}
	if p.APIKey == "" {
		return nil, fmt.Errorf("provider %q has empty api_key", p.Name)
	}
	if m.ModelID == "" {
		return nil, fmt.Errorf("model has empty model_id")
	}
	cw := m.ContextWindow
	if cw <= 0 {
		cw = 128000
	}
	label := m.DisplayName
	if label == "" {
		label = m.ModelID
	}
	return &llm.Provider{
		ID:             m.ID,
		Label:          fmt.Sprintf("%s / %s", p.Name, label),
		Model:          m.ModelID,
		BaseURL:        p.BaseURL,
		APIKey:         p.APIKey,
		Format:         p.Protocol.Format(),
		ContextWindow:  cw,
		SupportsVision: m.Capabilities.Images,
		Timeout:        timeout,
	}, nil
}
