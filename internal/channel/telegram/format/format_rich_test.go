package format

import (
	"strings"
	"testing"
)

func TestFormatRichHTML_PreservesSoftLineBreaks(t *testing.T) {
	in := "Commands:\n\n/start — main menu\n/new — new chat\n/help — this list\n"
	out := FormatRichHTML(in)
	if out == "" {
		t.Fatal("empty output")
	}
	if !strings.Contains(out, "<br") && !strings.Contains(out, "<p>") {
		t.Fatalf("expected structural tags, got: %q", out)
	}
	if strings.Contains(out, "/start — main menu /new") {
		t.Fatalf("soft breaks collapsed: %q", out)
	}
	if !strings.Contains(out, "/start") || !strings.Contains(out, "/new") {
		t.Fatalf("missing commands: %q", out)
	}
}

func TestFormatRichHTML_ParagraphsAndBold(t *testing.T) {
	in := "👋 **Welcome to Awaki**\n\nYour self-hosted agent.\n\n**Quick start**\n1. Add a key\n2. Pick a model"
	out := FormatRichHTML(in)
	if !strings.Contains(out, "Welcome to Awaki") {
		t.Fatalf("missing title: %q", out)
	}
	if !strings.Contains(out, "<p>") {
		t.Fatalf("expected <p> blocks: %q", out)
	}
}

func TestFormatRichHTML_Lists(t *testing.T) {
	in := "Useful\n\n- /mode — permissions\n- /help — list"
	out := FormatRichHTML(in)
	if !strings.Contains(out, "<ul>") || !strings.Contains(out, "<li>") {
		t.Fatalf("expected ul/li, got: %q", out)
	}
}
