package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gemechischala/awaki/internal/channel"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// steerOffer is a mid-run user message awaiting Stop-now vs Queue choice.
type steerOffer struct {
	Msg     channel.Message
	Created time.Time
}

type steerState struct {
	mu       sync.Mutex
	offers   map[string]steerOffer // id → pending choice
	queue    []channel.Message     // run after current finishes
	interrupt *channel.Message     // run next after cancel
}

func (s *steerState) init() {
	s.mu.Lock()
	if s.offers == nil {
		s.offers = map[string]steerOffer{}
	}
	s.mu.Unlock()
}

func (s *steerState) putOffer(msg channel.Message) string {
	s.init()
	id := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	s.mu.Lock()
	s.offers[id] = steerOffer{Msg: msg, Created: time.Now()}
	// drop offers older than 30m
	for k, o := range s.offers {
		if time.Since(o.Created) > 30*time.Minute {
			delete(s.offers, k)
		}
	}
	s.mu.Unlock()
	return id
}

func (s *steerState) takeOffer(id string) (channel.Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.offers[id]
	if !ok {
		return channel.Message{}, false
	}
	delete(s.offers, id)
	return o.Msg, true
}

func (s *steerState) enqueue(msg channel.Message) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, msg)
	return len(s.queue)
}

func (s *steerState) setInterrupt(msg channel.Message) {
	s.mu.Lock()
	cp := msg
	s.interrupt = &cp
	s.mu.Unlock()
}

// takeNext returns interrupt first, else next queued message.
func (s *steerState) takeNext() (channel.Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.interrupt != nil {
		m := *s.interrupt
		s.interrupt = nil
		return m, true
	}
	if len(s.queue) == 0 {
		return channel.Message{}, false
	}
	m := s.queue[0]
	s.queue = s.queue[1:]
	return m, true
}

func (s *steerState) queueLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// offerSteerWhenBusy presents Stop vs Queue for a mid-run user message.
func (e *Engine) offerSteerWhenBusy(ctx context.Context, msg channel.Message) {
	id := e.steer.putOffer(msg)
	preview := strings.TrimSpace(msg.Text)
	if preview == "" {
		preview = "(attachment)"
	}
	runes := []rune(preview)
	if len(runes) > 120 {
		preview = string(runes[:120]) + "…"
	}
	text := fmt.Sprintf(
		"⏳ Agent is busy.\n\nYour message:\n«%s»\n\nHow should I apply it?",
		preview,
	)
	e.deliverKeyboard(ctx, msg, channel.KeyboardMessage{
		Text: text,
		Rows: [][]channel.Button{
			{{Text: "⏹ Stop & run now", CallbackData: "steer:now:" + id}},
			{{Text: "📥 Queue after current", CallbackData: "steer:queue:" + id}},
			{{Text: "❌ Dismiss", CallbackData: "steer:dismiss:" + id}},
		},
	})
}

func (e *Engine) handleSteerCallback(ctx context.Context, msg channel.Message, data string) bool {
	parts := strings.SplitN(data, ":", 3)
	if len(parts) < 3 || parts[0] != "steer" {
		return false
	}
	action, id := parts[1], parts[2]
	switch action {
	case "dismiss":
		e.steer.takeOffer(id)
		_ = msg.Reply.SendText(ctx, "Dismissed.")
		return true
	case "queue":
		m, ok := e.steer.takeOffer(id)
		if !ok {
			_ = msg.Reply.SendText(ctx, "That choice expired. Send your message again.")
			return true
		}
		n := e.steer.enqueue(m)
		_ = msg.Reply.SendText(ctx, fmt.Sprintf("📥 Queued (#%d). Will run after the current task finishes.", n))
		return true
	case "now":
		m, ok := e.steer.takeOffer(id)
		if !ok {
			_ = msg.Reply.SendText(ctx, "That choice expired. Send your message again.")
			return true
		}
		e.steer.setInterrupt(m)
		e.mu.Lock()
		run := e.active
		e.mu.Unlock()
		if run != nil {
			run.cancel()
			_ = msg.Reply.SendText(ctx, "⏹ Stopping current run… your message will start next.")
		} else {
			// nothing active — run immediately
			go e.runAgent(context.WithoutCancel(ctx), m)
			_ = msg.Reply.SendText(ctx, "▶ Starting your message.")
		}
		return true
	}
	return false
}

// drainSteerAfterRun starts interrupt/queued work after the active run ends.
func (e *Engine) drainSteerAfterRun(ctx context.Context) {
	next, ok := e.steer.takeNext()
	if !ok {
		return
	}
	log.Info().Int("text_len", len(next.Text)).Msg("steer: starting next queued/interrupt message")
	// ensure Reply is still usable
	if next.Reply == nil {
		next.Reply = e.ch.OwnerReplier()
	}
	go e.runAgent(context.WithoutCancel(ctx), next)
}
