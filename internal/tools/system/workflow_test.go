package system

import (
	"os/exec"
	"strings"
	"testing"
)

// An agent session end to end: locate -> read a window -> edit -> patch ->
// reorganise -> review -> commit. Each step feeds on the previous tool's output.
func TestAgentWorkflowEndToEnd(t *testing.T) {
	dir := repo(t)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash missing")
	}
	var src strings.Builder
	for i := 1; i <= 300; i++ {
		src.WriteString("func f" + itoa(i) + "() int { return " + itoa(i) + " }\n")
	}
	write(t, dir, "src/lib/funcs.go", src.String())
	write(t, dir, "src/lib/other.go", "package lib\n")
	write(t, dir, "node_modules/junk/index.js", "var f150 = 1\n")
	write(t, dir, ".gitignore", "node_modules/\n")
	g(t, "git_stage", map[string]any{"all": true})
	g(t, "git_commit", map[string]any{"message": "add lib"})

	// 1. locate (ignored dirs must not pollute results)
	r := call(globFiles, map[string]any{"pattern": "src/**/*.go"})
	if r.Output != "src/lib/funcs.go\nsrc/lib/other.go\n" {
		t.Fatalf("glob: %q", r.Output)
	}
	r = call(grepHandler, map[string]any{"pattern": "func f150\\(", "path": "src"})
	if !strings.Contains(r.Output, "funcs.go") || strings.Contains(r.Output, "node_modules") {
		t.Fatalf("grep: %q", r.Output)
	}

	// 2. read just the window around the hit, with line numbers
	r = call(readFile, map[string]any{"path": "src/lib/funcs.go", "offset": 149, "limit": 3, "line_numbers": true})
	if !strings.Contains(r.Output, "[funcs.go lines 149-151 of 300]") || !strings.Contains(r.Output, "   150  func f150() int { return 150 }") {
		t.Fatalf("read: %q", r.Output)
	}

	// 3. targeted edit: ambiguous first (refused), then unique
	if r = call(editFile, map[string]any{"path": "src/lib/funcs.go", "old_string": "return 15", "new_string": "return 0"}); !r.IsErr {
		t.Fatalf("ambiguous edit must be refused: %q", r.Output)
	}
	r = call(editFile, map[string]any{"path": "src/lib/funcs.go", "old_string": "func f150() int { return 150 }", "new_string": "func f150() int { return -150 }"})
	if r.IsErr || !strings.Contains(r.Output, "+1 -1") {
		t.Fatalf("edit: %q", r.Output)
	}

	// 4. multi-file patch (LLM-style: wrong line numbers, no counts)
	patch := "--- a/src/lib/other.go\n+++ b/src/lib/other.go\n@@ -90,1 +90,3 @@\n package lib\n+\n+const Version = \"1\"\n" +
		"--- /dev/null\n+++ b/src/lib/doc.go\n@@ -0,0 +1,2 @@\n+// Package lib has helpers.\n+package lib\n"
	if r = call(applyPatch, patchIn(patch)); r.IsErr {
		t.Fatalf("patch: %q", r.Output)
	}

	// 5. reorganise
	if r = call(moveFile, map[string]any{"from": "src/lib/doc.go", "to": "src/lib/pkg/doc.go"}); r.IsErr {
		t.Fatalf("move: %q", r.Output)
	}
	if r = call(makeDir, map[string]any{"path": "src/tmp"}); r.IsErr {
		t.Fatalf("mkdir: %q", r.Output)
	}
	if r = call(deleteFile, map[string]any{"path": "src/tmp"}); r.IsErr {
		t.Fatalf("delete: %q", r.Output)
	}

	// 6. run something and review
	r = bash(map[string]any{"command": "wc -l src/lib/funcs.go"})
	if r.IsErr || !strings.Contains(r.Output, "300") {
		t.Fatalf("bash: %q", r.Output)
	}
	st := g(t, "git_status", map[string]any{})
	for _, w := range []string{"M src/lib/funcs.go", "M src/lib/other.go", "src/lib/pkg/"} {
		if !strings.Contains(st.Output, w) {
			t.Fatalf("status missing %q:\n%s", w, st.Output)
		}
	}
	d := g(t, "git_diff", map[string]any{"file": "src/lib/funcs.go", "context": float64(0)})
	if !strings.Contains(d.Output, "-func f150() int { return 150 }") || !strings.Contains(d.Output, "+func f150() int { return -150 }") {
		t.Fatalf("diff: %q", d.Output)
	}

	// 7. commit and verify the history
	g(t, "git_stage", map[string]any{"all": true})
	if r = g(t, "git_commit", map[string]any{"message": "tweak f150, add docs"}); r.IsErr {
		t.Fatalf("commit: %q", r.Output)
	}
	if r = g(t, "git_show", map[string]any{"stat_only": true}); !strings.Contains(r.Output, "tweak f150") || !strings.Contains(r.Output, "pkg/doc.go") {
		t.Fatalf("show: %q", r.Output)
	}
}

func itoa(n int) string {
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
