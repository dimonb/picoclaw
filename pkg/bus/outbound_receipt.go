package bus

import "context"

type outboundReceiptKey struct{}

// WithOutboundReceiptObserver attaches a request-scoped observer. Channels may
// report each confirmed native payload before returning, including partial sends.
// The observer must finish synchronously and must not turn persistence errors
// into send errors (retrying an accepted send would duplicate delivery).
func WithOutboundReceiptObserver(ctx context.Context, fn func(OutboundDelivery)) context.Context {
	return context.WithValue(ctx, outboundReceiptKey{}, fn)
}

// ObserveOutboundReceipt reports only a payload accepted by the native API.
func ObserveOutboundReceipt(ctx context.Context, d OutboundDelivery) {
	if fn, ok := ctx.Value(outboundReceiptKey{}).(func(OutboundDelivery)); ok && fn != nil {
		fn(d)
	}
}
