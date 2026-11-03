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
	CmdTasks          = "tasks"
	CmdTaskAdd        = "task"
	CmdMCP            = "mcp"
	CmdMCPAdd         = "mcp_add"
	CmdSkills         = "skills"
	CmdSkillAdd       = "skill_add"
	CmdPlugin         = "plugin"
	CmdMode           = "mode"
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
}

type Handler func(ctx context.Context, msg Message)

// Channel is the interface every communication channel must implement.
type Channel interface {
	Name() string
	OwnerReplier() transport.Replier
	Run(ctx context.Context, handler Handler) error
}
