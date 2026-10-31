package client

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/rs/zerolog/log"
)

// pollTimeout is the long-poll window for getUpdates. Keep this short so a
// terminating instance releases exclusive getUpdates access within a few
// seconds of SIGTERM (the library default is 60s, which causes deploy races).
const pollTimeout = 10 * time.Second

type UpdateHandler func(ctx context.Context, update *models.Update)

type Client struct {
	b *bot.Bot
}

func New(token string, handler UpdateHandler) (*Client, error) {
	httpClient := &http.Client{Timeout: pollTimeout}
	b, err := bot.New(token,
		bot.WithHTTPClient(pollTimeout, httpClient),
		bot.WithDefaultHandler(func(ctx context.Context, _ *bot.Bot, update *models.Update) {
			handler(ctx, update)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("telegram bot init: %w", err)
	}
	log.Info().Dur("poll_timeout", pollTimeout).Msg("telegram bot authorized")
	return &Client{b: b}, nil
}

func (c *Client) Start(ctx context.Context) {
	c.b.Start(ctx)
}

// DeleteWebhook removes a webhook if set and optionally drops pending updates
// so long-polling getUpdates can take over after a deploy.
func (c *Client) DeleteWebhook(ctx context.Context, dropPending bool) error {
	_, err := c.b.DeleteWebhook(ctx, &bot.DeleteWebhookParams{
		DropPendingUpdates: dropPending,
	})
	return err
}

func (c *Client) SetMyCommands(ctx context.Context, commands []models.BotCommand) {
	c.b.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: commands})
}

// EscapeMarkdown escapes special characters for Telegram MarkdownV2.
func EscapeMarkdown(s string) string {
	return bot.EscapeMarkdown(s)
}
