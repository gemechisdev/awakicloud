package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// richMessageRequest mirrors the Telegram Bot API sendRichMessage / editMessageText
// rich_message parameter.
type richMessageRequest struct {
	ChatID              int64       `json:"chat_id"`
	RichMessage         richMessage `json:"rich_message"`
	DisableNotification bool        `json:"disable_notification,omitempty"`
	MessageID           int         `json:"message_id,omitempty"`
}

// richMessage mirrors InputRichMessage. Exactly one of HTML/Markdown/Blocks must be set.
type richMessage struct {
	HTML     string `json:"html,omitempty"`
	Markdown string `json:"markdown,omitempty"`
	Blocks   any    `json:"blocks,omitempty"`
	Media    any    `json:"media,omitempty"`
	IsRTL    bool   `json:"is_rtl,omitempty"`
}

// draftRequest mirrors sendRichMessageDraft parameters.
type draftRequest struct {
	ChatID      int64       `json:"chat_id"`
	DraftID     int         `json:"draft_id"`
	RichMessage richMessage `json:"rich_message"`
	CanStop     bool        `json:"can_stop,omitempty"`
	KeepOnStop  bool        `json:"keep_on_stop,omitempty"`
}

// SendRichMessage sends a persistent Telegram Rich Message via raw HTTP (Bot API 10.1+).
// contentType must be "html", "markdown", or "blocks".
func (c *Client) SendRichMessage(ctx context.Context, chatID int64, contentType, content string, isRTL bool, disableNotification bool) (int, error) {
	rm := richMessage{IsRTL: isRTL}
	switch contentType {
	case "html":
		rm.HTML = content
	case "markdown":
		rm.Markdown = content
	case "blocks":
		rm.Blocks = content
	default:
		return 0, fmt.Errorf("invalid contentType %q: must be html, markdown, or blocks", contentType)
	}

	payload := richMessageRequest{
		ChatID:              chatID,
		RichMessage:         rm,
		DisableNotification: disableNotification,
	}
	return c.postRich(ctx, "sendRichMessage", payload)
}

// SendRichMessageDraft streams an ephemeral rich message draft (30s lifetime).
// draftID must be non-zero and stable across incremental updates.
func (c *Client) SendRichMessageDraft(ctx context.Context, chatID int64, draftID int, contentType, content string, canStop bool, keepOnStop bool) error {
	rm := richMessage{}
	switch contentType {
	case "html":
		rm.HTML = content
	case "markdown":
		rm.Markdown = content
	case "blocks":
		rm.Blocks = content
	default:
		return fmt.Errorf("invalid contentType %q: must be html, markdown, or blocks", contentType)
	}

	payload := draftRequest{
		ChatID:      chatID,
		DraftID:     draftID,
		RichMessage: rm,
		CanStop:     canStop,
		KeepOnStop:  keepOnStop,
	}
	_, err := c.postRich(ctx, "sendRichMessageDraft", payload)
	return err
}

// EditRichMessage updates an existing message to a Rich Message.
func (c *Client) EditRichMessage(ctx context.Context, chatID int64, messageID int, contentType, content string, isRTL bool) error {
	rm := richMessage{IsRTL: isRTL}
	switch contentType {
	case "html":
		rm.HTML = content
	case "markdown":
		rm.Markdown = content
	case "blocks":
		rm.Blocks = content
	default:
		return fmt.Errorf("invalid contentType %q: must be html, markdown, or blocks", contentType)
	}

	payload := richMessageRequest{
		ChatID:      chatID,
		MessageID:   messageID,
		RichMessage: rm,
	}
	_, err := c.postRich(ctx, "editMessageText", payload)
	return err
}

// postRich POSTs a JSON payload to the Telegram Bot API rich message endpoint.
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

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("rich message API HTTP %d", resp.StatusCode)
	}

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode rich message response: %w", err)
	}
	if !result.OK {
		return 0, fmt.Errorf("rich message API error %d: %s", result.ErrorCode, result.Description)
	}
	return result.Result.MessageID, nil
}

// ensureCompileTimeInterface checks that Client satisfies the rich message interface.
// It is a compile-time assertion only.
var _ interface {
	SendRichMessage(context.Context, int64, string, string, bool, bool) (int, error)
	SendRichMessageDraft(context.Context, int64, int, string, string, bool, bool) error
	EditRichMessage(context.Context, int64, int, string, string, bool) error
} = (*Client)(nil)

// telegramRichAPIError is returned when Telegram rejects a rich message payload.
type telegramRichAPIError struct {
	Code        int
	Description string
	Parameters  map[string]any
}

func (e *telegramRichAPIError) Error() string {
	if e.Parameters != nil {
		if retryAfter, ok := e.Parameters["retry_after"]; ok {
			return fmt.Sprintf("telegram rich API error %d: %s (retry_after=%v)", e.Code, e.Description, retryAfter)
		}
	}
	return fmt.Sprintf("telegram rich API error %d: %s", e.Code, e.Description)
}
