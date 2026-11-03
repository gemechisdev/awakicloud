package channel

import "strings"

// CommandSpec is the single source of truth for Telegram slash commands:
// routing (commandMap), SetMyCommands menu, and /help text.
type CommandSpec struct {
	// Slash is the Telegram command without leading slash, e.g. "start".
	Slash string
	// Cmd is the engine constant (CmdStart, …). Empty = handled only in channel layer.
	Cmd string
	// Description is the short Telegram menu description (max ~40 chars ideal).
	Description string
	// Help is the /help line body after "/slash ". Empty = omit from /help body details.
	Help string
	// Menu controls inclusion in SetMyCommands (Telegram bot menu).
	Menu bool
	// HideHelp omits this entry from the /help listing (still routable).
	HideHelp bool
}

// Commands is the ordered catalog. Add new bot commands here only.
var Commands = []CommandSpec{
	{Slash: "start", Cmd: CmdStart, Description: "Main menu", Help: "main menu", Menu: true},
	{Slash: "new", Cmd: CmdNewChat, Description: "New chat — pick a project or none", Help: "[project|-] — new chat (asks which project unless given)", Menu: true},
	{Slash: "chats", Cmd: CmdChatsByProject, Description: "Browse chats (all or by project)", Help: "[project|search words] — all chats with project badges, or filter", Menu: true},
	{Slash: "chat", Cmd: CmdChat, Description: "Current chat & actions", Help: "current chat card (rename, move, new chat in same project)", Menu: true},
	{Slash: "move", Cmd: CmdMove, Description: "Move this chat to a project", Help: "[project|-] — move current chat into a project (or out with -)", Menu: true},
	{Slash: "rename", Cmd: CmdRename, Description: "Rename this chat", Help: "<title> — rename current chat", Menu: true},
	{Slash: "chat", Cmd: CmdChat, Description: "Current chat & actions", Help: "current chat card (rename, move, new chat in same project)", Menu: true},
	{Slash: "move", Cmd: CmdMove, Description: "Move this chat to a project", Help: "[project|-] — move current chat into a project (or out with -)", Menu: true},
	{Slash: "rename", Cmd: CmdRename, Description: "Rename this chat", Help: "<title> — rename current chat", Menu: true},
	{Slash: "stop", Cmd: CmdStop, Description: "Cancel current task", Help: "cancel current agent run", Menu: true},
	{Slash: "help", Cmd: CmdHelp, Description: "List all commands", Help: "this list", Menu: true},

	{Slash: "mode", Cmd: CmdMode, Description: "Permission mode auto/ask/plan", Help: "[auto|ask|plan] — sticky permission mode (buttons if bare)", Menu: true},
	{Slash: "plan", Cmd: CmdPlan, Description: "One-shot read-only plan turn", Help: "<task> — one-shot read-only plan turn", Menu: true},

	{Slash: "tasks", Cmd: CmdTasks, Description: "Background task board", Help: "background task board", Menu: true},
	{Slash: "task", Cmd: CmdTaskAdd, Description: "Queue a background task", Help: "<work> — queue background task", Menu: true},

	{Slash: "history", Cmd: CmdHistory, Description: "Past conversations", Help: "alias of /chats", Menu: false, HideHelp: true},
	{Slash: "memory", Cmd: CmdMemory, Description: "List memory files", Help: "list memory files", Menu: true},
	{Slash: "soul", Cmd: CmdSoul, Description: "Show agent identity", Help: "agent identity (soul.md)", Menu: true},
	{Slash: "user", Cmd: CmdUser, Description: "Show user profile", Help: "user profile (user.md)", Menu: true},

	{Slash: "providers", Cmd: CmdProviders, Description: "Manage AI providers", Help: "manage LLM providers", Menu: true},
	{Slash: "models", Cmd: CmdModels, Description: "List and select models", Help: "list / select models", Menu: true},
	{Slash: "provider_add", Cmd: CmdProviderAdd, Description: "Quick-add provider", Help: "name|protocol|base_url|api_key", Menu: true},
	{Slash: "model_add", Cmd: CmdModelAdd, Description: "Quick-add model", Help: "provider|model_id|Display Name", Menu: true},

	{Slash: "projects", Cmd: CmdProjects, Description: "Projects and their chats", Help: "browse projects → their chats → new chat in project", Menu: true},
	{Slash: "project", Cmd: CmdProject, Description: "Open or create a project", Help: "[name] — open/create a project and use it for the current chat", Menu: true},
	{Slash: "upgrade", Cmd: CmdProjectUpgrade, Description: "Upgrade chat to project", Help: "[name] — upgrade chat → project", Menu: true},
	{Slash: "sync", Cmd: CmdProjectSync, Description: "Sync project to Telegram", Help: "sync active project to Telegram storage", Menu: true},
	{Slash: "chatlink", Cmd: CmdChatLink, Description: "Link chat to project", Help: "<project> — alias of /move", Menu: false, HideHelp: true},

	{Slash: "mcp", Cmd: CmdMCP, Description: "List MCP servers", Help: "list MCP servers", Menu: true},
	{Slash: "mcp_add", Cmd: CmdMCPAdd, Description: "Add MCP server", Help: "add MCP server (wizard)", Menu: true},
	{Slash: "skills", Cmd: CmdSkills, Description: "List Agent Skills", Help: "list agent skills", Menu: true},
	{Slash: "skill_add", Cmd: CmdSkillAdd, Description: "Add a skill", Help: "add a skill", Menu: true},
	{Slash: "plugin", Cmd: CmdPlugin, Description: "Skills marketplace", Help: "skills marketplace", Menu: true},
}

// CommandMap builds "/slash" → engine Cmd name for routing.
func CommandMap() map[string]string {
	m := make(map[string]string, len(Commands))
	for _, c := range Commands {
		if c.Cmd == "" {
			continue
		}
		m["/"+c.Slash] = c.Cmd
	}
	return m
}

// MenuCommands returns Telegram BotCommand pairs (command, description).
func MenuCommands() [][2]string {
	out := make([][2]string, 0, len(Commands))
	for _, c := range Commands {
		if !c.Menu {
			continue
		}
		desc := c.Description
		if desc == "" {
			desc = c.Help
		}
		if len(desc) > 48 {
			desc = desc[:45] + "…"
		}
		out = append(out, [2]string{c.Slash, desc})
	}
	return out
}

// HelpText builds the full /help message from the catalog.
func HelpText() string {
	var b strings.Builder
	b.WriteString("Commands:\n\n")
	for _, c := range Commands {
		if c.HideHelp {
			continue
		}
		help := c.Help
		if help == "" {
			help = c.Description
		}
		b.WriteString("/")
		b.WriteString(c.Slash)
		if help != "" {
			b.WriteString(" — ")
			b.WriteString(help)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nPermission: /mode auto | ask | plan\n")
	b.WriteString("One-shot plan: /plan <task>\n")
	return strings.TrimSpace(b.String())
}
