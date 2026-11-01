package types

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const MaxOutputSize = 50 * 1024

type Result struct {
	Output string
	IsErr  bool
}

func ErrResult(err error) Result {
	return Result{Output: err.Error(), IsErr: true}
}

func Errf(format string, args ...any) Result {
	return Result{Output: fmt.Sprintf(format, args...), IsErr: true}
}

// CutHead returns s limited to at most max bytes without splitting a UTF-8 rune.
func CutHead(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// CutTail returns the last max bytes of s without splitting a UTF-8 rune.
func CutTail(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	start := len(s) - max
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// TruncateOutput returns a Result whose Output keeps the first MaxOutputSize
// bytes (rune-safe) and says how much was dropped.
func TruncateOutput(s string) Result {
	if len(s) > MaxOutputSize {
		head := CutHead(s, MaxOutputSize)
		return Result{Output: head + fmt.Sprintf("\n... (truncated: %d more bytes)", len(s)-len(head))}
	}
	return Result{Output: s}
}

// TruncateHeadTail keeps the beginning and (mostly) the end of s, dropping the
// middle. Use for command/build/test logs where the failure is at the bottom.
// Cuts are moved to line boundaries when one is close.
func TruncateHeadTail(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	headN := max * 3 / 10
	tailN := max - headN
	head := CutHead(s, headN)
	if i := strings.LastIndexByte(head, '\n'); i > headN*8/10 {
		head = head[:i+1]
	}
	tail := CutTail(s, tailN)
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i < tailN*2/10 {
		tail = tail[i+1:]
	}
	omitted := len(s) - len(head) - len(tail)
	return head + fmt.Sprintf("\n… [%d bytes omitted from the middle] …\n", omitted) + tail
}

type Handler func(ctx context.Context, input map[string]any) Result

type Tool struct {
	Name        string
	Label       string // human-readable label for UI (e.g. "Running command")
	Description string
	Parameters  map[string]any
	Required    []string
	Handler     Handler

	Interactive bool                              // agent loop handles user interaction; handler is skipped
	Phase       string                            // UI phase override (e.g. "planning"); empty = default "tool"
	DetailFunc  func(input map[string]any) string // extracts short context from input for UI display
}

func GetString(input map[string]any, key string) (string, error) {
	v, _ := input[key].(string)
	if v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return v, nil
}

func GetStringOpt(input map[string]any, key string) string {
	v, _ := input[key].(string)
	return v
}

func GetInt(input map[string]any, key string) (int, error) {
	v, ok := input[key].(float64)
	if !ok {
		return 0, fmt.Errorf("%s is required", key)
	}
	return int(v), nil
}

func GetBool(input map[string]any, key string) bool {
	v, _ := input[key].(bool)
	return v
}

// GetIntOpt reads an optional integer that models may send as a JSON number or
// a numeric string. ok is false when the key is missing or not numeric.
func GetIntOpt(input map[string]any, key string) (n int, ok bool) {
	switch v := input[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return i, true
		}
	}
	return 0, false
}

// GetBoolDefault reads an optional boolean (bool, or "true"/"false" strings).
func GetBoolDefault(input map[string]any, key string, def bool) bool {
	switch v := input[key].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	return def
}
