package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gemechischala/awaki/internal/channel/telegram/rich"
)

// richMessageRequest mirrors sendRichMessage / editMessageText rich_message.
type richMessageRequest struct {
	ChatID              int64       `json:"chat_id"`
	MessageThreadID     int64       `json:"message_thread_id,omitempty"`
	RichMessage         richMessage `json:"rich_message"`
	DisableNotification bool        `json:"disable_notification,omitempty"`
	MessageID           int         `json:"message_id,omitempty"`
	ProtectContent      bool        `json:"protect_content,omitempty"`
}

// richMessage mirrors InputRichMessage. Exactly one of HTML/Markdown/Blocks must be set.
type richMessage struct {
	HTML                string `json:"html,omitempty"`
	Markdown            string `json:"markdown,omitempty"`
	Blocks              any    `json:"blocks,omitempty"`
	Media               any    `json:"media,omitempty"`
	IsRTL               bool   `json:"is_rtl,omitempty"`
	SkipEntityDetection bool   `json:"skip_entity_detection,omitempty"`
}

// draftRequest mirrors sendRichMessageDraft parameters.
type draftRequest struct {
	ChatID          int64       `json:"chat_id"`
	DraftID         int         `json:"draft_id"`
	MessageThreadID int64       `json:"message_thread_id,omitempty"`
	RichMessage     richMessage `json:"rich_message"`
	CanStop         bool        `json:"can_stop,omitempty"`
	KeepOnStop      bool        `json:"keep_on_stop,omitempty"`
}

// SendRichMessage sends a persistent Telegram Rich Message (Bot API 10.1+).
// contentType must be "html", "markdown", or "blocks".
func (c *Client) SendRichMessage(ctx context.Context, chatID int64, contentType, content string, isRTL bool, disableNotification bool) (int, error) {
	return c.SendRichMessageOpts(ctx, chatID, contentType, content, isRTL, disableNotification, 0)
}

// SendRichMessageOpts is the full form with optional message_thread_id.
func (c *Client) SendRichMessageOpts(ctx context.Context, chatID int64, contentType, content string, isRTL bool, disableNotification bool, messageThreadID int64) (int, error) {
	rm, err := buildRichMessage(contentType, content, isRTL)
	if err != nil {
		return 0, err
	}
	if err := rich.ValidateRichMessage(rm.HTML, rm.Markdown, rm.Blocks); err != nil {
		return 0, err
	}

	payload := richMessageRequest{
		ChatID:              chatID,
		MessageThreadID:     messageThreadID,
		RichMessage:         rm,
		DisableNotification: disableNotification,
	}
	return c.postRich(ctx, "sendRichMessage", payload)
}

// SendRichMessageDraft streams an ephemeral rich message draft (30s lifetime).
// draftID must be non-zero and stable across incremental updates.
func (c *Client) SendRichMessageDraft(ctx context.Context, chatID int64, draftID int, contentType, content string, canStop bool, keepOnStop bool) error {
	return c.SendRichMessageDraftOpts(ctx, chatID, draftID, contentType, content, canStop, keepOnStop, 0)
}

// SendRichMessageDraftOpts includes optional message_thread_id.
func (c *Client) SendRichMessageDraftOpts(ctx context.Context, chatID int64, draftID int, contentType, content string, canStop bool, keepOnStop bool, messageThreadID int64) error {
	if draftID == 0 {
		return fmt.Errorf("draft_id must be non-zero")
	}
	rm, err := buildRichMessage(contentType, content, false)
	if err != nil {
		return err
	}
	if rich.CharLen(content) > rich.MaxCharacters {
		return fmt.Errorf("draft content exceeds hard limit of %d characters", rich.MaxCharacters)
	}

	payload := draftRequest{
		ChatID:          chatID,
		DraftID:         draftID,
		MessageThreadID: messageThreadID,
		RichMessage:     rm,
		CanStop:         canStop,
		KeepOnStop:      keepOnStop,
	}
	_, err = c.postRich(ctx, "sendRichMessageDraft", payload)
	return err
}

// EditRichMessage updates an existing message to a Rich Message via editMessageText.
func (c *Client) EditRichMessage(ctx context.Context, chatID int64, messageID int, contentType, content string, isRTL bool) error {
	rm, err := buildRichMessage(contentType, content, isRTL)
	if err != nil {
		return err
	}
	if err := rich.ValidateRichMessage(rm.HTML, rm.Markdown, rm.Blocks); err != nil {
		return err
	}

	payload := richMessageRequest{
		ChatID:      chatID,
		MessageID:   messageID,
		RichMessage: rm,
	}
	_, err = c.postRich(ctx, "editMessageText", payload)
	return err
}

func buildRichMessage(contentType, content string, isRTL bool) (richMessage, error) {
	rm := richMessage{IsRTL: isRTL}
	switch contentType {
	case "html":
		rm.HTML = rich.EnsureSafeLength(content)
	case "markdown":
		rm.Markdown = content
		if rich.CharLen(content) > rich.MaxCharacters {
			return rm, fmt.Errorf("markdown content exceeds hard limit of %d characters", rich.MaxCharacters)
		}
	case "blocks":
		if content == "" {
			return rm, fmt.Errorf("blocks content is empty")
		}
		rm.Blocks = json.RawMessage(content)
	default:
		return rm, fmt.Errorf("invalid contentType %q: must be html, markdown, or blocks", contentType)
	}
	return rm, nil
}

// postRich POSTs a JSON payload to the Telegram Bot API.
func (c *Client) postRich(ctx context.Context, method string, payload any) (int, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/%s", c.token, method)

	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal rich message payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("create rich message request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send rich message request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var result struct {
		OK          bool   `json:"ok"`
		Result      any    `json:"result"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("rich message API HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
		}
		return 0, fmt.Errorf("decode rich message response: %w", err)
	}
	if !result.OK {
		return 0, fmt.Errorf("rich message API error %d: %s", result.ErrorCode, result.Description)
	}

	msgID := 0
	if m, ok := result.Result.(map[string]any); ok {
		switch v := m["message_id"].(type) {
		case float64:
			msgID = int(v)
		case int:
			msgID = v
		}
	}
	return msgID, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var _ interface {
	SendRichMessage(context.Context, int64, string, string, bool, bool) (int, error)
	SendRichMessageDraft(context.Context, int64, int, string, string, bool, bool) error
	EditRichMessage(context.Context, int64, int, string, string, bool) error
} = (*Client)(nil)

var _ = strings.Contains
