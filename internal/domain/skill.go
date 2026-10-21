package domain

import (
	"fmt"
	"strings"
)

// Skill is a reusable saved procedure the agent can discover and follow.
type Skill struct {
	Name        string
	Description string
	FilePath    string
	Dir         string
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
		fmt.Fprintf(&b, "<skill name=%q description=%q location=%q />\n",
			s.Name, s.Description, s.FilePath)
	}
	b.WriteString("</available_skills>")
	return b.String()
}
