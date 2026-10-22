package telegram

import (
	"context"
	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/channel/telegram/client"
	"github.com/gemechischala/awaki/internal/transport"
	"sync"

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
		{Command: "new", Description: "Start a fresh conversation"},
		{Command: "providers", Description: "Manage AI providers"},
		{Command: "models", Description: "List and select models"},
		{Command: "history", Description: "View past conversations"},
		{Command: "memory", Description: "List memory files"},
		{Command: "soul", Description: "Show agent identity"},
		{Command: "user", Description: "Show user profile"},
		{Command: "stop", Description: "Cancel the current task"},
		{Command: "help", Description: "Show available commands"},
	})

	log.Info().Int64("owner_id", c.ownerID).Msg("telegram bot polling started")
	c.client.Start(ctx)
	return ctx.Err()
}
