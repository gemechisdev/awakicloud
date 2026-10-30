package domain

import (
	"fmt"
	"strings"
)

// Skill is a reusable package the agent can discover and follow.
// Layout (Agent Skills style):
//
//	{name}/
//	  SKILL.md          required (frontmatter + instructions)
//	  references/       optional docs (Level 2)
//	  scripts/          optional executables (Level 3)
//	  assets/           optional templates/media (Level 2)
type Skill struct {
	Name        string
	Description string
	FilePath    string // path to SKILL.md
	Dir         string // skill root directory

	HasReferences bool
	HasScripts    bool
	HasAssets     bool
	Disabled      bool
}

// Level returns 1 (instructions), 2 (+resources), or 3 (+scripts).
func (s Skill) Level() int {
	if s.HasScripts {
		return 3
	}
	if s.HasReferences || s.HasAssets {
		return 2
	}
	return 1
}

func (s Skill) LevelLabel() string {
	switch s.Level() {
	case 3:
		return "L3 executable"
	case 2:
		return "L2 resources"
	default:
		return "L1 instructions"
	}
}

// FormatSkillsForPrompt renders the available skills as the <available_skills>
// block injected into the system prompt.
func FormatSkillsForPrompt(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<available_skills>\n")
	for _, s := range skills {
		if s.Disabled {
			continue
		}
		fmt.Fprintf(&b, "<skill name=%q description=%q location=%q level=%q />\n",
			s.Name, s.Description, s.FilePath, s.LevelLabel())
	}
	b.WriteString("</available_skills>")
	return b.String()
}
