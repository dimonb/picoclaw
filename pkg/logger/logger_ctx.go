package logger

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// withTraceFields decorates fields with trace_id and span_id when ctx carries
// a recording span. Returns the (possibly mutated) fields map. If ctx is nil
// or has no valid span, returns fields unchanged.
func withTraceFields(ctx context.Context, fields map[string]any) map[string]any {
	if ctx == nil {
		return fields
	}
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return fields
	}
	if fields == nil {
		fields = make(map[string]any, 2)
	}
	fields["trace_id"] = sc.TraceID().String()
	fields["span_id"] = sc.SpanID().String()
	return fields
}

func DebugCtx(ctx context.Context, message string) {
	DebugF(message, withTraceFields(ctx, nil))
}
func InfoCtx(ctx context.Context, message string) {
	InfoF(message, withTraceFields(ctx, nil))
}
func WarnCtx(ctx context.Context, message string) {
	WarnF(message, withTraceFields(ctx, nil))
}
func ErrorCtx(ctx context.Context, message string) {
	ErrorF(message, withTraceFields(ctx, nil))
}

func DebugFCtx(ctx context.Context, message string, fields map[string]any) {
	DebugF(message, withTraceFields(ctx, fields))
}
func InfoFCtx(ctx context.Context, message string, fields map[string]any) {
	InfoF(message, withTraceFields(ctx, fields))
}
func WarnFCtx(ctx context.Context, message string, fields map[string]any) {
	WarnF(message, withTraceFields(ctx, fields))
}
func ErrorFCtx(ctx context.Context, message string, fields map[string]any) {
	ErrorF(message, withTraceFields(ctx, fields))
}

func DebugCCtx(ctx context.Context, component string, message string) {
	DebugCF(component, message, withTraceFields(ctx, nil))
}
func InfoCCtx(ctx context.Context, component string, message string) {
	InfoCF(component, message, withTraceFields(ctx, nil))
}
func WarnCCtx(ctx context.Context, component string, message string) {
	WarnCF(component, message, withTraceFields(ctx, nil))
}
func ErrorCCtx(ctx context.Context, component string, message string) {
	ErrorCF(component, message, withTraceFields(ctx, nil))
}

func DebugCFCtx(ctx context.Context, component string, message string, fields map[string]any) {
	DebugCF(component, message, withTraceFields(ctx, fields))
}
func InfoCFCtx(ctx context.Context, component string, message string, fields map[string]any) {
	InfoCF(component, message, withTraceFields(ctx, fields))
}
func WarnCFCtx(ctx context.Context, component string, message string, fields map[string]any) {
	WarnCF(component, message, withTraceFields(ctx, fields))
}
func ErrorCFCtx(ctx context.Context, component string, message string, fields map[string]any) {
	ErrorCF(component, message, withTraceFields(ctx, fields))
}
