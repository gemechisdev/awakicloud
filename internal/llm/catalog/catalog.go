package catalog

import (
	_ "embed"
	"encoding/json"
	"strconv"
)

type ModelDef struct {
	ID              string  `json:"id"`
	Label           string  `json:"label"`
	ContextWindow   int     `json:"context_window"`
	Vision          bool    `json:"vision"`
	InputPricePerM  float64 `json:"input_price_per_m"`
	OutputPricePerM float64 `json:"output_price_per_m"`
}

//go:embed openai.json
var openaiJSON []byte

//go:embed anthropic.json
var anthropicJSON []byte

//go:embed openrouter_models.json
var openrouterJSON []byte

func OpenAI() map[string]ModelDef     { return loadClean(openaiJSON) }
func Anthropic() map[string]ModelDef  { return loadClean(anthropicJSON) }
func OpenRouter() map[string]ModelDef { return loadOpenRouter(openrouterJSON) }

func loadClean(data []byte) map[string]ModelDef {
	var models []ModelDef
	if err := json.Unmarshal(data, &models); err != nil {
		return nil
	}
	m := make(map[string]ModelDef, len(models))
	for _, def := range models {
		m[def.ID] = def
	}
	return m
}

// OpenRouter API returns a different schema; normalize into ModelDef.
func loadOpenRouter(data []byte) map[string]ModelDef {
	var raw struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
			Pricing       struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			Architecture struct {
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	m := make(map[string]ModelDef, len(raw.Data))
	for _, r := range raw.Data {
		inputPrice, _ := strconv.ParseFloat(r.Pricing.Prompt, 64)
		outputPrice, _ := strconv.ParseFloat(r.Pricing.Completion, 64)
		vision := false
		for _, mod := range r.Architecture.InputModalities {
			if mod == "image" {
				vision = true
				break
			}
		}
		m[r.ID] = ModelDef{
			ID:              r.ID,
			Label:           r.Name,
			ContextWindow:   r.ContextLength,
			Vision:          vision,
			InputPricePerM:  inputPrice * 1_000_000,
			OutputPricePerM: outputPrice * 1_000_000,
		}
	}
	return m
}
