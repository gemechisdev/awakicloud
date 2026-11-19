package rich

import (
	"strings"
	"testing"
)

func TestEscapeHTML(t *testing.T) {
	in := "a <b> & \"c\" 'd'"
	out := EscapeHTML(in)
	if strings.Contains(out, "<") || strings.Contains(out, ">") {
		t.Fatalf("unescaped brackets: %q", out)
	}
	if !strings.Contains(out, "&amp;") {
		t.Fatalf("expected &amp; in %q", out)
	}
}

func TestInlineMDEscapesThenFormats(t *testing.T) {
	in := "say **hello** <script> and `code`"
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
		t.Fatalf("bold missing: %q", html)
	}
}

func TestMarkdownToRichHTML_SoftLinesAsParagraphs(t *testing.T) {
	md := "Commands:\n\n/start — main\n/help — list"
	html := MarkdownToRichHTML(md)
	if strings.Count(html, "<p>") < 2 {
		t.Fatalf("expected multiple paragraphs: %q", html)
	}
}
