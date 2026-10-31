package system

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Matching engine for edit_file. Pure functions over strings: no I/O here so
// the whole thing is unit-testable and the tool handler stays thin.
//
// Matching tiers, strictest first:
//  1. exact substring (or RE2 regex when regex=true)
//  2. whole-line match that ignores indentation, trailing whitespace, runs of
//     inner whitespace and typographic quotes/dashes — only used when the exact
//     tier finds nothing, and only applied when unambiguous (or replace_all)
//  3. failure with a "closest match" report so the model can fix its snippet
//     in one retry instead of re-reading the whole file.

const (
	maxAmbiguousShown  = 5
	snippetMaxRunes    = 120
	suggestMinScore    = 0.55
	suggestMaxFileLine = 20000
	suggestMaxOldLines = 40
	suggestShowLines   = 15
)

// editSpec is one find/replace operation.
type editSpec struct {
	Old        string
	New        string
	ReplaceAll bool
	Regex      bool
	Expected   int // >0: require exactly this many replacements (implies replace-all)
}

// editResult is the outcome of applying one editSpec to in-memory content.
type editResult struct {
	Content string
	Count   int    // replacements performed
	Note    string // non-empty when a tolerant fallback was used
}

func applyEdit(content string, e editSpec) (editResult, error) {
	if e.Old == "" {
		return editResult{}, errors.New("old_string is empty")
	}
	if e.Regex {
		return applyRegex(content, e)
	}
	if e.Old == e.New {
		return editResult{}, errors.New("old_string and new_string are identical — nothing to change")
	}
	if idxs := indexAll(content, e.Old); len(idxs) > 0 {
		return replaceAtIndexes(content, e, idxs)
	}
	res, matched, err := applyLineBlock(content, e)
	if err != nil {
		return editResult{}, err
	}
	if matched {
		return res, nil
	}
	return editResult{}, notFoundError(content, e.Old)
}

// ---------------------------------------------------------------- exact tier

func indexAll(s, sub string) []int {
	var out []int
	from := 0
	for from <= len(s)-len(sub) {
		i := strings.Index(s[from:], sub)
		if i < 0 {
			break
		}
		out = append(out, from+i)
		from += i + len(sub)
	}
	return out
}

func replaceAtIndexes(content string, e editSpec, idxs []int) (editResult, error) {
	total := len(idxs)
	if e.Expected > 0 && total != e.Expected {
		return editResult{}, expectedError(content, e.Expected, idxs)
	}
	all := e.ReplaceAll || e.Expected > 0
	if total > 1 && !all {
		return editResult{}, ambiguityError("old_string", content, idxs)
	}
	use := idxs
	if !all {
		use = idxs[:1]
	}
	var b strings.Builder
	prev := 0
	for _, i := range use {
		b.WriteString(content[prev:i])
		b.WriteString(e.New)
		prev = i + len(e.Old)
	}
	b.WriteString(content[prev:])
	return editResult{Content: b.String(), Count: len(use)}, nil
}

// --------------------------------------------------------------------- regex

func applyRegex(content string, e editSpec) (editResult, error) {
	re, err := regexp.Compile("(?m)" + e.Old)
	if err != nil {
		return editResult{}, fmt.Errorf("invalid regex: %v (Go RE2 syntax; (?m) is on by default, add (?s) to let . match newlines)", err)
	}
	locs := re.FindAllStringSubmatchIndex(content, -1)
	if len(locs) == 0 {
		return editResult{}, errors.New("regex pattern matched nothing in the file — check escaping, or use grep to confirm the text exists")
	}
	starts := make([]int, len(locs))
	for i, l := range locs {
		if l[0] == l[1] {
			return editResult{}, errors.New("regex matches an empty string, which would insert text at many positions — make the pattern match real text")
		}
		starts[i] = l[0]
	}
	if e.Expected > 0 && len(locs) != e.Expected {
		return editResult{}, expectedError(content, e.Expected, starts)
	}
	all := e.ReplaceAll || e.Expected > 0
	if len(locs) > 1 && !all {
		return editResult{}, ambiguityError("pattern", content, starts)
	}
	use := locs
	if !all {
		use = locs[:1]
	}
	var b strings.Builder
	var dst []byte
	prev := 0
	for _, l := range use {
		b.WriteString(content[prev:l[0]])
		dst = re.ExpandString(dst[:0], e.New, content, l)
		b.Write(dst)
		prev = l[1]
	}
	b.WriteString(content[prev:])
	return editResult{Content: b.String(), Count: len(use)}, nil
}

// ------------------------------------------------------------ tolerant tier

// applyLineBlock matches old_string as a block of whole lines, ignoring
// indentation / trailing whitespace / typographic quote differences.
// matched=false (nil error) means "nothing found, try suggestions".
func applyLineBlock(content string, e editSpec) (editResult, bool, error) {
	oldLines := splitBlock(e.Old)
	if len(oldLines) == 0 || allBlank(oldLines) {
		return editResult{}, false, nil
	}
	lines := strings.Split(content, "\n")
	if len(oldLines) > len(lines) {
		return editResult{}, false, nil
	}
	oldKeys := lineKeys(oldLines)
	fileKeys := lineKeys(lines)

	var starts []int // line indexes
	for i := 0; i+len(oldLines) <= len(lines); {
		if windowEqual(fileKeys, oldKeys, i) {
			starts = append(starts, i)
			i += len(oldLines)
		} else {
			i++
		}
	}
	if len(starts) == 0 {
		return editResult{}, false, nil
	}

	offsets := lineOffsets(lines)
	byteStarts := make([]int, len(starts))
	for i, s := range starts {
		byteStarts[i] = offsets[s]
	}
	if e.Expected > 0 && len(starts) != e.Expected {
		return editResult{}, true, expectedError(content, e.Expected, byteStarts)
	}
	all := e.ReplaceAll || e.Expected > 0
	if len(starts) > 1 && !all {
		return editResult{}, true, ambiguityError("old_string", content, byteStarts)
	}
	if !all {
		starts = starts[:1]
	}

	newLines := splitBlock(e.New)
	first := firstNonBlank(oldLines)
	oldIndent := leadingWS(oldLines[first])

	// Apply back-to-front so earlier indexes stay valid.
	for i := len(starts) - 1; i >= 0; i-- {
		s := starts[i]
		repl := make([]string, len(newLines))
		fileIndent := leadingWS(lines[s+first])
		for j, nl := range newLines {
			repl[j] = reindent(nl, oldIndent, fileIndent)
		}
		tail := append([]string{}, lines[s+len(oldLines):]...)
		lines = append(append(lines[:s:s], repl...), tail...)
	}
	return editResult{
		Content: strings.Join(lines, "\n"),
		Count:   len(starts),
		Note:    "old_string matched only after ignoring indentation/whitespace/quote differences; replacement re-indented to the file's style",
	}, true, nil
}

func reindent(line, oldIndent, fileIndent string) string {
	if strings.TrimSpace(line) == "" || oldIndent == fileIndent {
		return line
	}
	if strings.HasPrefix(line, oldIndent) {
		return fileIndent + line[len(oldIndent):]
	}
	return line
}

func splitBlock(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

func allBlank(lines []string) bool {
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return false
		}
	}
	return true
}

func firstNonBlank(lines []string) int {
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			return i
		}
	}
	return 0
}

func leadingWS(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

func windowEqual(fileKeys, oldKeys []string, start int) bool {
	for j, k := range oldKeys {
		if fileKeys[start+j] != k {
			return false
		}
	}
	return true
}

func lineKeys(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = fuzzyKey(l)
	}
	return out
}

var fuzzyRunes = strings.NewReplacer(
	"\u2018", "'", "\u2019", "'", "\u201c", `"`, "\u201d", `"`,
	"\u2013", "-", "\u2014", "-", "\u00a0", " ",
)

func fuzzyKey(line string) string {
	return strings.Join(strings.Fields(fuzzyRunes.Replace(line)), " ")
}

func lineOffsets(lines []string) []int {
	out := make([]int, len(lines))
	pos := 0
	for i, l := range lines {
		out[i] = pos
		pos += len(l) + 1
	}
	return out
}

// ------------------------------------------------------------------- errors

func lineNumberAt(content string, idx int) int {
	if idx > len(content) {
		idx = len(content)
	}
	return 1 + strings.Count(content[:idx], "\n")
}

func clipLine(s string, max int) string {
	s = strings.TrimRight(s, "\r")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}

func describeMatches(content string, starts []int) string {
	lines := strings.Split(content, "\n")
	shown := starts
	if len(shown) > maxAmbiguousShown {
		shown = shown[:maxAmbiguousShown]
	}
	var b strings.Builder
	for n, idx := range shown {
		ln := lineNumberAt(content, idx)
		fmt.Fprintf(&b, "  match %d at line %d:\n", n+1, ln)
		for l := ln - 1; l <= ln+1; l++ {
			if l < 1 || l > len(lines) {
				continue
			}
			mark := " "
			if l == ln {
				mark = ">"
			}
			fmt.Fprintf(&b, "   %s %4d | %s\n", mark, l, clipLine(lines[l-1], snippetMaxRunes))
		}
	}
	if len(starts) > len(shown) {
		fmt.Fprintf(&b, "  … and %d more\n", len(starts)-len(shown))
	}
	return b.String()
}

func ambiguityError(what, content string, starts []int) error {
	return fmt.Errorf("%s matches %d locations; refusing to guess which one to change.\n%sInclude more surrounding lines so it matches exactly one place, or set replace_all=true to change every occurrence.",
		what, len(starts), describeMatches(content, starts))
}

func expectedError(content string, want int, starts []int) error {
	return fmt.Errorf("expected_replacements=%d but found %d match(es); nothing was changed.\n%s",
		want, len(starts), describeMatches(content, starts))
}

func notFoundError(content, old string) error {
	var b strings.Builder
	b.WriteString("old_string not found in file; nothing was changed.")
	if strings.Contains(strings.ToLower(content), strings.ToLower(old)) {
		b.WriteString("\nA case-insensitive match exists — check capitalisation.")
	}
	if s := closestBlock(content, old); s != "" {
		b.WriteString("\n")
		b.WriteString(s)
	} else if strings.Count(content, "\n") >= suggestMaxFileLine {
		b.WriteString("\nFile is too large for a similarity search. Use grep -n to locate the text, then retry with it copied exactly.")
	} else {
		b.WriteString("\nNo similar text found. Use grep or read_file to see the file's current content, then retry with text copied exactly.")
	}
	return errors.New(b.String())
}

// ----------------------------------------------------- "did you mean" report

type bigramSet struct {
	m map[uint64]int
	n int
}

func makeBigrams(s string) bigramSet {
	r := []rune(s)
	if len(r) > 200 {
		r = r[:200]
	}
	bs := bigramSet{m: make(map[uint64]int, len(r))}
	for i := 0; i+1 < len(r); i++ {
		bs.m[uint64(r[i])<<32|uint64(r[i+1])]++
		bs.n++
	}
	return bs
}

func diceScore(ka, kb string, a, b bigramSet) float64 {
	if ka == kb {
		return 1
	}
	if ka == "" || kb == "" {
		return 0
	}
	if a.n == 0 || b.n == 0 {
		return 0
	}
	small, big := a, b
	if len(small.m) > len(big.m) {
		small, big = big, small
	}
	inter := 0
	for k, c := range small.m {
		if d, ok := big.m[k]; ok {
			if d < c {
				c = d
			}
			inter += c
		}
	}
	return 2 * float64(inter) / float64(a.n+b.n)
}

// closestBlock finds the file window most similar to old and renders it with
// line numbers plus the first differing line. Returns "" when nothing is close.
func closestBlock(content, old string) string {
	oldLines := splitBlock(old)
	if len(oldLines) == 0 || allBlank(oldLines) {
		return ""
	}
	if len(oldLines) > suggestMaxOldLines {
		oldLines = oldLines[:suggestMaxOldLines]
	}
	lines := strings.Split(content, "\n")
	if len(lines) > suggestMaxFileLine || len(oldLines) > len(lines) {
		return ""
	}
	n := len(oldLines)
	oldKeys := lineKeys(oldLines)
	oldBG := make([]bigramSet, n)
	for i, k := range oldKeys {
		oldBG[i] = makeBigrams(k)
	}
	fileKeys := lineKeys(lines)
	fileBG := make([]bigramSet, len(lines))
	for i, k := range fileKeys {
		fileBG[i] = makeBigrams(k)
	}

	windows := len(lines) - n + 1
	scores := make([]float64, windows)
	for j := 0; j < n; j++ {
		for i := 0; i < windows; i++ {
			fk, ok2 := fileKeys[i+j], oldKeys[j]
			if lenGap(fk, ok2) {
				continue
			}
			scores[i] += diceScore(fk, ok2, fileBG[i+j], oldBG[j])
		}
	}
	best, bestScore := -1, 0.0
	for i, s := range scores {
		if s > bestScore {
			best, bestScore = i, s
		}
	}
	bestScore /= float64(n)
	if best < 0 || bestScore < suggestMinScore {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Closest match (%d%% similar) at lines %d-%d:\n", int(bestScore*100+0.5), best+1, best+n)
	show := n
	if show > suggestShowLines {
		show = suggestShowLines
	}
	for j := 0; j < show; j++ {
		fmt.Fprintf(&b, "  %4d | %s\n", best+j+1, clipLine(lines[best+j], snippetMaxRunes))
	}
	if show < n {
		fmt.Fprintf(&b, "  … %d more line(s)\n", n-show)
	}
	for j := 0; j < n; j++ {
		if fileKeys[best+j] != oldKeys[j] {
			fmt.Fprintf(&b, "First difference at line %d:\n  expected: %s\n  found:    %s\n",
				best+j+1, clipLine(strings.TrimSpace(oldLines[j]), snippetMaxRunes), clipLine(strings.TrimSpace(lines[best+j]), snippetMaxRunes))
			break
		}
	}
	b.WriteString("Retry with old_string copied exactly from the lines above (or re-read the file if it may have changed).")
	return b.String()
}

// lenGap is a cheap pre-filter: lines with very different lengths can't be similar.
func lenGap(a, b string) bool {
	la, lb := len(a), len(b)
	if la < lb {
		la, lb = lb, la
	}
	return la > 2*lb+8
}
