package utils

import (
	"fmt"
	"path/filepath"
	"strings"
)

// UniqueName deduplicates a filename using a counter map.
// On first occurrence it returns the name as-is; subsequent duplicates get a "_N" suffix.
func UniqueName(name string, counts map[string]int) string {
	counts[name]++
	if counts[name] == 1 {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s_%d%s", base, counts[name], ext)
}
