package telegram

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gemechischala/awaki/internal/channel/telegram/format"
	"github.com/gemechischala/awaki/internal/channel/telegram/rich"
	"github.com/gemechischala/awaki/internal/transport"

	"github.com/go-telegram/bot/models"
	"github.com/rs/zerolog/log"
)

// Rich status can show far more than classic 4k messages.
const (
	statusDetailVisibleRunes = 1200 // shown inline before folding
	statusDetailMaxRunes     = 12000
	statusPlanMaxTasks       = 40
)

var phaseEmoji = map[transport.Phase]string{
	transport.PhaseThinking: "\U0001f9e0",
	transport.PhasePlanning: "\U0001f4cb",
	transport.PhaseTool:     "\U0001f527",
	transport.PhaseWorking:  "\u26a1",
	transport.PhaseMessage:  "\U0001f4ac",
}

func taskStateEmoji(s transport.TaskStatus) string {
	switch s {
	case transport.TaskCompleted:
		return "✅"
	case transport.TaskInProgress:
		return "▶️"
	default:
		return "⬜"
	}
}

// formatStatus is the plain-text fallback (MarkdownV2) for short statuses.
func formatStatus(s transport.AgentStatus) string {
	var parts []string

	if len(s.Plan) > 0 {
		done := 0
		for _, t := range s.Plan {
			if t.Status == transport.TaskCompleted {
				done++
			}
		}
		parts = append(parts, fmt.Sprintf("📋 *Plan* %d/%d", done, len(s.Plan)))
	}

	if s.Percent != nil {
		parts = append(parts, formatProgressBar(*s.Percent))
	}

	emoji := phaseEmoji[s.Phase]
	if emoji == "" {
		emoji = "⏳"
	}

	label := s.Detail
	if label == "" {
		switch s.Phase {
		case transport.PhaseThinking:
			label = "Thinking"
		case transport.PhasePlanning:
			label = "Planning"
		default:
			label = s.Tool
		}
	}
	if label != "" {
		if s.Phase == transport.PhaseMessage {
			runes := []rune(label)
			if len(runes) > 300 {
				label = string(runes[:300]) + "…"
			}
			parts = append(parts, emoji+" "+label)
		} else {
			parts = append(parts, emoji+" "+label+"\\.\\.\\.")
		}
	}

	if len(parts) == 0 {
		return emoji + " Working\\.\\.\\."
	}
	return strings.Join(parts, "\n")
}

// formatStatusRich renders an AgentStatus as Telegram Rich HTML.
// Uses the larger rich-message budget: commands in <code>/<pre>, long
// output folded into <details>, plan in a collapsible drawer. The same
// message is edited in place via EditRichMessage while the agent runs.
func formatStatusRich(s transport.AgentStatus) string {
	var b strings.Builder

	emoji := phaseEmoji[s.Phase]
	if emoji == "" {
		emoji = "⏳"
	}

	phaseLabel := ""
	switch s.Phase {
	case transport.PhaseThinking:
		phaseLabel = "Thinking"
	case transport.PhasePlanning:
		phaseLabel = "Planning"
	case transport.PhaseTool:
		phaseLabel = "Running tool"
	case transport.PhaseWorking:
		phaseLabel = "Working"
	case transport.PhaseMessage:
		phaseLabel = "Update"
	default:
		phaseLabel = "Working"
	}

	b.WriteString(rich.Heading(3, emoji+" "+phaseLabel))

	// Tool name as tap-to-copy code
	if s.Tool != "" {
		b.WriteString("<p>Tool: " + rich.CodeBlock(s.Tool) + "</p>")
	}

	detail := strings.TrimSpace(s.Detail)
	if detail == "" && s.Tool == "" {
		b.WriteString(rich.Paragraph("…"))
	} else if detail != "" {
		// Commands / shell-like detail → fenced code block (tap-to-copy on mobile).
		if looksLikeCommand(detail) || s.Phase == transport.PhaseTool {
			cmd, rest := splitCommandAndOutput(detail)
			if cmd != "" {
				b.WriteString(rich.PreBlock(truncateRunes(cmd, 4000)))
			}
			if rest != "" {
				vis, folded := foldLong(rest, statusDetailVisibleRunes, statusDetailMaxRunes)
				if vis != "" && folded == "" {
					b.WriteString(rich.Paragraph(vis))
				} else if vis != "" {
					b.WriteString(rich.Paragraph(vis))
				}
				if folded != "" {
					b.WriteString(rich.DetailsPlain("📄 Output / response", folded, false))
				}
			} else if cmd == "" {
				b.WriteString(rich.PreBlock(truncateRunes(detail, 4000)))
			}
		} else {
			vis, folded := foldLong(detail, statusDetailVisibleRunes, statusDetailMaxRunes)
			if vis != "" {
				b.WriteString(rich.Paragraph(vis))
			}
			if folded != "" {
				b.WriteString(rich.DetailsPlain("📄 More detail", folded, false))
			}
		}
	}

	if s.Percent != nil {
		b.WriteString(rich.CodeBlock(formatProgressBar(*s.Percent)))
	}

	if len(s.Plan) > 0 {
		var planLines []string
		done := 0
		n := 0
		for _, t := range s.Plan {
			if n >= statusPlanMaxTasks {
				planLines = append(planLines, fmt.Sprintf("… +%d more", len(s.Plan)-statusPlanMaxTasks))
				break
			}
			planLines = append(planLines, fmt.Sprintf("%s %s", taskStateEmoji(t.Status), t.Title))
			if t.Status == transport.TaskCompleted {
				done++
			}
			n++
		}
		planBody := strings.Join(planLines, "\n")
		b.WriteString(rich.DetailsPlain(fmt.Sprintf("📋 Plan %d/%d", done, len(s.Plan)), planBody, false))
	}

	if b.Len() == 0 {
		return rich.Paragraph(emoji + " Working…")
	}
	return rich.EnsureSafeLength(b.String())
}

func looksLikeCommand(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	// Heuristics: shell prompts, common tools, paths, flags
	if strings.HasPrefix(s, "$ ") || strings.HasPrefix(s, "# ") || strings.HasPrefix(s, "> ") {
		return true
	}
	first := strings.Fields(s)
	if len(first) == 0 {
		return false
	}
	cmd := first[0]
	if strings.ContainsAny(cmd, "/\\") {
		return true
	}
	switch cmd {
	case "go", "git", "npm", "npx", "yarn", "pnpm", "pip", "python", "python3",
		"node", "docker", "kubectl", "curl", "wget", "make", "cmake", "cargo",
		"bash", "sh", "zsh", "sudo", "apt", "apt-get", "brew", "systemctl",
		"cat", "ls", "cd", "rm", "cp", "mv", "mkdir", "chmod", "chown", "ssh",
		"scp", "rsync", "tar", "unzip", "grep", "rg", "find", "sed", "awk":
		return true
	}
	if strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "../") {
		return true
	}
	return false
}

// splitCommandAndOutput splits "cmd\noutput..." style detail.
func splitCommandAndOutput(detail string) (cmd, rest string) {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return "", ""
	}
	// "label: command" form from buildToolStatus
	if i := strings.Index(detail, ": "); i > 0 && i < 80 {
		prefix := detail[:i]
		// If prefix looks like a short label, keep full string as command when no newline
		if !strings.Contains(detail, "\n") && looksLikeCommand(strings.TrimSpace(detail[i+2:])) {
			return strings.TrimSpace(detail[i+2:]), ""
		}
		_ = prefix
	}
	if i := strings.Index(detail, "\n"); i >= 0 {
		return strings.TrimSpace(detail[:i]), strings.TrimSpace(detail[i+1:])
	}
	if looksLikeCommand(detail) {
		return detail, ""
	}
	return "", detail
}

func foldLong(s string, visible, max int) (head, folded string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	r := []rune(s)
	if len(r) > max {
		r = append(r[:max], '…')
	}
	if len(r) <= visible {
		return string(r), ""
	}
	// Break at newline near boundary when possible
	cut := visible
	for i := visible; i > visible-200 && i > 0; i-- {
		if r[i] == '\n' {
			cut = i
			break
		}
	}
	return string(r[:cut]) + "…", string(r[cut:])
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func formatProgressBar(pct int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	const width = 10
	filled := pct * width / 100
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return fmt.Sprintf("%s %d%%", bar, pct)
}

// planKeyboard builds read-only inline buttons: one row per task.
func planKeyboard(tasks []transport.PlanTask) [][]models.InlineKeyboardButton {
	if len(tasks) == 0 {
		return nil
	}
	var rows [][]models.InlineKeyboardButton
	for i, t := range tasks {
		label := taskStateEmoji(t.Status) + " " + t.Title
		runes := []rune(label)
		if len(runes) > 64 {
			label = string(runes[:61]) + "…"
		}
		rows = append(rows, []models.InlineKeyboardButton{{
			Text:         label,
			CallbackData: fmt.Sprintf("plan:noop:%d", i),
		}})
	}
	return rows
}

func (r *replier) sendStatus(ctx context.Context, status transport.AgentStatus) int {
	html := formatStatusRich(status)
	id, err := r.ch.client.SendRichMessage(ctx, r.chatID, "html", html, false, false)
	if err != nil {
		log.Warn().Err(err).Msg("telegram: send rich status failed, falling back to plain")
		text := formatStatus(status)
		return r.ch.sendAndGetID(ctx, r.chatID, text)
	}
	return id
}

func (r *replier) updateStatus(ctx context.Context, msgID int, status transport.AgentStatus) {
	if msgID == 0 {
		return
	}
	html := formatStatusRich(status)
	err := r.ch.client.EditRichMessage(ctx, r.chatID, msgID, "html", html, false)
	if err != nil {
		log.Warn().Err(err).Int("msg_id", msgID).Str("phase", string(status.Phase)).Msg("telegram: edit rich status failed, falling back to plain")
		text := formatStatus(status)
		kb := planKeyboard(status.Plan)
		var err2 error
		if len(kb) > 0 {
			err2 = r.ch.client.EditMessageWithKeyboard(ctx, r.chatID, msgID, text, models.ParseModeMarkdown, kb)
		} else {
			err2 = r.ch.client.EditMessage(ctx, r.chatID, msgID, text, models.ParseModeMarkdown)
		}
		if err2 != nil {
			log.Warn().Err(err2).Int("msg_id", msgID).Str("phase", string(status.Phase)).Msg("telegram: EditMessage status failed")
		}
	}
}

func (r *replier) deleteStatus(ctx context.Context, msgID int) {
	if msgID == 0 {
		return
	}
	if err := r.ch.client.DeleteMessage(ctx, r.chatID, msgID); err != nil {
		log.Warn().Err(err).Int("msg_id", msgID).Msg("telegram: DeleteMessage failed")
	}
}

// editToFinal replaces the status message with the final response text (rich-first).
func (r *replier) editToFinal(ctx context.Context, msgID int, text string) {
	if msgID == 0 {
		_ = r.SendText(ctx, text)
		return
	}

	isRTL := rich.NeedsRTL(text)
	// Prefer markdown rich edit (tables, lists, code intact).
	err := r.ch.client.EditRichMessage(ctx, r.chatID, msgID, "markdown", text, isRTL)
	if err == nil {
		return
	}
	log.Warn().Err(err).Msg("telegram: edit to rich markdown final failed, trying HTML")

	md := rich.ConvertMathMarkdown(text)
	html := format.FormatRichHTML(md)
	if html == "" {
		html = rich.MarkdownToRichHTML(md)
	}
	html = rich.EnsureSafeLength(html)
	err = r.ch.client.EditRichMessage(ctx, r.chatID, msgID, "html", html, isRTL)
	if err == nil {
		return
	}
	log.Warn().Err(err).Msg("telegram: edit to rich HTML final failed, falling back to plain")
	r.editToFinalPlain(ctx, msgID, text)
}

func (r *replier) editToFinalPlain(ctx context.Context, msgID int, text string) {
	chunks := format.FormatHTMLChunks(text, maxMessageLen)
	if len(chunks) == 0 {
		r.deleteStatus(ctx, msgID)
		return
	}

	if len(chunks) == 1 {
		err := r.ch.client.EditMessage(ctx, r.chatID, msgID, chunks[0].HTML, models.ParseModeHTML)
		if err != nil {
			log.Warn().Err(err).Msg("telegram: edit to final HTML failed, trying plain text")
			err = r.ch.client.EditMessage(ctx, r.chatID, msgID, chunks[0].Text, "")
			if err != nil {
				log.Warn().Err(err).Msg("telegram: edit to final plain failed, falling back to delete+send")
				r.deleteStatus(ctx, msgID)
				_ = r.SendText(ctx, text)
			}
		}
		return
	}

	err := r.ch.client.EditMessage(ctx, r.chatID, msgID, chunks[0].HTML, models.ParseModeHTML)
	if err != nil {
		log.Warn().Err(err).Msg("telegram: edit first chunk failed, falling back to delete+send")
		r.deleteStatus(ctx, msgID)
		_ = r.SendText(ctx, text)
		return
	}
	for _, chunk := range chunks[1:] {
		r.ch.sendHTMLChunk(ctx, r.chatID, chunk)
	}
}
