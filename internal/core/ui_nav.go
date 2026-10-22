package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/providers"
	"github.com/gemechischala/awaki/internal/store"
)

// Reply-keyboard labels (persistent bottom bar).
const (
	navNewChat  = "💬 New Chat"
	navHistory  = "📜 History"
	navSettings = "⚙️ Settings"
	navProfile  = "👤 Profile"
)

func mainMenuKeyboard() channel.KeyboardMessage {
	return channel.KeyboardMessage{
		Text: "👋 Welcome to Awaki\n\n" +
			"Self-hosted AI agent — pick an action below, or just send a message once a model is configured.",
		Rows: [][]channel.Button{
			{
				{Text: "💬 New Chat", CallbackData: "ui:new"},
				{Text: "📜 History", CallbackData: "ui:hist:0"},
			},
			{
				{Text: "⚙️ Settings", CallbackData: "ui:settings"},
				{Text: "👤 Profile", CallbackData: "ui:profile"},
			},
		},
		ReplyKeyboard: [][]string{
			{navNewChat, navHistory},
			{navSettings, navProfile},
		},
	}
}

func settingsKeyboard() channel.KeyboardMessage {
	return channel.KeyboardMessage{
		Text: "⚙️ Settings\n\nConfigure AI providers, models, and agent identity.",
		Rows: [][]channel.Button{
			{{Text: "🔌 Manage AI Providers", CallbackData: "ui:providers:0"}},
			{{Text: "🧠 Models (all)", CallbackData: "ui:models:0"}},
			{{Text: "🧬 Soul", CallbackData: "ui:soul"}, {Text: "🧠 Memory", CallbackData: "ui:memory"}},
			{{Text: "⬅ Home", CallbackData: "ui:home"}},
		},
	}
}

type keyboardSender interface {
	SendKeyboardToOwner(ctx context.Context, km channel.KeyboardMessage)
	SendOrEditKeyboardToOwner(ctx context.Context, editMsgID int, km channel.KeyboardMessage)
}


func (e *Engine) uiHome(ctx context.Context, msg channel.Message) {
	e.ui.clear()
	e.deliverKeyboard(ctx, msg, mainMenuKeyboard())
}

func (e *Engine) uiSettings(ctx context.Context, msg channel.Message) {
	e.ui.clear()
	e.deliverKeyboard(ctx, msg, settingsKeyboard())
}

func (e *Engine) uiProfile(ctx context.Context, msg channel.Message) {
	user := e.store.ReadUser()
	soul := e.store.ReadSoul()
	var b strings.Builder
	b.WriteString("👤 Profile\n\n")
	if user != "" {
		b.WriteString("User:\n")
		b.WriteString(truncateUI(user, 800))
		b.WriteString("\n\n")
	} else {
		b.WriteString("User profile empty (user.md).\n\n")
	}
	if soul != "" {
		b.WriteString("Soul (identity):\n")
		b.WriteString(truncateUI(soul, 400))
	}
	active := "none"
	if m, p, err := e.providers.ActivePair(); err == nil {
		active = fmt.Sprintf("%s / %s", p.Name, m.DisplayName)
	}
	b.WriteString("\n\nActive model: ")
	b.WriteString(active)
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: b.String(),
		Rows: [][]channel.Button{{{Text: "⬅ Home", CallbackData: "ui:home"}}},
	})
}

func (e *Engine) uiHistory(ctx context.Context, msg channel.Message, page int) {
	chats, err := e.store.ListChats()
	if err != nil {
		_ = msg.Reply.SendText(ctx, "History error: "+err.Error())
		return
	}
	var closed []store.ChatInfo
	for _, c := range chats {
		if c.Status == "closed" || c.Status == "active" {
			closed = append(closed, c)
		}
	}
	total := len(closed)
	if total == 0 {
		e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
			Text: "📜 No conversations yet.",
			Rows: [][]channel.Button{{{Text: "⬅ Home", CallbackData: "ui:home"}}},
		})
		return
	}
	if page < 0 {
		page = 0
	}
	start := page * pageSizeHistory
	if start >= total {
		page = 0
		start = 0
	}
	end := start + pageSizeHistory
	if end > total {
		end = total
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("📜 History (page %d)\n\n", page+1))
	var rows [][]channel.Button
	for _, c := range closed[start:end] {
		title := c.Title
		if title == "" {
			title = c.ID
		}
		title = truncateUI(title, 32)
		b.WriteString(fmt.Sprintf("• %s\n", title))
		rows = append(rows, []channel.Button{
			{Text: "▶ " + title, CallbackData: "ui:open:" + c.ID},
		})
	}
	var nav []channel.Button
	if page > 0 {
		nav = append(nav, channel.Button{Text: "◀ Previous", CallbackData: fmt.Sprintf("ui:hist:%d", page-1)})
	}
	if end < total {
		nav = append(nav, channel.Button{Text: "Next ▶", CallbackData: fmt.Sprintf("ui:hist:%d", page+1)})
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, []channel.Button{{Text: "⬅ Home", CallbackData: "ui:home"}})
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{Text: b.String(), Rows: rows})
}

func (e *Engine) deliverKeyboard(ctx context.Context, msg channel.Message, km channel.KeyboardMessage) {
	if ks, ok := e.ch.(keyboardSender); ok {
		if msg.EditMessageID > 0 {
			ks.SendOrEditKeyboardToOwner(ctx, msg.EditMessageID, km)
			return
		}
		ks.SendKeyboardToOwner(ctx, km)
		return
	}
	var b strings.Builder
	b.WriteString(km.Text)
	for _, row := range km.Rows {
		for _, btn := range row {
			b.WriteString("\n[")
			b.WriteString(btn.Text)
			b.WriteString("]")
		}
	}
	if msg.Reply != nil {
		_ = msg.Reply.SendText(ctx, b.String())
	}
}


func truncateUI(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func parsePage(s string) int {
	n, _ := strconv.Atoi(s)
	if n < 0 {
		return 0
	}
	return n
}

func protocolShort(p providers.Protocol) string {
	switch p {
	case providers.ProtocolOpenAIChat:
		return "oc"
	case providers.ProtocolOpenAIResponses:
		return "or"
	case providers.ProtocolAnthropic:
		return "an"
	case providers.ProtocolGemini:
		return "gm"
	default:
		return "oc"
	}
}

func protocolFromShort(s string) (providers.Protocol, bool) {
	switch s {
	case "oc":
		return providers.ProtocolOpenAIChat, true
	case "or":
		return providers.ProtocolOpenAIResponses, true
	case "an":
		return providers.ProtocolAnthropic, true
	case "gm":
		return providers.ProtocolGemini, true
	}
	return providers.ParseProtocol(s)
}
