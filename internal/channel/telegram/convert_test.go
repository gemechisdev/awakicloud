package telegram

import (
	"testing"

	"github.com/go-telegram/bot/models"
)

// These pin down the quote-extraction contract:
//   - same-chat reply text/caption is picked up
//   - external reply is labelled when no text is available
//   - TextQuote always wins over the full message text (it's the
//     deliberately-highlighted slice, not the whole thing)
//   - empty message returns empty
//   - formatQuotedBlock truncates to the tail, preserves newlines,
//     and always emits the "Replying to:" prefix

func TestExtractQuote_SameChatReply(t *testing.T) {
	msg := &models.Message{
		ID: 42,
		ReplyToMessage: &models.Message{
			ID:   41,
			Text: "the agent's earlier response",
		},
	}
	if got := extractQuote(msg); got != "the agent's earlier response" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractQuote_CaptionOnlyReply(t *testing.T) {
	msg := &models.Message{
		ReplyToMessage: &models.Message{
			ID:      41,
			Caption: "photo caption",
		},
	}
	if got := extractQuote(msg); got != "photo caption" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractQuote_QuoteOverridesFullText(t *testing.T) {
	msg := &models.Message{
		ReplyToMessage: &models.Message{
			ID:   41,
			Text: "line one\nline two\nline three",
		},
		Quote: &models.TextQuote{
			Text: "line two",
		},
	}
	// TextQuote is the exact substring the user highlighted, so it
	// must win over the full message.
	if got := extractQuote(msg); got != "line two" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractQuote_ExternalReplyLabelled(t *testing.T) {
	msg := &models.Message{
		ExternalReply: &models.ExternalReplyInfo{
			Chat: &models.Chat{
				ID:    999,
				Title: "Some Channel",
			},
			MessageID: 7,
		},
	}
	got := extractQuote(msg)
	if got == "" {
		t.Fatal("external reply should produce a label")
	}
	if got != "(external reply to Some Channel, message #7)" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractQuote_NoReply(t *testing.T) {
	msg := &models.Message{Text: "plain"}
	if got := extractQuote(msg); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := extractQuote(nil); got != "" {
		t.Fatalf("nil msg should give empty, got %q", got)
	}
}

func TestFormatQuotedBlock_BasicShape(t *testing.T) {
	out := formatQuotedBlock("alpha\nbeta")
	want := "> Replying to:\n> alpha\n> beta"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestFormatQuotedBlock_TruncatesToTail(t *testing.T) {
	// 2000 lines of 10 chars each = 20000 chars. Last 4000 chars ≈ last 400 lines.
	// "line 20000" (the last line, 9 chars) must be present. "line 0" must be gone.
	long := ""
	for i := 0; i < 20000; i++ {
		long += "L" + itoa(i) + "\n" // "L0\n" = 3 chars, "L99999\n" = 7 chars
	}
	out := formatQuotedBlock(long)
	if len(out) > 6000 {
		t.Fatalf("block not bounded: %d chars", len(out))
	}
	if !contains(out, "L"+itoa(19999)) {
		t.Errorf("tail lost; last line L19999 should survive (output was %d chars)", len(out))
	}
	if contains(out, "L0\n") {
		t.Errorf("head should be trimmed on overflow")
	}
}

func TestFormatQuotedBlock_Empty(t *testing.T) {
	// An empty quoted string is never called (extractQuote filters it out),
	// but the helper must still produce the header line without panicking.
	out := formatQuotedBlock("")
	if !contains(out, "> Replying to:") {
		t.Fatalf("header missing: %q", out)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// itoa avoids fmt import in the test.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}
