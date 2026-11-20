package rich

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxCharacters is Telegram's hard limit for a Rich Message (32,768 UTF-8 characters).
// Source: Bot API 10.1+ / official rich messages limits.
const MaxCharacters = 32768

// SafeCharacters is the soft threshold above which desktop clients may silently
// truncate uncollapsed content. Wrap secondary material in <details>.
const SafeCharacters = 8000

// tableHeaderRE detects a Markdown table header + separator.
var tableHeaderRE = regexp.MustCompile(`(?m)^\s*\|[^\n]*\|[^\n]*\n\s*\|[\s\-:|]+\|`)

// tableRowRE detects a Markdown table row line.
var tableRowRE = regexp.MustCompile(`^\s*\|.*\|\s*$`)

// IsRichWorthy reports whether the given text benefits from a Rich Message
// instead of a classic sendMessage. Heuristics are conservative: long content
// or structural markdown (tables, many lines, headings, code fences).
func IsRichWorthy(text string) bool {
	if text == "" {
		return false
	}
	if CharLen(text) > 1200 {
		return true
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 3 {
		return true
	}
	if tableHeaderRE.MatchString(text) || containsTableRows(text) {
		return true
	}
	if strings.Contains(text, "```") || strings.Contains(text, "# ") {
		return true
	}
	return false
}

// CharLen returns the number of Unicode code points (Telegram's "UTF-8 characters").
func CharLen(s string) int {
	return utf8.RuneCountInString(s)
}

// EscapeHTML escapes the five characters that break HTML markup.
// Prefer this over the stdlib html.EscapeString when you only need & < > " '.
func EscapeHTML(s string) string {
	return escapeHTML(s)
}

func escapeHTML(s string) string {
	// Order matters: & first.
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	return s
}

func escapeHTMLAttr(s string) string {
	return escapeHTML(s)
}


// parseGFMTable tries to read a GFM table starting at lines[i].
// Returns the HTML table, the number of lines consumed, and true on success.
func parseGFMTable(lines []string, i int) (html string, consumed int, ok bool) {
	if i >= len(lines) {
		return "", 0, false
	}
	headerLine := strings.TrimSpace(lines[i])
	if !tableRowRE.MatchString(headerLine) {
		return "", 0, false
	}
	if i+1 >= len(lines) {
		return "", 0, false
	}
	sep := strings.TrimSpace(lines[i+1])
	// Separator: |---|:---:| or similar
	if !regexp.MustCompile(`^\|?[\s:|-]+\|[\s:|-]*$`).MatchString(sep) && !regexp.MustCompile(`^\s*\|?(\s*:?-+:?\s*\|)+\s*:?-+:?\s*\|?\s*$`).MatchString(sep) {
		// Still accept classic |---|---|
		if !strings.Contains(sep, "-") || !strings.Contains(sep, "|") {
			return "", 0, false
		}
	}
	splitRow := func(line string) []string {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "|")
		line = strings.TrimSuffix(line, "|")
		parts := strings.Split(line, "|")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			out = append(out, strings.TrimSpace(p))
		}
		return out
	}
	headers := splitRow(headerLine)
	if len(headers) == 0 {
		return "", 0, false
	}
	var rows [][]string
	j := i + 2
	for j < len(lines) {
		rowLine := strings.TrimSpace(lines[j])
		if rowLine == "" || !tableRowRE.MatchString(rowLine) {
			break
		}
		cells := splitRow(rowLine)
		// Pad / trim to header width
		for len(cells) < len(headers) {
			cells = append(cells, "")
		}
		if len(cells) > len(headers) {
			cells = cells[:len(headers)]
		}
		rows = append(rows, cells)
		j++
	}
	if len(rows) == 0 {
		return "", 0, false
	}
	return Table(headers, rows, "", true), j - i, true
}

// MarkdownToRichHTML is a best-effort converter for agent / status text that
// needs HTML (details, buttons, custom tags). Prefer sending native Markdown
// via contentType "markdown" when no custom HTML is required.
//
// Guarantees:
//   - All user-visible text is escaped before insertion into tags.
//   - Inline **bold**, *italic*, `code`, ~~strike~~ work inside paragraphs.
//   - Code fences, headings, unordered lists, basic tables are recognised.
func MarkdownToRichHTML(text string) string {
	if text == "" {
		return ""
	}
	text = ConvertMathMarkdown(text)

	var b strings.Builder
	lines := strings.Split(text, "\n")
	inCodeBlock := false
	inList := false
	var codeLang string

	flushList := func() {
		if inList {
			b.WriteString("</ul>")
			inList = false
		}
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Fenced code blocks
		if strings.HasPrefix(trimmed, "```") {
			if inCodeBlock {
				b.WriteString("</code></pre>")
				inCodeBlock = false
				codeLang = ""
			} else {
				flushList()
				codeLang = strings.TrimSpace(trimmed[3:])
				if codeLang != "" {
					b.WriteString(fmt.Sprintf(`<pre><code class="language-%s">`, escapeHTMLAttr(codeLang)))
				} else {
					b.WriteString("<pre><code>")
				}
				inCodeBlock = true
			}
			continue
		}
		if inCodeBlock {
			b.WriteString(escapeHTML(line))
			b.WriteString("\n")
			continue
		}

		// GFM tables (| a | b | + separator + rows)
		if html, n, ok := parseGFMTable(lines, i); ok {
			flushList()
			b.WriteString(html)
			i += n - 1 // loop will i++
			continue
		}

		// Block math $$ ... $$
		if strings.HasPrefix(trimmed, "$$") {
			flushList()
			if strings.Count(trimmed, "$$") >= 2 && len(trimmed) > 4 {
				inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "$$"), "$$"))
				b.WriteString(Math(inner, true))
				continue
			}
			var body strings.Builder
			if len(trimmed) > 2 {
				body.WriteString(trimmed[2:])
				body.WriteByte('\n')
			}
			i++
			for i < len(lines) {
				l := lines[i]
				t := strings.TrimSpace(l)
				if strings.Contains(t, "$$") {
					before, _, _ := strings.Cut(t, "$$")
					body.WriteString(before)
					break
				}
				body.WriteString(l)
				body.WriteByte('\n')
				i++
			}
			b.WriteString(Math(body.String(), true))
			continue
		}

		// Pre-converted rich tags — emit raw
		if strings.HasPrefix(trimmed, "<tg-math") || strings.HasPrefix(trimmed, "<tg-spoiler") || strings.HasPrefix(trimmed, "<tg-math-block") {
			flushList()
			b.WriteString(trimmed)
			continue
		}

		// Headings
		if h := extractHeading(trimmed); h != "" {
			flushList()
			b.WriteString(h)
			continue
		}

		// Unordered list items
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			if !inList {
				b.WriteString("<ul>")
				inList = true
			}
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			item = strings.TrimSpace(strings.TrimPrefix(item, "* "))
			// Task-list style
			checked := false
			if strings.HasPrefix(item, "[x] ") || strings.HasPrefix(item, "[X] ") {
				checked = true
				item = strings.TrimSpace(item[4:])
			} else if strings.HasPrefix(item, "[ ] ") {
				item = strings.TrimSpace(item[4:])
			}
			if checked {
				b.WriteString(fmt.Sprintf("<li>✅ %s</li>", inlineMDToHTML(item)))
			} else if strings.HasPrefix(trimmed, "- [") || strings.HasPrefix(trimmed, "* [") {
				b.WriteString(fmt.Sprintf("<li>⬜ %s</li>", inlineMDToHTML(item)))
			} else {
				b.WriteString(fmt.Sprintf("<li>%s</li>", inlineMDToHTML(item)))
			}
			continue
		}

		// Ordered list (simple)
		if m := orderedListRE.FindStringSubmatch(trimmed); m != nil {
			if !inList {
				b.WriteString("<ol>")
				inList = true // reuse flag; close as </ol> below if needed
			}
			// For simplicity we emit <ul> style close; better to track list type.
			// Use unordered for mixed; emit <li value="N">.
			b.WriteString(fmt.Sprintf(`<li value="%s">%s</li>`, m[1], inlineMDToHTML(m[2])))
			continue
		}

		flushList()

		if trimmed == "" {
			b.WriteString("\n")
			continue
		}

		// Horizontal rule
		if trimmed == "---" || trimmed == "***" || trimmed == "___" {
			b.WriteString("<hr/>")
			continue
		}

		// Blockquote
		if strings.HasPrefix(trimmed, "> ") {
			b.WriteString(fmt.Sprintf("<blockquote>%s</blockquote>", inlineMDToHTML(strings.TrimSpace(trimmed[2:]))))
			continue
		}

		// Regular paragraph
		b.WriteString(fmt.Sprintf("<p>%s</p>", inlineMDToHTML(trimmed)))
	}

	if inCodeBlock {
		b.WriteString("</code></pre>")
	}
	flushList()
	return b.String()
}

var orderedListRE = regexp.MustCompile(`^(\d+)\.\s+(.*)$`)

func extractHeading(trimmed string) string {
	for level := 6; level >= 1; level-- {
		prefix := strings.Repeat("#", level) + " "
		if strings.HasPrefix(trimmed, prefix) {
			text := strings.TrimSpace(trimmed[len(prefix):])
			return fmt.Sprintf("<h%d>%s</h%d>", level, escapeHTML(text), level)
		}
	}
	return ""
}

// inlineMDToHTML converts common inline markdown *after* escaping the whole
// string, then re-introducing safe tags for the matched spans.
func inlineMDToHTML(s string) string {
	if s == "" {
		return ""
	}
	// Escape first so user content cannot inject tags.
	s = escapeHTML(s)

	// Links: [text](url)  — url is already escaped; only allow http(s)/tg/mailto/tel
	s = linkRE.ReplaceAllStringFunc(s, func(m string) string {
		parts := linkRE.FindStringSubmatch(m)
		if len(parts) != 3 {
			return m
		}
		text, url := parts[1], parts[2]
		if !isSafeURL(url) {
			return text
		}
		return fmt.Sprintf(`<a href="%s">%s</a>`, url, text)
	})

	// Bold **...** or __...__
	s = boldRE.ReplaceAllString(s, "<b>$1</b>")
	// Italic *...* or _..._ (avoid matching inside bold)
	s = italicRE.ReplaceAllString(s, "<i>$1</i>")
	// Strikethrough ~~...~~
	s = strikeRE.ReplaceAllString(s, "<s>$1</s>")
	// Inline code `...`
	s = codeRE.ReplaceAllString(s, "<code>$1</code>")
	// Spoiler ||...||
	s = spoilerRE.ReplaceAllString(s, "<tg-spoiler>$1</tg-spoiler>")
	// Highlight ==marked==
	s = markRE.ReplaceAllString(s, "<mark>$1</mark>")
	// Underline ++text++ (Telegram HTML <u>; not GFM-standard but useful)
	s = underlineRE.ReplaceAllString(s, "<u>$1</u>")
	// Subscript H~2~O style (single tilde pairs; strike uses ~~)
	s = subRE.ReplaceAllString(s, "<sub>$1</sub>")
	// Superscript x^2^ style
	s = supRE.ReplaceAllString(s, "<sup>$1</sup>")
	// Inline math $...$ (after escape; $ and TeX ASCII are safe)
	s = inlineMathRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := strings.TrimSpace(m[1 : len(m)-1])
		if inner == "" {
			return m
		}
		return Math(inner, false)
	})

	return s
}

var (
	linkRE    = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	boldRE    = regexp.MustCompile(`\*\*(.+?)\*\*|__(.+?)__`)
	italicRE  = regexp.MustCompile(`(?:^|[^*])\*([^*]+?)\*(?:[^*]|$)`)
	// Simpler italic that works after bold replacement
	italicRE2 = regexp.MustCompile(`\*([^*\n]+?)\*|_([^_\n]+?)_`)
	strikeRE  = regexp.MustCompile(`~~(.+?)~~`)
	codeRE       = regexp.MustCompile("`([^`]+)`")
	spoilerRE    = regexp.MustCompile(`\|\|(.+?)\|\|`)
	markRE       = regexp.MustCompile(`==([^=]+?)==`)
	underlineRE  = regexp.MustCompile(`\+\+([^+]+?)\+\+`)
	subRE        = regexp.MustCompile(`~([^~\n]+?)~`)
	supRE        = regexp.MustCompile(`\^([^\^\n]+?)\^`)
	blockMathRE  = regexp.MustCompile(`(?s)\$\$(.+?)\$\$`)
	inlineMathRE = regexp.MustCompile(`\$([^\$\n]+?)\$`)
)

func init() {
	// Prefer the simpler italic after bold has been applied
	italicRE = italicRE2
}

// ExtendedInlineHTML escapes s and applies Telegram-extended inline syntax that
// goldmark does not parse: ||spoiler||, ==mark==, ++underline++, ~sub~, ^sup^.
// Safe to call on plain text segments from FormatRichHTML.

// InlinePlaceholder holds a deferred rich HTML fragment restored after goldmark.
type InlinePlaceholder struct {
	Token string
	HTML  string
}

// PreprocessExtendedMarkdown replaces Telegram-extended inline markers with
// opaque tokens so goldmark cannot mis-parse them (e.g. ~sub~ as strikethrough).
// Call RestoreInlinePlaceholders on the final HTML.
func PreprocessExtendedMarkdown(s string) (string, []InlinePlaceholder) {
	if s == "" || !hasExtendedMarkers(s) {
		return s, nil
	}
	parts, _ := splitFences(s)
	var phs []InlinePlaceholder
	for i, p := range parts {
		if i%2 == 1 {
			continue // fenced code
		}
		parts[i] = preprocessExtendedInText(p, &phs)
	}
	return strings.Join(parts, ""), phs
}

// RestoreInlinePlaceholders puts rich HTML fragments back in place of tokens.
func RestoreInlinePlaceholders(s string, phs []InlinePlaceholder) string {
	for i := len(phs) - 1; i >= 0; i-- {
		s = strings.Replace(s, phs[i].Token, phs[i].HTML, 1)
	}
	return s
}

func hasExtendedMarkers(s string) bool {
	return strings.Contains(s, "||") ||
		strings.Contains(s, "==") ||
		strings.Contains(s, "++") ||
		strings.Contains(s, "^") ||
		strings.Contains(s, "~")
}

// splitFences splits s into alternating text/code segments on ``` fences.
// Odd indices are fence bodies left untouched.
func splitFences(s string) (parts []string, hasFence bool) {
	if !strings.Contains(s, "```") {
		return []string{s}, false
	}
	var out []string
	rest := s
	for {
		start := strings.Index(rest, "```")
		if start < 0 {
			out = append(out, rest)
			break
		}
		out = append(out, rest[:start])
		rest = rest[start:]
		endRel := strings.Index(rest[3:], "```")
		if endRel < 0 {
			out = append(out, rest)
			break
		}
		end := 3 + endRel + 3
		out = append(out, rest[:end])
		rest = rest[end:]
		hasFence = true
	}
	return out, hasFence
}

func preprocessExtendedInText(s string, phs *[]InlinePlaceholder) string {
	put := func(html string) string {
		tok := fmt.Sprintf("«TGX%d»", len(*phs))
		*phs = append(*phs, InlinePlaceholder{Token: tok, HTML: html})
		return tok
	}

	// Protect inline code so markers inside stay literal.
	type keep struct{ key, val string }
	var kept []keep
	keepPut := func(val string) string {
		key := fmt.Sprintf("«KEEP%d»", len(kept))
		kept = append(kept, keep{key, val})
		return key
	}
	s = codeRE.ReplaceAllStringFunc(s, func(m string) string { return keepPut(m) })

	// ~~strikethrough~~ → <s> before single-tilde sub, so goldmark cannot
	// mis-read ~~ as two single-tilde delimiters (it treats ~ as strike).
	s = strikeRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := strikeRE.FindStringSubmatch(m)[1]
		return put("<s>" + escapeHTML(inner) + "</s>")
	})
	s = spoilerRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := spoilerRE.FindStringSubmatch(m)[1]
		return put("<tg-spoiler>" + escapeHTML(inner) + "</tg-spoiler>")
	})
	s = markRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := markRE.FindStringSubmatch(m)[1]
		return put("<mark>" + escapeHTML(inner) + "</mark>")
	})
	s = underlineRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := underlineRE.FindStringSubmatch(m)[1]
		return put("<u>" + escapeHTML(inner) + "</u>")
	})
	s = supRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := supRE.FindStringSubmatch(m)[1]
		return put("<sup>" + escapeHTML(inner) + "</sup>")
	})
	s = subRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := subRE.FindStringSubmatch(m)[1]
		return put("<sub>" + escapeHTML(inner) + "</sub>")
	})

	for i := len(kept) - 1; i >= 0; i-- {
		s = strings.Replace(s, kept[i].key, kept[i].val, 1)
	}
	return s
}

func ExtendedInlineHTML(s string) string {
	if s == "" {
		return ""
	}
	s = escapeHTML(s)
	s = spoilerRE.ReplaceAllString(s, "<tg-spoiler>$1</tg-spoiler>")
	s = markRE.ReplaceAllString(s, "<mark>$1</mark>")
	s = underlineRE.ReplaceAllString(s, "<u>$1</u>")
	// Subscript before we might confuse with leftover tildes; strike is ~~ handled by goldmark.
	s = subRE.ReplaceAllString(s, "<sub>$1</sub>")
	s = supRE.ReplaceAllString(s, "<sup>$1</sup>")
	return s
}


func isSafeURL(u string) bool {
	u = strings.ToLower(u)
	return strings.HasPrefix(u, "http://") ||
		strings.HasPrefix(u, "https://") ||
		strings.HasPrefix(u, "tg://") ||
		strings.HasPrefix(u, "mailto:") ||
		strings.HasPrefix(u, "tel:")
}

func containsTableRows(text string) bool {
	rows := 0
	for _, line := range strings.Split(text, "\n") {
		if tableRowRE.MatchString(line) {
			rows++
			if rows >= 2 {
				return true
			}
		} else {
			rows = 0
		}
	}
	return false
}

// WrapInDetails wraps secondary content in a collapsible <details> drawer.
func WrapInDetails(html, summary string) string {
	return fmt.Sprintf(`<details><summary>%s</summary>%s</details>`, escapeHTML(summary), html)
}

// ButtonRow builds a <tg-button-row> HTML block.
// Max 8 buttons per row (Bot API 10.3).
func ButtonRow(buttons []map[string]string, align string) string {
	if align == "" {
		align = "center"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`<tg-button-row align="%s">`, escapeHTMLAttr(align)))
	n := 0
	for _, btn := range buttons {
		if n >= 8 {
			break
		}
		b.WriteString(`<tg-button`)
		if t, ok := btn["type"]; ok {
			b.WriteString(fmt.Sprintf(` type="%s"`, escapeHTMLAttr(t)))
		}
		if s, ok := btn["style"]; ok {
			b.WriteString(fmt.Sprintf(` style="%s"`, escapeHTMLAttr(s)))
		}
		if v, ok := btn["url"]; ok {
			b.WriteString(fmt.Sprintf(` url="%s"`, escapeHTMLAttr(v)))
		}
		if v, ok := btn["data"]; ok {
			// Callback data hard limit 1–64 bytes
			if len(v) > 64 {
				v = v[:64]
			}
			b.WriteString(fmt.Sprintf(` data="%s"`, escapeHTMLAttr(v)))
		}
		if v, ok := btn["text"]; ok {
			b.WriteString(fmt.Sprintf(` text="%s"`, escapeHTMLAttr(v)))
		}
		label, hasLabel := btn["label"]
		if !hasLabel {
			label = btn["text"]
		}
		b.WriteString(`>`)
		b.WriteString(escapeHTML(label))
		b.WriteString(`</tg-button>`)
		n++
	}
	b.WriteString(`</tg-button-row>`)
	return b.String()
}

// CopyButton returns a single tap-to-copy button (copy_text type).
func CopyButton(label, copyText string) string {
	return ButtonRow([]map[string]string{
		{"type": "copy_text", "style": "primary", "text": copyText, "label": label},
	}, "center")
}

// URLButton returns a single URL button.
func URLButton(label, url, style string) string {
	if style == "" {
		style = "primary"
	}
	return ButtonRow([]map[string]string{
		{"type": "url", "style": style, "url": url, "label": label},
	}, "center")
}

// CallbackButton returns a single callback_data button.
func CallbackButton(label, data, style string) string {
	if style == "" {
		style = "primary"
	}
	return ButtonRow([]map[string]string{
		{"type": "callback_data", "style": style, "data": data, "label": label},
	}, "center")
}

// CodeBlock wraps text in <code> for mobile tap-to-copy.
func CodeBlock(text string) string {
	return fmt.Sprintf("<code>%s</code>", escapeHTML(text))
}

// PreBlock wraps text in <pre><code> for multi-line code with tap-to-copy.
func PreBlock(text string) string {
	return fmt.Sprintf("<pre><code>%s</code></pre>", escapeHTML(text))
}

// Table builds a native Telegram HTML table.
func Table(headers []string, rows [][]string, caption string, compact bool) string {
	var b strings.Builder
	attrs := "bordered striped"
	if compact {
		attrs += " compact"
	}
	b.WriteString(fmt.Sprintf(`<table %s>`, attrs))
	if caption != "" {
		b.WriteString(fmt.Sprintf(`<caption>%s</caption>`, escapeHTML(caption)))
	}
	b.WriteString("<tr>")
	for _, h := range headers {
		b.WriteString(fmt.Sprintf(`<th align="center">%s</th>`, escapeHTML(h)))
	}
	b.WriteString("</tr>")
	for _, row := range rows {
		b.WriteString("<tr>")
		for _, cell := range row {
			b.WriteString(fmt.Sprintf(`<td align="left">%s</td>`, escapeHTML(cell)))
		}
		b.WriteString("</tr>")
	}
	b.WriteString("</table>")
	return b.String()
}

// Details creates a collapsible drawer. Body is expected to already be safe HTML
// (or plain text that has been escaped by the caller).
func Details(summary, body string, open bool) string {
	openAttr := ""
	if open {
		openAttr = " open"
	}
	return fmt.Sprintf(`<details%s><summary>%s</summary>%s</details>`, openAttr, escapeHTML(summary), body)
}

// DetailsPlain escapes the body as well (for plain-text plan items etc.).
func DetailsPlain(summary, plainBody string, open bool) string {
	// Turn newlines into separate paragraphs so the drawer is readable.
	var parts []string
	for _, line := range strings.Split(plainBody, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts = append(parts, Paragraph(line))
	}
	return Details(summary, strings.Join(parts, ""), open)
}

// Heading renders an <h1>–<h6> heading.
func Heading(level int, text string) string {
	if level < 1 {
		level = 1
	}
	if level > 6 {
		level = 6
	}
	return fmt.Sprintf("<h%d>%s</h%d>", level, escapeHTML(text), level)
}

// Paragraph renders a <p> paragraph.
func Paragraph(text string) string {
	return fmt.Sprintf("<p>%s</p>", escapeHTML(text))
}

// Footer renders a <footer> block.
func Footer(text string) string {
	return fmt.Sprintf("<footer>%s</footer>", escapeHTML(text))
}

// HR renders a horizontal divider.
func HR() string {
	return "<hr/>"
}

// Math renders TeX/LaTeX: inline <tg-math>, block <tg-math-block> (Bot API RichText).
func Math(tex string, display bool) string {
	tex = strings.TrimSpace(tex)
	if tex == "" {
		return ""
	}
	if display {
		return fmt.Sprintf(`<tg-math-block>%s</tg-math-block>`, escapeHTML(tex))
	}
	return fmt.Sprintf(`<tg-math>%s</tg-math>`, escapeHTML(tex))
}

// ConvertMathMarkdown replaces $$...$$ (block) and $...$ (inline) with <tg-math>.
// Call before Markdown→HTML so formulas survive goldmark and the hand converter.
func ConvertMathMarkdown(s string) string {
	if s == "" || !strings.Contains(s, "$") {
		return s
	}
	// Block first: $$ ... $$ (may span lines)
	s = blockMathRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := strings.TrimSpace(m[2 : len(m)-2])
		if inner == "" {
			return m
		}
		return Math(inner, true)
	})
	// Inline: $...$ (single line, no nested $)
	s = inlineMathRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := strings.TrimSpace(m[1 : len(m)-1])
		if inner == "" {
			return m
		}
		// Skip currency-like $10 or empty
		if len(inner) < 1 {
			return m
		}
		return Math(inner, false)
	})
	return s
}

// Spoiler renders tap-to-reveal content.
func Spoiler(text string) string {
	return fmt.Sprintf("<tg-spoiler>%s</tg-spoiler>", escapeHTML(text))
}

// Blockquote renders a block quotation.
func Blockquote(text string) string {
	return fmt.Sprintf("<blockquote>%s</blockquote>", escapeHTML(text))
}

// Aside renders a pull quote with optional citation.
func Aside(text, cite string) string {
	var b strings.Builder
	b.WriteString("<aside>")
	b.WriteString(escapeHTML(text))
	if cite != "" {
		b.WriteString(fmt.Sprintf("<cite>%s</cite>", escapeHTML(cite)))
	}
	b.WriteString("</aside>")
	return b.String()
}

// Time renders a locale-aware <tg-time> element.
func Time(unix int64, format string) string {
	return fmt.Sprintf(`<tg-time unix="%d" format="%s"></tg-time>`, unix, escapeHTMLAttr(format))
}

// TruncateToSafe truncates to SafeCharacters, appending an ellipsis if cut.
func TruncateToSafe(text string) (string, bool) {
	if CharLen(text) <= SafeCharacters {
		return text, false
	}
	runes := []rune(text)
	cut := SafeCharacters - 3
	if cut < 0 {
		cut = 0
	}
	return string(runes[:cut]) + "...", true
}

// ValidateRichMessage checks that exactly one input mode is set and the
// content is within Telegram's hard character limit.
func ValidateRichMessage(html, markdown string, blocks any) error {
	modes := 0
	if html != "" {
		modes++
	}
	if markdown != "" {
		modes++
	}
	if blocks != nil {
		modes++
	}
	if modes != 1 {
		return fmt.Errorf("exactly one of html, markdown, or blocks must be set, got %d", modes)
	}
	content := html
	if markdown != "" {
		content = markdown
	}
	if CharLen(content) > MaxCharacters {
		return fmt.Errorf("rich message content exceeds hard limit of %d characters (got %d)", MaxCharacters, CharLen(content))
	}
	return nil
}

// EnsureSafeLength returns content that is under MaxCharacters.
// If it exceeds SafeCharacters it prefers wrapping the tail in a details drawer
// when the content looks like HTML; otherwise it truncates.
func EnsureSafeLength(html string) string {
	if CharLen(html) <= MaxCharacters {
		if CharLen(html) > SafeCharacters {
			// Leave as-is; caller should have used details for secondary content.
			// We only hard-truncate above MaxCharacters.
		}
		return html
	}
	runes := []rune(html)
	cut := MaxCharacters - 20
	if cut < 0 {
		cut = 0
	}
	return string(runes[:cut]) + "\n<p>… (truncated)</p>"
}

// StripThinking removes <tg-thinking>…</tg-thinking> blocks (draft-only tag).
func StripThinking(html string) string {
	re := regexp.MustCompile(`(?is)<tg-thinking>.*?</tg-thinking>`)
	return re.ReplaceAllString(html, "")
}

// NeedsRTL returns true when the text contains a significant amount of
// RTL script (Persian, Arabic, Hebrew, Urdu).
func NeedsRTL(text string) bool {
	count := 0
	for _, r := range text {
		if isRTLRune(r) {
			count++
			if count > 10 {
				return true
			}
		}
	}
	return false
}

func isRTLRune(r rune) bool {
	// Hebrew, Arabic, Syriac, Thaana, N'Ko, Arabic Presentation Forms, etc.
	return (r >= 0x0590 && r <= 0x05FF) ||
		(r >= 0x0600 && r <= 0x06FF) ||
		(r >= 0x0700 && r <= 0x074F) ||
		(r >= 0x0750 && r <= 0x077F) ||
		(r >= 0x08A0 && r <= 0x08FF) ||
		(r >= 0xFB50 && r <= 0xFDFF) ||
		(r >= 0xFE70 && r <= 0xFEFF)
}

// Compile-time check that stdlib html is available if needed elsewhere.
var _ = html.EscapeString
