package project

import (
	"regexp"
	"strings"
	"unicode"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns a display name into a filesystem-safe slug.
func Slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	if s == "" {
		return "project"
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		} else if r == ' ' || r == '-' || r == '_' || r == '.' {
			b.WriteByte('-')
		}
	}
	s = nonSlug.ReplaceAllString(b.String(), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "project"
	}
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}
