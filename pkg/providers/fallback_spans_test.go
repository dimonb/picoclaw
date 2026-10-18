package providers

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestFallbackSpansRemainSiblings(t *testing.T) {
	for _, mode := range []string{"chat", "image"} {
		t.Run(mode, func(t *testing.T) {
			recorder := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			previous := otel.GetTracerProvider()
			otel.SetTracerProvider(tp)
			t.Cleanup(func() {
				otel.SetTracerProvider(previous)
				_ = tp.Shutdown(context.Background())
			})
			ctx, root := tp.Tracer("test").Start(context.Background(), "root")
			defer root.End()
			fc := NewFallbackChain(NewCooldownTracker(), nil)
			candidates := []FallbackCandidate{
				makeCandidate("first", "first"),
				makeCandidate("second", "second"),
				makeCandidate("third", "third"),
			}
			run := func(_ context.Context, _, model string) (*LLMResponse, error) {
				if model != "third" {
					return nil, errors.New("rate limit exceeded")
				}
				return &LLMResponse{Content: "ok"}, nil
			}
			var err error
			if mode == "image" {
				_, err = fc.ExecuteImage(ctx, candidates, run)
			} else {
				_, err = fc.Execute(ctx, candidates, run)
			}
			if err != nil {
				t.Fatal(err)
			}
			var fallback sdktrace.ReadOnlySpan
			var attempts []sdktrace.ReadOnlySpan
			for _, span := range recorder.Ended() {
				switch span.Name() {
				case "Fallback":
					if fallback != nil {
						t.Fatal("more than one fallback span")
					}
					fallback = span
				case "LLMAttempt":
					attempts = append(attempts, span)
				}
			}
			if fallback == nil || len(attempts) != 3 {
				t.Fatalf("fallback = %v, attempts = %d", fallback, len(attempts))
			}
			if fallback.Parent().SpanID() != root.SpanContext().SpanID() ||
				attempts[0].Parent().SpanID() != root.SpanContext().SpanID() {
				t.Fatal("primary attempt and fallback must be children of the original context")
			}
			for _, attempt := range attempts[1:] {
				if attempt.Parent().SpanID() != fallback.SpanContext().SpanID() {
					t.Fatal("fallback attempts must be sibling children of the fallback span")
				}
			}
		})
	}
}
