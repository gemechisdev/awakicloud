package llm

import (
	"context"
	"os"
	"strings"
)

// ContinuationEnabled reports the global default (ON unless explicitly disabled).
func ContinuationEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("AWAKI_CONTINUATION")))
	if v == "" {
		return true
	}
	return v != "0" && v != "false" && v != "off" && v != "no"
}

// Continuer is implemented by adapters that can chain server-side conversation state.
type Continuer interface {
	SupportsContinuation() bool
	// CallContinue sends only newMessages while previousID anchors server history.
	CallContinue(ctx context.Context, model, systemPrompt, previousID string,
		newMessages []Message, tools []ToolDef, maxTokens int,
	) (*Response, error)
}
