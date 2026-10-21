package transport

import "context"

type replierKey struct{}

func WithReplier(ctx context.Context, r Replier) context.Context {
	return context.WithValue(ctx, replierKey{}, r)
}

func ReplierFromContext(ctx context.Context) (Replier, bool) {
	r, ok := ctx.Value(replierKey{}).(Replier)
	return r, ok
}

type sinkKey struct{}

// WithSink attaches the agent's event sink to the context so tools running deep
// in the call stack can emit progress/status events without a direct reference.
func WithSink(ctx context.Context, s Sink) context.Context {
	return context.WithValue(ctx, sinkKey{}, s)
}

func SinkFromContext(ctx context.Context) (Sink, bool) {
	s, ok := ctx.Value(sinkKey{}).(Sink)
	return s, ok
}
