package transport

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"
)

// Bus is a non-blocking event emitter backed by a buffered channel.
// The consumer reads from the receive-only channel returned by NewBus.
type Bus struct {
	ch chan<- Event
}

// NewBus creates a Bus and the corresponding receive channel.
// The caller owns closing via Bus.Close.
func NewBus(size int) (*Bus, <-chan Event) {
	ch := make(chan Event, size)
	return &Bus{ch: ch}, ch
}

// Emit sends an event without blocking. If the channel is full the event
// is dropped and a warning is logged. Use only for UI events that tolerate
// loss; persistent events must go through a guaranteed path.
func (b *Bus) Emit(ev Event) {
	if b == nil || b.ch == nil {
		return
	}
	select {
	case b.ch <- ev:
	default:
		log.Warn().Str("event", fmt.Sprintf("%T", ev)).Msg("event dropped — bus full")
	}
}

// EmitBlocking sends an event, blocking until it is delivered or the context
// is cancelled. Use for events that must not be dropped (e.g. Ask).
func (b *Bus) EmitBlocking(ctx context.Context, ev Event) error {
	if b == nil || b.ch == nil {
		return context.Canceled
	}
	select {
	case b.ch <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close closes the underlying channel, signalling consumers to stop.
func (b *Bus) Close() {
	if b != nil && b.ch != nil {
		close(b.ch)
	}
}
