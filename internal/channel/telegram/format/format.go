package format

import (
	"fmt"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	mdtext "github.com/yuin/goldmark/text"
	htmlrender "github.com/yuin/goldmark/renderer/html"

	"github.com/gemechischala/awaki/internal/channel/telegram/rich"
)

// FormattedChunk holds both the HTML and plain-text version of a chunk,
// so we can fall back to plain text if Telegram rejects the HTML.
type FormattedChunk struct {
	HTML string
	Text string
}

var mdParser goldmark.Markdown

func init() {
	mdParser = goldmark.New(
		goldmark.WithExtensions(
			extension.Strikethrough,
			extension.Table,
		),
		// Allow preprocessed <tg-spoiler>/<sub>/… tags through to the AST as raw HTML.
		goldmark.WithRendererOptions(htmlrender.WithUnsafe()),
	)
}

type tagEntry struct {
	open  string
	close string
}

type listState struct {
	ordered bool
	index   int
}

type htmlWriter struct {
	html      strings.Builder
	plain     strings.Builder
	limit     int
	chunks    []FormattedChunk
	tagStack  []tagEntry
	listStack   []listState
	nativeTable bool // emit <table> for rich messages instead of bullets
}

func (w *htmlWriter) writeText(s string) {
	w.html.WriteString(escapeHTML(s))
	w.plain.WriteString(s)
}

// writeInline escapes and applies Telegram-extended inline markdown (spoiler,
// mark, underline, sub, sup) for rich-message mode. Used for normal text
// nodes only — code spans/blocks keep writeText so markers stay literal.
func (w *htmlWriter) writeInline(s string) {
	if w.nativeTable {
		w.html.WriteString(rich.ExtendedInlineHTML(s))
	} else {
		w.html.WriteString(escapeHTML(s))
	}
	w.plain.WriteString(s)
}

// writeRaw inserts trusted HTML (e.g. <tg-math>) without escaping tags.
func (w *htmlWriter) writeRaw(s string) {
	w.html.WriteString(s)
	w.plain.WriteString(s)
}

func (w *htmlWriter) openTag(open, close string) {
	w.html.WriteString(open)
	w.tagStack = append(w.tagStack, tagEntry{open: open, close: close})
}

func (w *htmlWriter) closeTag() {
	if len(w.tagStack) == 0 {
		return
	}
	top := w.tagStack[len(w.tagStack)-1]
	w.tagStack = w.tagStack[:len(w.tagStack)-1]
	w.html.WriteString(top.close)
}

func (w *htmlWriter) inList() bool {
	return len(w.listStack) > 0
}

func (w *htmlWriter) listPrefix() string {
	if len(w.listStack) == 0 {
		return ""
	}
	top := &w.listStack[len(w.listStack)-1]
	top.index++
	indent := strings.Repeat("  ", max(0, len(w.listStack)-1))
	if top.ordered {
		return fmt.Sprintf("%s%d. ", indent, top.index)
	}
	return indent + "• "
}

// flush saves the current buffer as a chunk and resets for the next one.
// Open tags are closed at the end and reopened at the start of the next chunk.
func (w *htmlWriter) flush() {
	if w.plain.Len() == 0 {
		return
	}
	var closing strings.Builder
	for i := len(w.tagStack) - 1; i >= 0; i-- {
		closing.WriteString(w.tagStack[i].close)
	}
	w.chunks = append(w.chunks, FormattedChunk{
		HTML: w.html.String() + closing.String(),
		Text: w.plain.String(),
	})
	w.html.Reset()
	w.plain.Reset()
	for _, tag := range w.tagStack {
		w.html.WriteString(tag.open)
	}
}

func (w *htmlWriter) maybeFlush() {
	// Flush on plain-text length or HTML length (escapes/tags can expand size).
	// Telegram limit is 4096 on the text field; keep headroom for open tags.
	if w.limit > 0 && (w.plain.Len() >= w.limit || w.html.Len() >= w.limit) {
		w.flush()
	}
}

func (w *htmlWriter) finish() []FormattedChunk {
	htmlStr := strings.TrimRight(w.html.String(), "\n ")
	plainStr := strings.TrimRight(w.plain.String(), "\n ")
	if plainStr != "" {
		var closing strings.Builder
		for i := len(w.tagStack) - 1; i >= 0; i-- {
			closing.WriteString(w.tagStack[i].close)
		}
		w.chunks = append(w.chunks, FormattedChunk{
			HTML: htmlStr + closing.String(),
			Text: plainStr,
		})
	}
	return w.chunks
}

func (w *htmlWriter) walkNode(n ast.Node, source []byte) {
	switch node := n.(type) {
	case *ast.Document:
		w.walkChildren(n, source)

	case *ast.Paragraph:
		// Rich HTML collapses whitespace: use real <p> blocks so newlines survive.
		if w.nativeTable {
			if w.inList() {
				w.walkChildren(n, source)
			} else {
				w.openTag("<p>", "</p>")
				w.walkChildren(n, source)
				w.closeTag()
				w.plain.WriteString("\n\n")
				w.maybeFlush()
			}
		} else {
			w.walkChildren(n, source)
			if !w.inList() {
				w.writeText("\n\n")
				w.maybeFlush()
			}
		}

	case *ast.Heading:
		if w.nativeTable {
			level := node.Level
			if level < 1 {
				level = 1
			}
			if level > 6 {
				level = 6
			}
			open := fmt.Sprintf("<h%d>", level)
			close := fmt.Sprintf("</h%d>", level)
			w.openTag(open, close)
			w.walkChildren(n, source)
			w.closeTag()
			w.plain.WriteString("\n\n")
		} else {
			w.openTag("<b>", "</b>")
			w.walkChildren(n, source)
			w.closeTag()
			w.writeText("\n\n")
		}
		w.maybeFlush()

	case *ast.Emphasis:
		if node.Level == 2 {
			w.openTag("<b>", "</b>")
		} else {
			w.openTag("<i>", "</i>")
		}
		w.walkChildren(n, source)
		w.closeTag()

	case *ast.CodeSpan:
		w.openTag("<code>", "</code>")
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if t, ok := c.(*ast.Text); ok {
				w.writeText(string(t.Segment.Value(source)))
			}
		}
		w.closeTag()

	case *ast.FencedCodeBlock, *ast.CodeBlock:
		w.openTag("<pre><code>", "</code></pre>")
		lines := n.Lines()
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			w.writeText(string(seg.Value(source)))
			if w.limit > 0 && w.plain.Len() >= w.limit && i+1 < lines.Len() {
				w.flush()
			}
		}
		if p := w.plain.String(); len(p) > 0 && !strings.HasSuffix(p, "\n") {
			w.writeText("\n")
		}
		w.closeTag()
		if !w.inList() {
			if w.nativeTable {
				w.plain.WriteString("\n")
			} else {
				w.writeText("\n")
			}
			w.maybeFlush()
		}

	case *ast.Blockquote:
		w.openTag("<blockquote>", "</blockquote>")
		w.walkChildren(n, source)
		w.closeTag()
		if w.nativeTable {
			w.plain.WriteString("\n")
		}

	case *ast.Link:
		href := strings.TrimSpace(string(node.Destination))
		label := extractNodeText(n, source)
		if href == "" || isAutoLinkedFileRef(href, label) {
			w.walkChildren(n, source)
		} else {
			w.openTag(`<a href="`+escapeHTMLAttr(href)+`">`, "</a>")
			w.walkChildren(n, source)
			w.closeTag()
		}

	case *ast.AutoLink:
		href := string(node.URL(source))
		label := string(node.Label(source))
		if isAutoLinkedFileRef(href, label) {
			w.writeText(label)
		} else {
			w.openTag(`<a href="`+escapeHTMLAttr(href)+`">`, "</a>")
			w.writeText(label)
			w.closeTag()
		}

	case *ast.Image:
		dest := strings.TrimSpace(string(node.Destination))
		alt := extractNodeText(n, source)
		if dest == "" {
			w.writeText(alt)
			break
		}
		if isSafeMediaURL(dest) {
			tag := `<img src="` + escapeHTMLAttr(dest) + `"`
			if alt != "" {
				tag += ` alt="` + escapeHTMLAttr(alt) + `"`
			}
			tag += `/>`
			if w.nativeTable {
				w.writeRaw(tag)
				w.plain.WriteString(alt)
			} else {
				w.writeText(alt)
				if alt == "" {
					w.writeText(dest)
				}
			}
		} else {
			w.writeText(alt)
			if alt == "" {
				w.writeText(dest)
			}
		}


	case *ast.Text:
		w.writeInline(string(node.Segment.Value(source)))
		if node.SoftLineBreak() || node.HardLineBreak() {
			if w.nativeTable {
				w.writeRaw("<br/>")
				w.plain.WriteString("\n")
			} else {
				w.writeText("\n")
			}
		}

	case *ast.String:
		w.writeText(string(node.Value))

	case *ast.List:
		if w.nativeTable {
			tag := "ul"
			if node.IsOrdered() {
				tag = "ol"
			}
			w.openTag("<"+tag+">", "</"+tag+">")
			w.listStack = append(w.listStack, listState{
				ordered: node.IsOrdered(),
				index:   node.Start - 1,
			})
			w.walkChildren(n, source)
			w.listStack = w.listStack[:len(w.listStack)-1]
			w.closeTag()
			w.plain.WriteString("\n")
			if !w.inList() {
				w.maybeFlush()
			}
		} else {
			if w.inList() {
				w.writeText("\n")
			}
			w.listStack = append(w.listStack, listState{
				ordered: node.IsOrdered(),
				index:   node.Start - 1,
			})
			w.walkChildren(n, source)
			w.listStack = w.listStack[:len(w.listStack)-1]
			if !w.inList() {
				w.writeText("\n")
				w.maybeFlush()
			}
		}

	case *ast.ListItem:
		if w.nativeTable {
			w.openTag("<li>", "</li>")
			w.walkChildren(n, source)
			w.closeTag()
			w.plain.WriteString("\n")
		} else {
			w.writeText(w.listPrefix())
			w.walkChildren(n, source)
			if !strings.HasSuffix(w.plain.String(), "\n") {
				w.writeText("\n")
			}
		}

	case *ast.ThematicBreak:
		if w.nativeTable {
			w.writeRaw("<hr/>")
			w.plain.WriteString("\n\n")
		} else {
			w.writeText("───\n\n")
		}
		w.maybeFlush()

	case *ast.HTMLBlock:
		lines := n.Lines()
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			s := string(seg.Value(source))
			if w.nativeTable && isTrustedRichHTML(s) {
				w.writeRaw(s)
			} else {
				w.writeText(s)
			}
		}

	case *ast.RawHTML:
		segs := node.Segments
		for i := 0; i < segs.Len(); i++ {
			seg := segs.At(i)
			s := string(seg.Value(source))
			if w.nativeTable && isTrustedRichHTML(s) {
				w.writeRaw(s)
			} else {
				w.writeText(s)
			}
		}

	case *east.Strikethrough:
		w.openTag("<s>", "</s>")
		w.walkChildren(n, source)
		w.closeTag()

	case *east.Table:
		if w.nativeTable {
			w.renderNativeTable(n, source)
		} else {
			w.renderTableAsBullets(n, source)
		}

	default:
		w.walkChildren(n, source)
	}
}

func (w *htmlWriter) walkChildren(n ast.Node, source []byte) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		w.walkNode(c, source)
	}
}

func (w *htmlWriter) renderTableAsBullets(table ast.Node, source []byte) {
	var headers []string
	var rows [][]string

	for child := table.FirstChild(); child != nil; child = child.NextSibling() {
		switch child.(type) {
		case *east.TableHeader:
			for cell := child.FirstChild(); cell != nil; cell = cell.NextSibling() {
				headers = append(headers, extractCellText(cell, source))
			}
		case *east.TableRow:
			var rowCells []string
			for cell := child.FirstChild(); cell != nil; cell = cell.NextSibling() {
				rowCells = append(rowCells, extractCellText(cell, source))
			}
			rows = append(rows, rowCells)
		}
	}

	if len(headers) == 0 && len(rows) == 0 {
		return
	}

	useFirstColAsLabel := len(headers) > 1 && len(rows) > 0

	if useFirstColAsLabel {
		for _, row := range rows {
			if len(row) == 0 {
				continue
			}
			w.openTag("<b>", "</b>")
			w.writeText(row[0])
			w.closeTag()
			w.writeText("\n")
			for i := 1; i < len(row); i++ {
				w.writeText("• ")
				if i < len(headers) && headers[i] != "" {
					w.writeText(headers[i])
					w.writeText(": ")
				}
				w.writeText(row[i])
				w.writeText("\n")
			}
			w.writeText("\n")
		}
	} else {
		for _, row := range rows {
			for i, cell := range row {
				w.writeText("• ")
				if i < len(headers) && headers[i] != "" {
					w.writeText(headers[i])
					w.writeText(": ")
				}
				w.writeText(cell)
				w.writeText("\n")
			}
			w.writeText("\n")
		}
	}
}



// FormatRichHTML converts markdown into a single Telegram Rich HTML document.
// Unlike FormatHTMLChunks (classic HTML, tables → bullets), this emits native
// <table bordered striped> blocks suitable for sendRichMessage html mode.
func FormatRichHTML(s string) string {
	if s == "" {
		return ""
	}
	// Tokens for spoiler/mark/sub/sup so goldmark does not treat ~sub~ as strike.
	s, phs := rich.PreprocessExtendedMarkdown(s)
	source := []byte(s)
	doc := mdParser.Parser().Parse(mdtext.NewReader(source))
	w := &htmlWriter{limit: 0, nativeTable: true}
	w.walkNode(doc, source)
	result := w.finish()
	if len(result) == 0 {
		return rich.RestoreInlinePlaceholders(escapeHTML(s), phs)
	}
	var b strings.Builder
	for i, ch := range result {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(ch.HTML)
	}
	return rich.RestoreInlinePlaceholders(b.String(), phs)
}

func (w *htmlWriter) renderNativeTable(table ast.Node, source []byte) {
	var headers []string
	var rows [][]string
	for child := table.FirstChild(); child != nil; child = child.NextSibling() {
		switch child.(type) {
		case *east.TableHeader:
			for cell := child.FirstChild(); cell != nil; cell = cell.NextSibling() {
				headers = append(headers, extractCellText(cell, source))
			}
		case *east.TableRow:
			var rowCells []string
			for cell := child.FirstChild(); cell != nil; cell = cell.NextSibling() {
				rowCells = append(rowCells, extractCellText(cell, source))
			}
			rows = append(rows, rowCells)
		}
	}
	if len(headers) == 0 && len(rows) == 0 {
		return
	}
	w.html.WriteString(`<table bordered striped compact>`)
	if len(headers) > 0 {
		w.html.WriteString("<tr>")
		for _, h := range headers {
			w.html.WriteString("<th align=\"center\">")
			w.writeText(h)
			w.html.WriteString("</th>")
		}
		w.html.WriteString("</tr>")
	}
	for _, row := range rows {
		w.html.WriteString("<tr>")
		for _, cell := range row {
			w.html.WriteString("<td align=\"left\">")
			w.writeText(cell)
			w.html.WriteString("</td>")
		}
		w.html.WriteString("</tr>")
	}
	w.html.WriteString("</table>")
	w.writeText("\n")
	w.maybeFlush()
}

func isTrustedRichHTML(s string) bool {
	s = strings.TrimSpace(s)
	return strings.Contains(s, "<tg-math") ||
		strings.Contains(s, "</tg-math>") ||
		strings.Contains(s, "<tg-spoiler") ||
		strings.Contains(s, "</tg-spoiler>") ||
		strings.Contains(s, "<tg-time") ||
		strings.Contains(s, "<details") ||
		strings.Contains(s, "<table") ||
		strings.Contains(s, "<mark") ||
		strings.Contains(s, "<sub") ||
		strings.Contains(s, "<sup") ||
		strings.Contains(s, "<u>") ||
		strings.Contains(s, "</u>") ||
		strings.Contains(s, "<img") ||
		strings.Contains(s, "<video") ||
		strings.Contains(s, "<audio") ||
		strings.Contains(s, "<figure") ||
		strings.Contains(s, "<tg-collage") ||
		strings.Contains(s, "<tg-slideshow") ||
		strings.Contains(s, "<tg-document") ||
		strings.Contains(s, "<figcaption")
}

// FormatHTMLChunks converts markdown into Telegram HTML chunks, each within
// the byte limit. Tags are properly closed/reopened across chunk boundaries.
func FormatHTMLChunks(s string, limit int) []FormattedChunk {
	if s == "" {
		return nil
	}
	source := []byte(s)
	doc := mdParser.Parser().Parse(mdtext.NewReader(source))
	w := &htmlWriter{limit: limit}
	w.walkNode(doc, source)
	result := w.finish()
	if len(result) == 0 {
		return []FormattedChunk{{HTML: escapeHTML(s), Text: s}}
	}
	return result
}

func extractNodeText(n ast.Node, source []byte) string {
	var b strings.Builder
	extractTextRecursive(n, source, &b)
	return b.String()
}

func extractCellText(cell ast.Node, source []byte) string {
	var b strings.Builder
	extractTextRecursive(cell, source, &b)
	return strings.TrimSpace(b.String())
}

func extractTextRecursive(n ast.Node, source []byte, b *strings.Builder) {
	if t, ok := n.(*ast.Text); ok {
		b.Write(t.Segment.Value(source))
		return
	}
	if _, ok := n.(*ast.CodeSpan); ok {
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if t, ok := c.(*ast.Text); ok {
				b.Write(t.Segment.Value(source))
			}
		}
		return
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		extractTextRecursive(c, source, b)
	}
}

// isSafeMediaURL allows https/http and Telegram tg:// media/emoji references.
func isSafeMediaURL(u string) bool {
	u = strings.TrimSpace(u)
	low := strings.ToLower(u)
	return strings.HasPrefix(low, "https://") ||
		strings.HasPrefix(low, "http://") ||
		strings.HasPrefix(low, "tg://photo") ||
		strings.HasPrefix(low, "tg://video") ||
		strings.HasPrefix(low, "tg://document") ||
		strings.HasPrefix(low, "tg://audio") ||
		strings.HasPrefix(low, "tg://emoji")
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

// File extensions that overlap with TLDs. When these appear as bare filenames
// (e.g. README.md), Telegram's linkify turns them into domain previews.
var fileExtTLDs = map[string]bool{
	"md": true, "go": true, "py": true, "pl": true, "sh": true,
	"am": true, "at": true, "be": true, "cc": true,
}

func isAutoLinkedFileRef(href, label string) bool {
	stripped := strings.TrimPrefix(strings.TrimPrefix(href, "https://"), "http://")
	if stripped != label {
		return false
	}
	dot := strings.LastIndex(label, ".")
	if dot < 1 {
		return false
	}
	ext := strings.ToLower(label[dot+1:])
	return fileExtTLDs[ext]
}
