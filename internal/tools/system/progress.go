package system

import (
	"context"

	"github.com/gemechischala/awaki/internal/tasks"
	"github.com/gemechischala/awaki/internal/transport"
)

// reportProgress surfaces a one-line status for a long-running tool to the
// chat UI (live runs) and to the background-task board (bg runs).
func reportProgress(ctx context.Context, text string) {
	if sink, ok := transport.SinkFromContext(ctx); ok {
		sink.Emit(transport.ProgressEvent{Status: text})
	}
	if fn, ok := tasks.ProgressFromContext(ctx); ok {
		fn(text, nil)
	}
}
