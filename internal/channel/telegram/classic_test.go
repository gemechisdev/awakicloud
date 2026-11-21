package telegram

import (
	"errors"
	"strings"
	"testing"
)

func TestClassicHTMLHasNoRichOnlyTags(t *testing.T) {
	md := "📂 `proj`\n\n3 entries\n\n- 📄 `a.go` · 1.0 KB\n- 📁 `src/`\n\n```go\nx := 1\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |"
	out := classicHTML(md)
	for _, bad := range []string{"<p>", "</p>", "<table", "<tg-", "<ul", "<li"} {
		if strings.Contains(out, bad) {
			t.Fatalf("classic HTML contains %q:\n%s", bad, out)
		}
	}
	if out == "" || !strings.Contains(out, "proj") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestIsNotModified(t *testing.T) {
	if !isNotModified(errors.New("bad request, Bad Request: message is not modified: specified new message content…")) {
		t.Fatal("should detect")
	}
	if isNotModified(nil) || isNotModified(errors.New("can't parse entities")) {
		t.Fatal("false positive")
	}
}
