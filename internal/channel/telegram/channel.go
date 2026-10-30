package telegram

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/channel/telegram/client"
	"github.com/gemechischala/awaki/internal/transport"

	"github.com/go-telegram/bot/models"
	"github.com/rs/zerolog/log"
)

type Channel struct {
	client  *client.Client
	ownerID int64

	handler channel.Handler

	mu          sync.Mutex
	mediaGroups map[string]*mediaGroupBuffer
}

func New(token string, ownerID int64) (*Channel, error) {
	ch := &Channel{
		ownerID:     ownerID,
		mediaGroups: make(map[string]*mediaGroupBuffer),
	}

	cl, err := client.New(token, ch.defaultHandler)
	if err != nil {
		return nil, err
	}
	ch.client = cl

	return ch, nil
}

func (c *Channel) Name() string   { return "telegram" }
func (c *Channel) OwnerID() int64 { return c.ownerID }

func (c *Channel) OwnerReplier() transport.Replier {
	return c.newReplier(c.ownerID)
}

func (c *Channel) newReplier(chatID int64) *replier {
	return &replier{ch: c, chatID: chatID}
}

func (c *Channel) Run(ctx context.Context, handler channel.Handler) error {
	c.handler = handler

	c.client.SetMyCommands(ctx, []models.BotCommand{
		{Command: "start", Description: "Main menu"},
		{Command: "new", Description: "New conversation"},
		{Command: "stop", Description: "Cancel current task"},
		{Command: "help", Description: "List all commands"},
		{Command: "plan", Description: "Read-only plan mode for one turn"},
		{Command: "tasks", Description: "Background task board"},
		{Command: "task", Description: "Queue a background task"},
		{Command: "history", Description: "Past conversations"},
		{Command: "memory", Description: "List memory files"},
		{Command: "soul", Description: "Show agent identity"},
		{Command: "user", Description: "Show user profile"},
		{Command: "providers", Description: "Manage AI providers"},
		{Command: "models", Description: "List and select models"},
		{Command: "provider_add", Description: "Quick-add provider"},
		{Command: "model_add", Description: "Quick-add model"},
		{Command: "projects", Description: "List projects"},
		{Command: "project", Description: "Create a project"},
		{Command: "upgrade", Description: "Upgrade chat to project"},
		{Command: "sync", Description: "Sync project to Telegram"},
		{Command: "mcp", Description: "List MCP servers"},
		{Command: "mcp_add", Description: "Add MCP server"},
	})

	// Grace period after process/health is up so the previous instance can
	// release getUpdates (Koyeb/rolling deploys otherwise fight over the token).
	grace := pollGracePeriod()
	if grace > 0 {
		log.Info().
			Dur("grace", grace).
			Int64("owner_id", c.ownerID).
			Msg("telegram: waiting before polling so prior instance can release getUpdates")
		timer := time.NewTimer(grace)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}

	// Ensure long-polling mode; drop queued updates from the old instance.
	if err := c.client.DeleteWebhook(ctx, true); err != nil {
		log.Warn().Err(err).Msg("telegram: DeleteWebhook failed (continuing)")
	}

	log.Info().Int64("owner_id", c.ownerID).Msg("telegram bot polling started")
	c.client.Start(ctx)
	return ctx.Err()
}

// pollGracePeriod returns how long to wait before getUpdates after startup.
// Override with AWAKI_POLL_GRACE_SEC (seconds). Default 15. Set 0 to disable.
func pollGracePeriod() time.Duration {
	const defaultSec = 15
	v := os.Getenv("AWAKI_POLL_GRACE_SEC")
	if v == "" {
		return defaultSec * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return defaultSec * time.Second
	}
	return time.Duration(n) * time.Second
}
