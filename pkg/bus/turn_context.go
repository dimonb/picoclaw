package bus

import "context"

type inboundContextKey struct{}

// WithInboundContext attaches the inbound context of the turn being processed
// to ctx. Channels can read it back (InboundContextFrom) in calls that only
// receive a chat ID, e.g. StreamingCapable.BeginStream, to address the exact
// message/thread the turn is answering.
func WithInboundContext(ctx context.Context, inbound InboundContext) context.Context {
	return context.WithValue(ctx, inboundContextKey{}, inbound)
}

// InboundContextFrom returns the turn's inbound context stored by
// WithInboundContext, if any.
func InboundContextFrom(ctx context.Context) (InboundContext, bool) {
	if ctx == nil {
		return InboundContext{}, false
	}
	inbound, ok := ctx.Value(inboundContextKey{}).(InboundContext)
	return inbound, ok
}
