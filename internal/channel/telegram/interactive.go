package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/gemechischala/awaki/internal/channel/telegram/rich"
	"github.com/gemechischala/awaki/internal/transport"

	"github.com/go-telegram/bot/models"
	"github.com/rs/zerolog/log"
)

const defaultPollGraceSec = 3

// registerAgentActions stores button values and returns callback_data ids (agent:act:N).
func (c *Channel) registerAgentActions(buttons []transport.AskOption) []map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.agentActions == nil {
		c.agentActions = make(map[string]string)
	}
	out := make([]map[string]string, 0, len(buttons))
	for _, b := range buttons {
		c.actionSeq++
		id := strconv.Itoa(c.actionSeq)
		val := b.Value
		if val == "" {
			val = b.Label
		}
		c.agentActions[id] = val
		label := b.Label
		if label == "" {
			label = val
		}
		out = append(out, map[string]string{
			"type":  "callback_data",
			"style": "primary",
			"data":  "agent:act:" + id,
			"label": label,
		})
	}
	return out
}

func (c *Channel) lookupAgentAction(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.agentActions[id]
	return v, ok
}

func (c *Channel) sendAgentButtons(ctx context.Context, chatID int64, prompt string, buttons []transport.AskOption) error {
	specs := c.registerAgentActions(buttons)
	var b strings.Builder
	if prompt != "" {
		b.WriteString("<p>" + rich.EscapeHTML(prompt) + "</p>")
	}
	// chunk into rows of max 4
	for i := 0; i < len(specs); i += 4 {
		end := i + 4
		if end > len(specs) {
			end = len(specs)
		}
		b.WriteString(rich.ButtonRow(specs[i:end], "center"))
	}
	_, err := c.client.SendRichMessage(ctx, chatID, "html", b.String(), false, false)
	return err
}

func (c *Channel) sendPollAsk(ctx context.Context, chatID int64, ev transport.AskEvent) error {
	labels := make([]string, 0, len(ev.Options))
	for _, o := range ev.Options {
		lbl := o.Label
		if lbl == "" {
			lbl = o.Value
		}
		labels = append(labels, lbl)
	}
	if len(labels) < 2 {
		return fmt.Errorf("poll needs at least 2 options")
	}
	grace := ev.PollGraceSec
	if grace <= 0 {
		grace = defaultPollGraceSec
	}
	msgID, err := c.client.SendPoll(ctx, chatID, ev.Prompt, labels, ev.PollAllowsMultiple, ev.PollIsAnonymous)
	if err != nil {
		return err
	}
	_ = msgID

	// We need the poll_id from the sent message. SendPoll returns message id;
	// Telegram includes Poll in the Message. Re-fetch isn't available easily —
	// register by waiting for PollAnswer and matching options. Store a pending
	// session keyed temporarily by chat until first answer carries poll_id.
	c.mu.Lock()
	if c.pollSessions == nil {
		c.pollSessions = make(map[string]*pollSession)
	}
	// Use pending key until we learn poll_id from the first answer.
	pendingKey := fmt.Sprintf("pending:%d:%d", chatID, msgID)
	sess := &pollSession{
		options:     labels,
		replyCh:     ev.ReplyCh,
		allowsMulti: ev.PollAllowsMultiple,
		graceSec:    grace,
	}
	c.pollSessions[pendingKey] = sess
	c.mu.Unlock()
	log.Info().Int("msg_id", msgID).Bool("multi", ev.PollAllowsMultiple).Int("grace", grace).Msg("telegram: poll ask sent")
	return nil
}

func (c *Channel) handlePollAnswer(ctx context.Context, ans *models.PollAnswer) {
	if ans == nil {
		return
	}
	// Owner-only when user present
	if ans.User != nil && ans.User.ID != c.ownerID {
		return
	}

	c.mu.Lock()
	sess := c.pollSessions[ans.PollID]
	if sess == nil {
		// Bind first answer to a pending session for this owner.
		for k, s := range c.pollSessions {
			if strings.HasPrefix(k, "pending:") {
				delete(c.pollSessions, k)
				c.pollSessions[ans.PollID] = s
				sess = s
				break
			}
		}
	}
	c.mu.Unlock()
	if sess == nil {
		log.Debug().Str("poll_id", ans.PollID).Msg("telegram: poll answer for unknown poll")
		return
	}

	sess.mu.Lock()
	sess.optionIDs = append([]int(nil), ans.OptionIDs...)
	if sess.timer != nil {
		sess.timer.Stop()
	}
	grace := time.Duration(sess.graceSec) * time.Second
	if !sess.allowsMulti {
		grace = 0
	}
	finish := func() {
		sess.mu.Lock()
		ids := append([]int(nil), sess.optionIDs...)
		opts := sess.options
		ch := sess.replyCh
		sess.mu.Unlock()

		c.mu.Lock()
		delete(c.pollSessions, ans.PollID)
		c.mu.Unlock()

		labels := make([]string, 0, len(ids))
		for _, id := range ids {
			if id >= 0 && id < len(opts) {
				labels = append(labels, opts[id])
			}
		}
		resp := strings.Join(labels, ", ")
		if resp == "" {
			resp = "(no option selected)"
		}
		if ch != nil {
			select {
			case ch <- resp:
			default:
				log.Warn().Msg("telegram: poll reply channel full/closed")
			}
		}
	}
	if grace <= 0 {
		sess.mu.Unlock()
		finish()
		return
	}
	sess.timer = time.AfterFunc(grace, finish)
	sess.mu.Unlock()
}

func (c *Channel) dispatchAgentAction(ctx context.Context, text string) {
	if text == "" {
		return
	}
	c.handler(ctx, channel.Message{
		Text:  text,
		Reply: c.newReplier(c.ownerID),
	})
}
