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

func TestFormatRichHTML_ExtendedInline(t *testing.T) {
	in := "See ||secret|| and ==marked== and H~2~O and x^2^ and ++under++."
	out := FormatRichHTML(in)
	checks := []string{
		"<tg-spoiler>secret</tg-spoiler>",
		"<mark>marked</mark>",
		"<sub>2</sub>",
		"<sup>2</sup>",
		"<u>under</u>",
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	// Must not appear as raw markers
	for _, raw := range []string{"||secret||", "==marked=="} {
		if strings.Contains(out, raw) {
			t.Fatalf("raw marker still present %q in %q", raw, out)
		}
	}
	t.Log(out)
}

func TestFormatRichHTML_StrikethroughAndExtended(t *testing.T) {
	in := "has ~~strikethrough~~, ==marked==, ||spoiler||, H~2~O, x^2^, ++under++."
	out := FormatRichHTML(in)
	t.Log(out)
	for _, want := range []string{
		"<s>strikethrough</s>",
		"<mark>marked</mark>",
		"<tg-spoiler>spoiler</tg-spoiler>",
		"<sub>2</sub>",
		"<sup>2</sup>",
		"<u>under</u>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	// Must not leak raw markers or broken tails like "strikethrough=="
	for _, bad := range []string{"strikethrough==", "~~strikethrough~~", "==marked==", "||spoiler||"} {
		if strings.Contains(out, bad) {
			t.Fatalf("raw/broken %q still present in %q", bad, out)
		}
	}
}
