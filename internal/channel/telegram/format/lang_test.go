package format

import (
	"strings"
	"testing"
)

func TestFenceLanguageBecomesHighlightClass(t *testing.T) {
	out := FormatRichHTML("```go\nfmt.Println(1)\n```")
	if !strings.Contains(out, `<pre><code class="language-go">`) {
		t.Fatalf("language class missing: %q", out)
	}
	if plain := FormatRichHTML("```\nx\n```"); !strings.Contains(plain, "<pre><code>") {
		t.Fatalf("plain fence changed: %q", plain)
	}
	if bad := FormatRichHTML("```\"><script>\nx\n```"); strings.Contains(bad, "script") && strings.Contains(bad, "class=") {
		t.Fatalf("unsafe language leaked: %q", bad)
	}
}

func TestSafeCodeLang(t *testing.T) {
	for in, want := range map[string]string{"Go": "go", "c++": "c++", "c#": "c#", "go title=x": "go", "": "", "a b\"c": "a", "x\"y": ""} {
		if got := safeCodeLang([]byte(in)); got != want {
			t.Errorf("safeCodeLang(%q) = %q, want %q", in, got, want)
		}
	}
}
