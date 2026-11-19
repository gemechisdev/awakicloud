package rich

import (
	"strings"
	"testing"
)

func TestEscapeHTML(t *testing.T) {
	in := `a <b> & "c" 'd'`
	out := EscapeHTML(in)
	if strings.Contains(out, "<") || strings.Contains(out, ">") {
		t.Fatalf("unescaped brackets: %q", out)
	}
	if !strings.Contains(out, "&amp;") {
		t.Fatalf("expected &amp; in %q", out)
	}
}

func TestInlineMDEscapesThenFormats(t *testing.T) {
	in := `say **hello** <script> and \`code\``
	out := inlineMDToHTML(in)
	if strings.Contains(out, "<script>") {
		t.Fatalf("script not escaped: %q", out)
	}
	if !strings.Contains(out, "<b>hello</b>") {
		t.Fatalf("bold missing: %q", out)
	}
	if !strings.Contains(out, "<code>code</code>") {
		t.Fatalf("code missing: %q", out)
	}
}

func TestMarkdownToRichHTML_HeadingsAndList(t *testing.T) {
	md := "# Title\n\n- item one\n- item two\n\nPlain **bold**."
	html := MarkdownToRichHTML(md)
	if !strings.Contains(html, "<h1>Title</h1>") {
		t.Fatalf("heading missing: %q", html)
	}
	if !strings.Contains(html, "<li>") {
		t.Fatalf("list missing: %q", html)
	}
	if !strings.Contains(html, "<b>bold</b>") {
		t.Fatalf("inline bold missing: %q", html)
	}
}

func TestCharLenAndValidate(t *testing.T) {
	if CharLen("café") != 4 {
		t.Fatalf("CharLen café = %d", CharLen("café"))
	}
	err := ValidateRichMessage("<p>hi</p>", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateRichMessage("<p>hi</p>", "also", nil)
	if err == nil {
		t.Fatal("expected dual-mode error")
	}
}

func TestDetailsPlainEscapes(t *testing.T) {
	out := DetailsPlain("sum <x>", "line <y>\nline & z", false)
	if strings.Contains(out, "<y>") || strings.Contains(out, "<x>") {
		t.Fatalf("not escaped: %q", out)
	}
	if !strings.Contains(out, "<p>") {
		t.Fatalf("expected paragraphs: %q", out)
	}
}

func TestStripThinking(t *testing.T) {
	in := `<tg-thinking>wait</tg-thinking><p>done</p>`
	out := StripThinking(in)
	if strings.Contains(out, "tg-thinking") {
		t.Fatalf("thinking not stripped: %q", out)
	}
	if !strings.Contains(out, "<p>done</p>") {
		t.Fatalf("body lost: %q", out)
	}
}

func TestNeedsRTL(t *testing.T) {
	if NeedsRTL("hello world") {
		t.Fatal("latin should be false")
	}
	if !NeedsRTL("سلام دنیا این یک متن فارسی است برای تست") {
		t.Fatal("persian should be true")
	}
}
