package system

import (
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Directory walking shared by glob_files and directory_tree: skips noisy
// directories and honours .gitignore files (nested ones included), so results
// are not drowned in node_modules / build output.

var defaultSkipDirs = map[string]bool{
	".git": true, "node_modules": true, ".awaki": true, "__pycache__": true,
	".venv": true, ".next": true, ".cache": true, ".gradle": true, ".pytest_cache": true,
}

const maxIgnoreFileBytes = 1 << 20

type ignoreRule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

type ignoreSet struct {
	base  string // directory (relative to walk root, "/"-separated) the file lives in
	rules []ignoreRule
}

type ignoreStack []*ignoreSet

// globRegex translates a glob (supporting **, *, ?, [..], {a,b}) into a regex
// fragment where "/" is the only path separator.
func globRegex(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				if i+2 < len(g) && g[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := strings.IndexByte(g[i+1:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := g[i+1 : i+1+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += j + 1
		case '{':
			end, parts := splitBraces(g[i:])
			if end < 0 {
				b.WriteString(`\{`)
				continue
			}
			alts := make([]string, len(parts))
			for k, p := range parts {
				alts[k] = globRegex(p)
			}
			b.WriteString("(?:" + strings.Join(alts, "|") + ")")
			i += end
		case '\\':
			if i+1 < len(g) {
				i++
				b.WriteString(regexp.QuoteMeta(string(g[i])))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// splitBraces parses "{a,b{c,d}}..." at s[0]=='{'; returns index of the closing
// brace and the top-level comma-separated parts, or -1 when unbalanced.
func splitBraces(s string) (int, []string) {
	depth := 0
	start := 1
	var parts []string
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				parts = append(parts, s[start:i])
				return i, parts
			}
		case ',':
			if depth == 1 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return -1, nil
}

func compileGlob(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile("^" + globRegex(pattern) + "$")
}

func parseGitignore(base, text string) *ignoreSet {
	set := &ignoreSet{base: base}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := ignoreRule{}
		if strings.HasPrefix(line, "!") {
			r.negate = true
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		anchored := strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			continue
		}
		expr := "^" + globRegex(line) + "$"
		if !anchored {
			expr = "^(?:.*/)?" + globRegex(line) + "$"
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			continue
		}
		r.re = re
		set.rules = append(set.rules, r)
	}
	return set
}

func (s ignoreStack) ignored(rel string, isDir bool) bool {
	ignored := false
	for _, set := range s {
		r := rel
		if set.base != "" {
			if !strings.HasPrefix(rel, set.base+"/") {
				continue
			}
			r = rel[len(set.base)+1:]
		}
		for _, rule := range set.rules {
			if rule.dirOnly && !isDir {
				continue
			}
			if rule.re.MatchString(r) {
				ignored = !rule.negate
			}
		}
	}
	return ignored
}

// readFiltered lists dirAbs (relDir is its path relative to the walk root) with
// ignore rules applied. The returned stack includes dirAbs/.gitignore, if any.
// Entries are sorted directories first, then by name.
func readFiltered(dirAbs, relDir string, stack ignoreStack, respect bool) ([]os.DirEntry, ignoreStack, error) {
	entries, err := os.ReadDir(dirAbs)
	if err != nil {
		return nil, stack, err
	}
	if !respect {
		sortEntries(dirAbs, entries)
		return entries, stack, nil
	}
	if data, err := os.ReadFile(path.Join(dirAbs, ".gitignore")); err == nil && len(data) <= maxIgnoreFileBytes {
		stack = append(append(ignoreStack{}, stack...), parseGitignore(relDir, string(data)))
	}
	out := entries[:0:0]
	for _, e := range entries {
		isDir := e.IsDir()
		if isDir && defaultSkipDirs[e.Name()] {
			continue
		}
		rel := e.Name()
		if relDir != "" {
			rel = relDir + "/" + e.Name()
		}
		if stack.ignored(rel, isDir) {
			continue
		}
		out = append(out, e)
	}
	sortEntries(dirAbs, out)
	return out, stack, nil
}

func sortEntries(dirAbs string, entries []os.DirEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		di, dj := entries[i].IsDir(), entries[j].IsDir()
		if di != dj {
			return di
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
}
