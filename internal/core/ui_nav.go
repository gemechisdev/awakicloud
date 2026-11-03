package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/providers"
)

// Reply-keyboard labels (persistent bottom bar).
const (
	navNewChat  = "💬 New Chat"
	navChats    = "📜 Chats"
	navHistory  = "📜 History" // legacy reply-keyboard label (older clients still send it)
	navProjects = "📁 Projects"
	navSettings = "⚙️ Settings"
	navProfile  = "👤 Profile"
)

func mainMenuKeyboard(current string) channel.KeyboardMessage {
	return channel.KeyboardMessage{
		Text: "👋 **Welcome to Awaki**\n\n" +
			"Your self-hosted AI agent on Telegram.\n\n" +
			current +
			"**Quick start**\n" +
			"1. Settings → Providers — add an API key\n" +
			"2. Pick a model under Models\n" +
			"3. Send a message, or open a Project for durable work\n\n" +
			"**Useful**\n" +
			"• /mode — auto · ask · plan permissions\n" +
			"• /plan <task> — one-shot read-only plan\n" +
			"• /tasks — background work with live progress\n" +
			"• /help — full command list\n\n" +
			"Just type when a model is configured.",
		Rows: [][]channel.Button{
			{
				{Text: "💬 New Chat", CallbackData: cb("new")},
				{Text: "📜 Chats", CallbackData: cb("chats")},
			},
			{
				{Text: "📁 Projects", CallbackData: cb("projects", "0")},
				{Text: "📍 This chat", CallbackData: cb("cur")},
			},
			{
				{Text: "⚙️ Settings", CallbackData: "ui:settings"},
				{Text: "👤 Profile", CallbackData: "ui:profile"},
			},
			{
				{Text: "📋 Tasks", CallbackData: "ui:tasks"},
				{Text: "🛡️ Mode", CallbackData: "ui:mode"},
			},
			{
				{Text: "👨‍💻 Developer", URL: "tg://user?id=8345389653"},
			},
		},
		ReplyKeyboard: [][]string{
			{navNewChat, navChats},
			{navProjects, navSettings, navProfile},
		},
	}
}

func settingsKeyboard() channel.KeyboardMessage {
	return channel.KeyboardMessage{
		Text: "⚙️ Settings\n\nConfigure AI providers, models, and agent identity.",
		Rows: [][]channel.Button{
			{{Text: "📁 Projects", CallbackData: "ui:projects"}},
			{{Text: "🔌 Manage AI Providers", CallbackData: "ui:providers:0"}},
			{{Text: "🧩 MCP Servers", CallbackData: "ui:mcp"}},
			{{Text: "📚 Agent Skills", CallbackData: "ui:skills"}},
			{{Text: "🧠 Models (all)", CallbackData: "ui:models:0"}},
			{{Text: "🛡️ Permission mode", CallbackData: "ui:mode"}},
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
	e.deliverKeyboard(ctx, msg, mainMenuKeyboard(e.currentLine()))
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

// currentLine is the "where am I" header shown on the home screen.
func (e *Engine) currentLine() string {
	id := e.chats.ActiveID()
	if id == "" {
		return "📍 **Now:** no chat yet — tap New Chat or just send a message.\n\n"
	}
	ch, err := e.chats.Get(id)
	if err != nil {
		return ""
	}
	info := chatInfoOf(ch)
	where := "💬 General"
	if info.ProjectSlug != "" {
		where = "📁 " + code(info.ProjectSlug)
	}
	return "📍 **Now:** " + where + " › " + mdSafe(truncRunes(chatTitle(info, true), 40)) + "\n\n"
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
