package agent

import (
	"encoding/json"
	types "github.com/gemechischala/awaki/internal/domain"

	"github.com/rs/zerolog/log"
)

type parsedResponse struct {
	assistantBlocks []types.ContentBlock
	toolCalls       []types.ContentBlock
	textContent     string
}

// buildLLMMessages projects the agent's working set of turns into the message
// form the LLM client expects.
func buildLLMMessages(turns []types.Turn) []types.Message {
	msgs := make([]types.Message, 0, len(turns))
	for _, t := range turns {
		role := types.RoleUser
		if t.Role == string(types.RoleAssistant) {
			role = types.RoleAssistant
		}
		msgs = append(msgs, types.Message{Role: role, Content: t.Content})
	}
	return msgs
}

func parseResponseBlocks(content []types.ContentBlock) parsedResponse {
	var p parsedResponse
	for _, block := range content {
		switch block.Type {
		case "tool_use":
			p.toolCalls = append(p.toolCalls, block)
			p.assistantBlocks = append(p.assistantBlocks, block)
		case "text":
			p.textContent += block.Text
			p.assistantBlocks = append(p.assistantBlocks, block)
		}
	}
	return p
}

func unmarshalToolInput(raw json.RawMessage) map[string]any {
	var input map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &input); err != nil {
			log.Error().Err(err).Msg("failed to unmarshal tool input")
		}
	}
	return input
}
