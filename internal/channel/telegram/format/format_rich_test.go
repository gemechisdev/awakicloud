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
}

func TestFormatRichHTML_ListItems(t *testing.T) {
	in := "**Commands**\n\n- `/start` — main menu\n- `/help` — this list\n\n**Permission:** `/mode auto`"
	out := FormatRichHTML(in)
	if !strings.Contains(out, "<ul>") || !strings.Contains(out, "<li>") {
		t.Fatalf("expected ul/li: %q", out)
	}
	t.Log(out)
}
