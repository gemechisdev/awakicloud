package llm

import (
	"context"
	"fmt"
)

// Unconfigured is a placeholder LLM used when no provider/model is selected yet.
// The bot still starts so the owner can run /provider_add and /model_add.
type Unconfigured struct{}

func (Unconfigured) Call(context.Context, []Message, CallOptions) (*Response, error) {
	return nil, fmt.Errorf("no LLM configured — use /providers and /provider_add then /model_add")
}

func (Unconfigured) ModelID() string        { return "unconfigured" }
func (Unconfigured) ModelLabel() string     { return "not configured" }
func (Unconfigured) ContextWindow() int     { return 0 }
func (Unconfigured) InputPricePerM() float64  { return 0 }
func (Unconfigured) OutputPricePerM() float64 { return 0 }
func (Unconfigured) SupportsContinuation() bool { return false }

// Ensure Unconfigured implements LLM.
var _ LLM = Unconfigured{}
