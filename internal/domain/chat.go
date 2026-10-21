package domain

import (
	"context"
	"time"
)

// Turn is a single message in the LLM conversation context.
// Rich format: includes tool_use, tool_result, images — everything the LLM sees.
type Turn struct {
	Role      string // "user" | "assistant"
	Content   []ContentBlock
	Timestamp time.Time
	Usage     *Usage
	Metadata  map[string]any
}

// Usage tracks token consumption and cost for a run.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	TotalTokens      int
	LLMCalls         int
	ToolCalls        int
	Cost             float64 // estimated USD
	Duration         time.Duration
}

func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.TotalTokens += other.TotalTokens
	u.LLMCalls += other.LLMCalls
	u.ToolCalls += other.ToolCalls
	u.Cost += other.Cost
	u.Duration += other.Duration
}

// ChatInfo is a lightweight summary view of a stored chat.
type ChatInfo struct {
	ID        string
	Title     string
	Summary   string
	Tags      []string
	Status    string
	StartedAt time.Time
	EndedAt   time.Time
}

// ChatSearchFunc is a callback that searches past chats by query.
type ChatSearchFunc func(ctx context.Context, query string) ([]ChatInfo, error)
