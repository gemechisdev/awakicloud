package channel

import (
	"context"
	"github.com/gemechischala/awaki/internal/transport"
)

const (
	CmdStart          = "start"
	CmdNewChat        = "new_chat"
	CmdStop           = "stop"
	CmdHistory        = "history"
	CmdMemory         = "memory"
	CmdSoul           = "soul"
	CmdUser           = "user"
	CmdProviders      = "providers"
	CmdModels         = "models"
	CmdProviderAdd    = "provider_add"
	CmdModelAdd       = "model_add"
	CmdProjects       = "projects"
	CmdProject        = "project"
	CmdProjectUpgrade = "project_upgrade"
	CmdProjectSync    = "project_sync"
	CmdChatLink       = "chatlink"
	CmdChatsByProject = "chats"
	CmdChat           = "chat"
	CmdMove           = "move"
	CmdRename         = "rename"
	CmdPlan           = "plan"
	CmdHelp           = "help"
	CmdHealth         = "health"
	CmdTasks          = "tasks"
	CmdTaskAdd        = "task"
	CmdSchedules      = "schedules"
	CmdSSH            = "ssh"
	CmdMCP            = "mcp"
	CmdMCPAdd         = "mcp_add"
	CmdSkills         = "skills"
	CmdSkillAdd       = "skill_add"
	CmdPlugin         = "plugin"
	CmdMode           = "mode"
	CmdExport         = "export"
	CmdImport         = "import"
)

type Command struct {
	Name   string
	Args   map[string]string
	Result *CommandResult
}

type CommandResult struct {
	// Handled is set by the engine when it already delivered its own UI
	// (inline keyboards etc.); channels must not send "success"/"empty" fallbacks.
	Handled bool
	Data    map[string]string
	Payload any // typed data for channel-specific formatting
	Error   error
}

type Message struct {
	Text          string
	Attachments   []transport.Attachment
	Command       *Command
	Reply         transport.Replier
	EditMessageID int // if set, UI keyboards should edit this Telegram message

	// QuotedText is the text the user replied-to or quoted in this message,
	// rendered as a block. Populated by the channel layer from Telegram's
	// reply_to_message / external_reply / quote fields. Empty when the
	// incoming message has no reply context.
	QuotedText string
}

type Handler func(ctx context.Context, msg Message)

// Channel is the interface every communication channel must implement.
type Channel interface {
	Name() string
	OwnerReplier() transport.Replier
	Run(ctx context.Context, handler Handler) error
}
