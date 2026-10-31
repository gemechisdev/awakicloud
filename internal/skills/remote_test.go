package skills

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAddArgs(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantSrc string
		want    InstallOptions
		wantErr bool
	}{
		{"plain url", "https://smithery.ai/skills/anthropics/frontend-design", "https://smithery.ai/skills/anthropics/frontend-design", InstallOptions{}, false},
		{"pasted npx", "npx -y skills add https://smithery.ai/skills/anthropics/frontend-design", "https://smithery.ai/skills/anthropics/frontend-design", InstallOptions{}, false},
		{"skill flag", "npx skills add https://github.com/anthropics/skills --skill frontend-design", "https://github.com/anthropics/skills", InstallOptions{Skill: "frontend-design"}, false},
		{"skill eq", "anthropics/skills --skill=pdf", "anthropics/skills", InstallOptions{Skill: "pdf"}, false},
		{"all", "anthropics/skills --all", "anthropics/skills", InstallOptions{All: true}, false},
		{"missing", "npx -y skills add", "", InstallOptions{}, true},
		{"flag needs value", "anthropics/skills --skill", "", InstallOptions{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, opts, err := ParseAddArgs(strings.Fields(tc.in))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if src != tc.wantSrc || opts != tc.want {
				t.Fatalf("got (%q, %+v), want (%q, %+v)", src, opts, tc.wantSrc, tc.want)
			}
		})
	}
}

func TestParseGitHubRef(t *testing.T) {
	cases := []struct {
		in   string
		want ghRef
		ok   bool
	}{
		{"anthropics/skills", ghRef{Owner: "anthropics", Repo: "skills"}, true},
		{"anthropics/skills@frontend-design", ghRef{Owner: "anthropics", Repo: "skills", Skill: "frontend-design"}, true},
		{"https://github.com/anthropics/skills", ghRef{Owner: "anthropics", Repo: "skills"}, true},
		{"https://github.com/anthropics/skills.git", ghRef{Owner: "anthropics", Repo: "skills"}, true},
		{"https://github.com/anthropics/skills/tree/main/skills/pdf", ghRef{Owner: "anthropics", Repo: "skills", Ref: "main", Subpath: "skills/pdf"}, true},
		{"https://github.com/anthropics/skills/blob/main/skills/pdf/SKILL.md", ghRef{Owner: "anthropics", Repo: "skills", Ref: "main", Subpath: "skills/pdf"}, true},
		{"github.com/anthropics/skills", ghRef{Owner: "anthropics", Repo: "skills"}, true},
		{"https://smithery.ai/skills/anthropics/frontend-design", ghRef{}, false},
		{"https://example.com/foo/bar", ghRef{}, false},
		{"https://backstage.io", ghRef{}, false},
		{"justaword", ghRef{}, false},
	}
	for _, tc := range cases {
		got, ok := parseGitHubRef(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseGitHubRef(%q) = (%+v, %v), want (%+v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseSkillsSh(t *testing.T) {
	g, ok := parseSkillsSh("https://skills.sh/anthropics/skills/frontend-design")
	if !ok || g.Owner != "anthropics" || g.Repo != "skills" || g.Skill != "frontend-design" {
		t.Fatalf("got %+v %v", g, ok)
	}
	if _, ok := parseSkillsSh("https://skills.sh/site/smithery.ai"); ok {
		t.Fatal("site pages must not resolve to a repo")
	}
	if _, ok := parseSkillsSh("https://smithery.ai/skills/a/b"); ok {
		t.Fatal("wrong host accepted")
	}
}

func TestSelectSkillDirs(t *testing.T) {
	dirs := []string{"skills/pdf", "skills/pdf/examples/demo", "skills/frontend-design", "skills/docx"}

	got, err := selectSkillDirs(dirs, "", "frontend-design", false)
	if err != nil || !reflect.DeepEqual(got, []string{"skills/frontend-design"}) {
		t.Fatalf("by name: got %v, %v", got, err)
	}

	got, err = selectSkillDirs(dirs, "", "", false)
	if err != nil || !reflect.DeepEqual(got, []string{"skills/docx", "skills/frontend-design", "skills/pdf"}) {
		t.Fatalf("all (nested dropped): got %v, %v", got, err)
	}

	got, err = selectSkillDirs(dirs, "skills/pdf", "", false)
	if err != nil || !reflect.DeepEqual(got, []string{"skills/pdf"}) {
		t.Fatalf("subpath: got %v, %v", got, err)
	}

	if _, err = selectSkillDirs(dirs, "", "nope", false); err == nil || !strings.Contains(err.Error(), "available") {
		t.Fatalf("missing name should list available skills, got %v", err)
	}

	var many []string
	for i := 0; i < maxBulkInstall+1; i++ {
		many = append(many, "skills/s"+strings.Repeat("x", i+1))
	}
	if _, err = selectSkillDirs(many, "", "", false); err == nil {
		t.Fatal("bulk install over the cap must require --all")
	}
	if _, err = selectSkillDirs(many, "", "", true); err != nil {
		t.Fatalf("--all should lift the cap: %v", err)
	}
}

func TestWriteSafeRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"../evil", "a/../../evil", "/abs", `a\b`, ""} {
		if err := writeSafe(root, bad, []byte("x"), false); err == nil {
			t.Errorf("writeSafe accepted %q", bad)
		}
	}
	if err := writeSafe(root, "scripts/run.sh", []byte("x"), true); err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
}

func TestWellKnownCandidates(t *testing.T) {
	u, _ := normalizeURL("https://smithery.ai/skills/anthropics/frontend-design")
	got := wellKnownCandidates(u)
	want := []string{
		"https://smithery.ai/skills/anthropics/frontend-design/.well-known/skills/index.json",
		"https://smithery.ai/skills/anthropics/frontend-design/.well-known/agent-skills/index.json",
		"https://smithery.ai/.well-known/skills/index.json",
		"https://smithery.ai/.well-known/agent-skills/index.json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}
