//go:build !nogotd

package user

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/rs/zerolog/log"
)

// Config for a Telegrive-style user-account client.
type Config struct {
	APIID   int    // from https://my.telegram.org
	APIHash string // from https://my.telegram.org
	Phone   string // e.g. +2519...
	// SessionPath stores the MTProto session (like TDLib's db). Persist this.
	SessionPath string
	// CodeFile optional path; operator writes the SMS/Telegram code here.
	CodeFile string
	// Password optional 2FA cloud password.
	Password string
	// ChannelTitle for the private storage channel (default "Awaki").
	ChannelTitle string
}

// Client is a logged-in Telegram user session used only for durable storage.
// The agent chat interface can still use the Bot API; storage mirrors Telegrive.
type Client struct {
	cfg     Config
	tg      *tg.Client
	raw     *telegram.Client
	channel *tg.Channel // storage channel
}

// Run starts the MTProto client, authenticates if needed, ensures the Awaki
// storage channel exists, then invokes fn with a ready Client.
// The telegram connection lives for the duration of fn.
func Run(ctx context.Context, cfg Config, fn func(context.Context, *Client) error) error {
	if cfg.APIID == 0 || cfg.APIHash == "" {
		return fmt.Errorf("AWAKI_TG_API_ID and AWAKI_TG_API_HASH required (https://my.telegram.org)")
	}
	if cfg.Phone == "" {
		return fmt.Errorf("AWAKI_TG_PHONE required for user-account storage")
	}
	if cfg.SessionPath == "" {
		return fmt.Errorf("session path required")
	}
	if cfg.ChannelTitle == "" {
		cfg.ChannelTitle = "Awaki"
	}
	if err := os.MkdirAll(filepath.Dir(cfg.SessionPath), 0o755); err != nil {
		return err
	}

	storage := &session.FileStorage{Path: cfg.SessionPath}
	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: storage,
	})

	return client.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(
			&codeAuth{
				phone:    cfg.Phone,
				password: cfg.Password,
				codeFile: cfg.CodeFile,
			},
			auth.SendCodeOptions{},
		)
		if err := client.Auth().IfNecessary(ctx, flow); err != nil {
			return fmt.Errorf("telegram auth: %w", err)
		}

		api := client.API()
		self, err := api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
		if err != nil {
			log.Warn().Err(err).Msg("could not fetch self (continuing)")
		} else if len(self) > 0 {
			if u, ok := self[0].(*tg.User); ok {
				log.Info().Str("user", u.Username).Int64("id", u.ID).Msg("telegram user session ready")
			}
		}

		uc := &Client{cfg: cfg, tg: api, raw: client}
		ch, err := uc.ensureChannel(ctx)
		if err != nil {
			return fmt.Errorf("storage channel: %w", err)
		}
		uc.channel = ch
		log.Info().Int64("channel_id", ch.ID).Str("title", cfg.ChannelTitle).Msg("Awaki storage channel ready")
		return fn(ctx, uc)
	})
}

// ChannelID returns the storage channel id.
func (c *Client) ChannelID() int64 {
	if c.channel == nil {
		return 0
	}
	return c.channel.ID
}

func (c *Client) inputChannel() *tg.InputChannel {
	return &tg.InputChannel{
		ChannelID:  c.channel.ID,
		AccessHash: c.channel.AccessHash,
	}
}

func (c *Client) peer() *tg.InputPeerChannel {
	return &tg.InputPeerChannel{
		ChannelID:  c.channel.ID,
		AccessHash: c.channel.AccessHash,
	}
}

// ensureChannel finds an existing private channel titled ChannelTitle or creates one.
func (c *Client) ensureChannel(ctx context.Context) (*tg.Channel, error) {
	// Search dialogs for matching title
	dialogs, err := c.tg.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      100,
	})
	if err == nil {
		if ch := findChannelByTitle(dialogs, c.cfg.ChannelTitle); ch != nil {
			return ch, nil
		}
	}

	// Create private megagroup/channel (broadcast channel for storage, like Telegrive)
	u := uploader.NewUploader(c.tg)
	_ = u
	updates, err := c.tg.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
		Broadcast: true,
		Megagroup: false,
		Title:     c.cfg.ChannelTitle,
		About:     "Awaki durable agent storage — do not delete. Managed like Telegrive.",
	})
	if err != nil {
		return nil, fmt.Errorf("create channel: %w", err)
	}
	ch := channelFromUpdates(updates)
	if ch == nil {
		return nil, fmt.Errorf("create channel: no channel in updates")
	}

	// Pin is optional; try to set discussion / archive later if needed.
	return ch, nil
}

func findChannelByTitle(dialogs tg.MessagesDialogsClass, title string) *tg.Channel {
	var chats []tg.ChatClass
	switch d := dialogs.(type) {
	case *tg.MessagesDialogs:
		chats = d.Chats
	case *tg.MessagesDialogsSlice:
		chats = d.Chats
	default:
		return nil
	}
	for _, c := range chats {
		ch, ok := c.(*tg.Channel)
		if !ok {
			continue
		}
		if ch.Title == title {
			return ch
		}
	}
	return nil
}

func channelFromUpdates(u tg.UpdatesClass) *tg.Channel {
	switch v := u.(type) {
	case *tg.Updates:
		for _, ch := range v.Chats {
			if c, ok := ch.(*tg.Channel); ok {
				return c
			}
		}
	case *tg.UpdatesCombined:
		for _, ch := range v.Chats {
			if c, ok := ch.(*tg.Channel); ok {
				return c
			}
		}
	}
	return nil
}

// SendDocument uploads content as a document message with caption (Telegrive pattern).
func (c *Client) SendDocument(ctx context.Context, filename string, data []byte, caption string) (msgID int, err error) {
	up := uploader.NewUploader(c.tg)
	uploadCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	f, err := up.Upload(uploadCtx, uploader.NewUpload(filename, bytesNewReader(data), int64(len(data))))
	if err != nil {
		return 0, fmt.Errorf("upload: %w", err)
	}

	doc := &tg.InputMediaUploadedDocument{
		File:     f,
		MimeType: mimeFromName(filename),
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: filename},
		},
	}

	upd, err := c.tg.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:     c.peer(),
		Media:    doc,
		Message:  caption,
		RandomID: time.Now().UnixNano(),
	})
	if err != nil {
		return 0, err
	}
	return messageIDFromUpdates(upd), nil
}

// SendText sends a plain text message (for small records).
func (c *Client) SendText(ctx context.Context, text string) (int, error) {
	upd, err := c.tg.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     c.peer(),
		Message:  text,
		RandomID: time.Now().UnixNano(),
	})
	if err != nil {
		return 0, err
	}
	return messageIDFromUpdates(upd), nil
}

// DeleteMessage deletes a channel message by id.
func (c *Client) DeleteMessage(ctx context.Context, msgID int) error {
	_, err := c.tg.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
		Channel: c.inputChannel(),
		ID:      []int{msgID},
	})
	return err
}

// DownloadMessageDocument downloads the document attached to a channel message.
func (c *Client) DownloadMessageDocument(ctx context.Context, msgID int) ([]byte, error) {
	msgs, err := c.tg.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: c.inputChannel(),
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
	})
	if err != nil {
		return nil, err
	}
	msg := firstMessage(msgs)
	if msg == nil {
		return nil, fmt.Errorf("message %d not found", msgID)
	}
	doc, ok := documentFromMessage(msg)
	if !ok {
		// text-only: return message text after caption marker if any
		return []byte(msg.Message), nil
	}
	d := downloader.NewDownloader()
	var buf writerBuffer
	_, err = d.Download(c.tg, doc.AsInputDocumentFileLocation()).WithThreads(2).Stream(ctx, &buf)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// IterHistory walks recent channel messages (for rebuild / recovery).
func (c *Client) IterHistory(ctx context.Context, limit int, fn func(msgID int, caption string, hasDoc bool) error) error {
	if limit <= 0 {
		limit = 100
	}
	hist, err := c.tg.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:  c.peer(),
		Limit: limit,
	})
	if err != nil {
		return err
	}
	var messages []tg.MessageClass
	switch h := hist.(type) {
	case *tg.MessagesMessages:
		messages = h.Messages
	case *tg.MessagesMessagesSlice:
		messages = h.Messages
	case *tg.MessagesChannelMessages:
		messages = h.Messages
	}
	for _, m := range messages {
		msg, ok := m.(*tg.Message)
		if !ok {
			continue
		}
		hasDoc := false
		if _, ok := documentFromMessage(msg); ok {
			hasDoc = true
		}
		if err := fn(msg.ID, msg.Message, hasDoc); err != nil {
			return err
		}
	}
	return nil
}

func messageIDFromUpdates(u tg.UpdatesClass) int {
	switch v := u.(type) {
	case *tg.UpdateShortSentMessage:
		return v.ID
	case *tg.Updates:
		for _, up := range v.Updates {
			if nm, ok := up.(*tg.UpdateNewChannelMessage); ok {
				if m, ok := nm.Message.(*tg.Message); ok {
					return m.ID
				}
			}
			if nm, ok := up.(*tg.UpdateNewMessage); ok {
				if m, ok := nm.Message.(*tg.Message); ok {
					return m.ID
				}
			}
		}
	}
	return 0
}

func firstMessage(mc tg.MessagesMessagesClass) *tg.Message {
	var list []tg.MessageClass
	switch v := mc.(type) {
	case *tg.MessagesMessages:
		list = v.Messages
	case *tg.MessagesMessagesSlice:
		list = v.Messages
	case *tg.MessagesChannelMessages:
		list = v.Messages
	}
	for _, m := range list {
		if msg, ok := m.(*tg.Message); ok {
			return msg
		}
	}
	return nil
}

func documentFromMessage(msg *tg.Message) (*tg.Document, bool) {
	if msg.Media == nil {
		return nil, false
	}
	md, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok || md.Document == nil {
		return nil, false
	}
	doc, ok := md.Document.(*tg.Document)
	return doc, ok
}

func mimeFromName(name string) string {
	switch filepath.Ext(name) {
	case ".json":
		return "application/json"
	case ".md", ".txt":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}
