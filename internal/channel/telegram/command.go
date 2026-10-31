package telegram

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/channel/telegram/client"
	"github.com/gemechischala/awaki/internal/store"
	"github.com/gemechischala/awaki/internal/transport"
)

// commandMap maps Telegram slash commands to engine command names.
// Keep in sync with SetMyCommands in channel.go and /help text.
var commandMap = map[string]string{
	"/start":        channel.CmdStart,
	"/new":          channel.CmdNewChat,
	"/stop":         channel.CmdStop,
	"/help":         channel.CmdHelp,
	"/plan":         channel.CmdPlan,
	"/tasks":        channel.CmdTasks,
	"/task":         channel.CmdTaskAdd,
	"/history":      channel.CmdHistory,
	"/memory":       channel.CmdMemory,
	"/soul":         channel.CmdSoul,
	"/user":         channel.CmdUser,
	"/providers":    channel.CmdProviders,
	"/models":       channel.CmdModels,
	"/provider_add": channel.CmdProviderAdd,
	"/model_add":    channel.CmdModelAdd,
	"/projects":     channel.CmdProjects,
	"/project":      channel.CmdProject,
	"/upgrade":      channel.CmdProjectUpgrade,
	"/sync":         channel.CmdProjectSync,
	"/mcp":          channel.CmdMCP,
	"/mcp_add":      channel.CmdMCPAdd,
	"/skills":       channel.CmdSkills,
	"/skill_add":    channel.CmdSkillAdd,
	"/plugin":       channel.CmdPlugin,
}

var commandSuccess = map[string]string{
	channel.CmdNewChat: "✨ New conversation started.",
}

var commandEmpty = map[string]string{
	channel.CmdHistory: "No conversation history yet.",
	channel.CmdMemory:  "Memory is empty — no files yet.",
	channel.CmdSoul:    "Soul not configured yet — no soul.md.",
	channel.CmdUser:    "User profile not configured yet — no user.md.",
}

func helpText() string {
	return strings.TrimSpace(`
Commands:

/start — main menu
/new — new conversation
/stop — cancel current agent run
/help — this list
/plan <task> — read-only plan mode for one turn
/tasks — background task board
/task <work> — queue background task manually

/history — past conversations
/memory — list memory files
/soul — agent identity (soul.md)
/user — user profile (user.md)

/providers — manage LLM providers
/models — list / select models
/provider_add name|protocol|base_url|api_key
/model_add provider|model_id|Display Name

/projects — list projects
/project <name> — create project
/upgrade [name] — upgrade chat → project
/sync — sync active project to Telegram storage

/mcp — list MCP servers
/mcp_add — add MCP server (wizard)
/skills — list Agent Skills
/skills add <url> [--skill name] — install a skill from GitHub / skills.sh / smithery.ai / any URL
/skill_add — add a skill
/plugin marketplace add owner/repo — register skills marketplace
/plugin install name@market — install plugin skills
`)
}

func (c *Channel) handleCommand(ctx context.Context, chatID int64, reply transport.Replier, cmdText string) {
	cmdText = strings.TrimSpace(cmdText)
	fields := strings.Fields(cmdText)
	cmdName := cmdText
	rawArgs := ""
	if len(fields) > 0 {
		cmdName = fields[0]
		// Telegram may send /plan@BotName
		if i := strings.Index(cmdName, "@"); i > 0 {
			cmdName = cmdName[:i]
		}
		if len(fields) > 1 {
			rawArgs = strings.TrimSpace(strings.TrimPrefix(cmdText, fields[0]))
		}
	}

	name, ok := commandMap[cmdName]
	if !ok {
		c.send(ctx, chatID, "Unknown command\\. Try /help")
		return
	}

	cmd := &channel.Command{Name: name, Args: map[string]string{"raw": rawArgs}, Result: &channel.CommandResult{}}
	c.handler(ctx, channel.Message{Text: cmdText, Reply: reply, Command: cmd})

	if cmd.Result.Error != nil {
		c.send(ctx, chatID, "Error: "+client.EscapeMarkdown(cmd.Result.Error.Error()))
		return
	}

	if km, ok := cmd.Result.Payload.(channel.KeyboardMessage); ok {
		c.sendKeyboard(ctx, chatID, km)
		return
	}
	if text := formatPayload(cmd.Result.Payload); text != "" {
		c.sendLong(ctx, chatID, text)
		return
	}

	if text := cmd.Result.Data["text"]; text != "" {
		c.sendLong(ctx, chatID, text)
		return
	}

	if msg, ok := commandSuccess[name]; ok {
		c.sendLong(ctx, chatID, msg)
		return
	}

	if msg, ok := commandEmpty[name]; ok {
		c.sendLong(ctx, chatID, msg)
	}
}

func formatPayload(payload any) string {
	switch v := payload.(type) {
	case []store.ChatInfo:
		return FormatHistory(v)
	case []store.MemoryFile:
		return FormatMemory(v)
	default:
		return ""
	}
}

func FormatHistory(chats []store.ChatInfo) string {
	var closed []store.ChatInfo
	for _, ch := range chats {
		if ch.Status == "closed" {
			closed = append(closed, ch)
		}
	}
	if len(closed) == 0 {
		return ""
	}

	const maxShown = 15
	if len(closed) > maxShown {
		closed = closed[:maxShown]
	}

	var sb strings.Builder
	sb.WriteString("📜 **Conversation history:**\n\n")
	for _, ch := range closed {
		title := ch.Title
		if title == "" {
			title = "Untitled"
		}
		if len([]rune(title)) > 50 {
			title = string([]rune(title)[:50]) + "…"
		}
		date := ch.StartedAt.Format("02 Jan")
		if !ch.EndedAt.IsZero() && ch.EndedAt.Format("02 Jan") != date {
			date += " — " + ch.EndedAt.Format("02 Jan")
		}
		fmt.Fprintf(&sb, "**%s** (%s)\n", title, date)
		if ch.Summary != "" {
			summary := ch.Summary
			if len([]rune(summary)) > 120 {
				summary = string([]rune(summary)[:120]) + "…"
			}
			fmt.Fprintf(&sb, "*%s*\n", summary)
		}
		if len(ch.Tags) > 0 {
			fmt.Fprintf(&sb, "`%s`\n", strings.Join(ch.Tags, ", "))
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func FormatMemory(files []store.MemoryFile) string {
	if len(files) == 0 {
		return ""
	}

	var sb strings.Builder
	for i, f := range files {
		if i > 0 {
			sb.WriteString("\n---\n\n")
		}
		fmt.Fprintf(&sb, "📂 **%s**\n\n%s\n", f.Name, strings.TrimSpace(f.Content))
	}
	return sb.String()
}
