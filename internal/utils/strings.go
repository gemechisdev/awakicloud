package utils

import "strings"

// Truncate trims whitespace and truncates s to max bytes, appending suffix if truncated.
func Truncate(s string, max int, suffix string) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + suffix
}

// CollapseWhitespace merges consecutive blank lines into a single empty line
// and trims leading/trailing whitespace from each line.
func CollapseWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	prevEmpty := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if !prevEmpty {
				out = append(out, "")
				prevEmpty = true
			}
			continue
		}
		out = append(out, trimmed)
		prevEmpty = false
	}
	return strings.Join(out, "\n")
}

// HasAnySuffix reports whether s ends with any of the given suffixes.
func HasAnySuffix(s string, suffixes ...string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}
