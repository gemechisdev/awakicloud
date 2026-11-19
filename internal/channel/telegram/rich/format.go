package rich

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxCharacters is Telegram's hard limit for a Rich Message (32,768 UTF-8 chars).
const MaxCharacters = 32768

// SafeCharacters is the recommended soft threshold to avoid silent truncation
// on desktop and older mobile clients.
const SafeCharacters = 8000

// tableHeaderRE detects a Markdown table header row like | a | b |
var tableHeaderRE = regexp.MustCompile(`^\s*\|[^\n]*\|[^\n]*\n\s*\|[\s\-:|]+\|\s*$`)

// tableRowRE detects a Markdown table row line.
var tableRowRE = regexp.MustCompile(`^\s*\|.*\|\s*$`)

// IsRichWorthy reports whether the given text should be promoted to a Rich
// Message instead of a plain sendMessage. Heuristics:
//   - More than 3 lines
//   - Contains a Markdown table
//   - Explicitly requested (caller can always force rich)
func IsRichWorthy(text string) bool {
	if text == "" {
		return false
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 3 {
		return true
	}
	if tableHeaderRE.MatchString(text) || containsTableRows(text) {
		return true
	}
	return false
}

// MarkdownToRichHTML converts plain markdown-ish text to Telegram Rich HTML.
// It handles headings, bold/italic, code spans, code blocks, and lists.
// This is a best-effort converter; for complex content callers should hand-author HTML.
func MarkdownToRichHTML(text string) string {
	var b strings.Builder
	lines := strings.Split(text, "\n")
	inCodeBlock := false
	inList := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Code blocks
		if strings.HasPrefix(trimmed, "```") {
			if inCodeBlock {
				b.WriteString("</code></pre>")
				inCodeBlock = false
			} else {
				b.WriteString("<pre><code>")
				inCodeBlock = true
			}
			continue
		}
		if inCodeBlock {
			b.WriteString(escapeHTML(line))
			b.WriteString("\n")
			continue
		}

		// Headings
		if headingMatch := extractHeading(trimmed); headingMatch != "" {
			if inList {
				b.WriteString("</ul>")
				inList = false
			}
			b.WriteString(headingMatch)
			continue
		}

		// List items
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			if !inList {
				b.WriteString("<ul>")
				inList = true
			}
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			item = strings.TrimSpace(strings.TrimPrefix(item, "* "))
			b.WriteString(fmt.Sprintf("<li>%s</li>", inlineMDToHTML(item)))
			continue
		}
		if inList {
			b.WriteString("</ul>")
			inList = false
		}

		// Blank line -> paragraph break
		if trimmed == "" {
			b.WriteString("\n")
			continue
		}

		// Regular paragraph
		b.WriteString(fmt.Sprintf("<p>%s</p>", inlineMDToHTML(trimmed)))
	}
	if inCodeBlock {
		b.WriteString("</code></pre>")
	}
	if inList {
		b.WriteString("</ul>")
	}
	return b.String()
}

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

func inlineMDToHTML(s string) string {
	// Bold: **text** or __text__
	s = replaceMD(s, `^\*\*(.+)\*\*$`, "<b>$1</b>")
	s = replaceMD(s, `^__(.+)__$`, "<b>$1</b>")
	// Italic: *text* or _text_
	s = replaceMD(s, `^\*(.+)\*$`, "<i>$1</i>")
	s = replaceMD(s, `^_(.+)_$`, "<i>$1</i>")
	// Inline code: `text`
	re := regexp.MustCompile("`([^`]+)`")
	s = re.ReplaceAllString(s, "<code>$1</code>")
	return s
}

var mdReplaceRE = regexp.MustCompile(`^(.+)$`)

func replaceMD(s, pattern, repl string) string {
	re := regexp.MustCompile(pattern)
	return re.ReplaceAllString(s, repl)
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

// UTF8Len returns the UTF-8 byte length of a string.
func UTF8Len(s string) int {
	return len([]rune(s))
}

// WrapInDetails wraps secondary content in a collapsible <details> drawer,
// keeping the main message under the SafeCharacters threshold.
func WrapInDetails(html string, summary string) string {
	return fmt.Sprintf(`<details><summary>%s</summary>%s</details>`, escapeHTML(summary), html)
}

// ButtonRow builds a <tg-button-row> HTML block from a list of button specs.
// Each spec is a map with keys: type, style, url/data/text, label.
// Max 8 buttons per row.
func ButtonRow(buttons []map[string]string, align string) string {
	if align == "" {
		align = "center"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`<tg-button-row align="%s">`, align))
	for _, btn := range buttons {
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
func URLButton(label, url string, style string) string {
	if style == "" {
		style = "primary"
	}
	return ButtonRow([]map[string]string{
		{"type": "url", "style": style, "url": url, "label": label},
	}, "center")
}

// CallbackButton returns a single callback_data button.
func CallbackButton(label, data string, style string) string {
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

// EscapeHTML escapes HTML special characters.
func EscapeHTML(s string) string {
	return escapeHTML(s)
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func escapeHTMLAttr(s string) string {
	return strings.ReplaceAll(escapeHTML(s), `"`, "&quot;")
}

// Table builds a native Telegram HTML table from headers and rows.
// Each row must have the same number of cells as headers.
// Compact, bordered, and striped are enabled by default for mobile readability.
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
		for i, cell := range row {
			align := "left"
			if i == 0 {
				align = "left"
			}
			b.WriteString(fmt.Sprintf(`<td align="%s">%s</td>`, align, escapeHTML(cell)))
		}
		b.WriteString("</tr>")
	}
	b.WriteString("</table>")
	return b.String()
}

// Details creates a collapsible drawer with a summary and body.
func Details(summary, body string, open bool) string {
	openAttr := ""
	if open {
		openAttr = " open"
	}
	return fmt.Sprintf(`<details%s><summary>%s</summary>%s</details>`, openAttr, escapeHTML(summary), body)
}

// Heading renders an <h1>-<h6> heading.
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
	b := strings.Builder{}
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
	return fmt.Sprintf(`<tg-time unix="%d" format="%s"></tg-time>`, unix, format)
}

// TruncateToSafe truncates text to the SafeCharacters limit, appending an
// ellipsis if cut. Returns the truncated text and whether it was cut.
func TruncateToSafe(text string) (string, bool) {
	if UTF8Len(text) <= SafeCharacters {
		return text, false
	}
	runes := []rune(text)
	// Cut at SafeCharacters minus ellipsis room
	cut := SafeCharacters - 3
	if cut < 0 {
		cut = 0
	}
	return string(runes[:cut]) + "...", true
}

// ValidateRichMessage checks that exactly one input mode is set and the
// content is within Telegram's hard limit.
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
	if UTF8Len(content) > MaxCharacters {
		return fmt.Errorf("rich message content exceeds hard limit of %d characters (got %d)", MaxCharacters, UTF8Len(content))
	}
	return nil
}
