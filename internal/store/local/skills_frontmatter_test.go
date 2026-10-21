package local

import (
	"strings"
	"testing"
)

func newFrontmatterTestStore(t *testing.T) *LocalStore {
	t.Helper()
	s := &LocalStore{dataDir: t.TempDir()}
	if err := s.ensureDirs(); err != nil {
		t.Fatalf("ensureDirs: %v", err)
	}
	return s
}

// Regression test: formatSkillMd writes `description: %q\n` (Go-quoted,
// escaping `"` as `\"`), but the parser only did
// `strings.Trim(val, `"'`)`, which strips outer quotes without undoing
// inner escapes. A description containing `"` round-tripped to a string
// with literal backslash-quote pairs instead of the original quotes.
func TestFrontmatterRoundTrip_PreservesQuotedDescription(t *testing.T) {
	s := newFrontmatterTestStore(t)

	want := `hello "world" how are you`
	if _, err := s.CreateSkill("test-skill", want, "body"); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	skills, err := s.LoadSkills()
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if skills[0].Description != want {
		t.Errorf("description round-trip mismatch:\n  got:  %q\n  want: %q",
			skills[0].Description, want)
	}
}

// Regression test: %q escapes `\n` as the two-character sequence `\n`.
// The pre-fix parser saw a single-line value and returned it with the
// literal `\n` still present. The fix unquotes the value so `\n`
// becomes a real newline.
func TestFrontmatterRoundTrip_PreservesEmbeddedNewline(t *testing.T) {
	s := newFrontmatterTestStore(t)

	want := "line one\nline two"
	if _, err := s.CreateSkill("multiline-skill", want, "body"); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	skills, err := s.LoadSkills()
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if skills[0].Description != want {
		t.Errorf("description round-trip mismatch:\n  got:  %q\n  want: %q",
			skills[0].Description, want)
	}
}

// Sanity guard: descriptions that don't need escaping must still
// round-trip — i.e., the fix must not be over-broad.
func TestFrontmatterRoundTrip_PreservesPlainDescription(t *testing.T) {
	s := newFrontmatterTestStore(t)

	want := "a plain ascii description"
	if _, err := s.CreateSkill("plain-skill", want, "body"); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	skills, err := s.LoadSkills()
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if skills[0].Description != want {
		t.Errorf("description round-trip mismatch:\n  got:  %q\n  want: %q",
			skills[0].Description, want)
	}
}

// formatSkillMd writes `name: %s\n` (unquoted), so a fix that always
// tries strconv.Unquote would corrupt names. Guard that the parser
// still returns unquoted values untouched.
func TestFrontmatterRoundTrip_PreservesSkillName(t *testing.T) {
	s := newFrontmatterTestStore(t)

	if _, err := s.CreateSkill("named-skill", "desc", "body"); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	skills, err := s.LoadSkills()
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if !strings.HasPrefix(skills[0].Name, "named-skill") {
		t.Errorf("name mismatch: got %q", skills[0].Name)
	}
}
