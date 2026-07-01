package providers

import (
	"context"
	"fmt"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

type (
	ToolCall               = protocoltypes.ToolCall
	FunctionCall           = protocoltypes.FunctionCall
	LLMResponse            = protocoltypes.LLMResponse
	StreamChunk            = protocoltypes.StreamChunk
	UsageInfo              = protocoltypes.UsageInfo
	Message                = protocoltypes.Message
	MessageMetadata        = protocoltypes.MessageMetadata
	ToolDefinition         = protocoltypes.ToolDefinition
	ToolFunctionDefinition = protocoltypes.ToolFunctionDefinition
	ExtraContent           = protocoltypes.ExtraContent
	GoogleExtra            = protocoltypes.GoogleExtra
	ContentBlock           = protocoltypes.ContentBlock
	CacheControl           = protocoltypes.CacheControl
	Attachment             = protocoltypes.Attachment
)

type LLMProvider interface {
	Chat(
		ctx context.Context,
		messages []Message,
		tools []ToolDefinition,
		model string,
		options map[string]any,
	) (*LLMResponse, error)
	GetDefaultModel() string
}

type StatefulProvider interface {
	LLMProvider
	Close()
}

// StreamingProvider is an optional interface for providers that support token streaming.
// onChunk receives the accumulated text so far (not individual deltas).
// The returned LLMResponse is the same complete response for compatibility with tool-call handling.
type StreamingProvider interface {
	ChatStream(
		ctx context.Context,
		messages []Message,
		tools []ToolDefinition,
		model string,
		options map[string]any,
		onChunk func(accumulated string),
	) (*LLMResponse, error)
}

type StreamingEventProvider interface {
	ChatStreamEvents(
		ctx context.Context,
		messages []Message,
		tools []ToolDefinition,
		model string,
		options map[string]any,
		onChunk func(StreamChunk),
	) (*LLMResponse, error)
}

// ThinkingCapable is an optional interface for providers that support
// extended thinking (e.g. Anthropic). Used by the agent loop to warn
// when thinking_level is configured but the active provider cannot use it.
type ThinkingCapable interface {
	SupportsThinking() bool
}

// NativeSearchCapable is an optional interface for providers that support
// built-in web search during LLM inference (e.g. OpenAI web_search_preview,
// xAI Grok search). When the active provider implements this interface and
// returns true, the agent loop can hide the client-side web_search tool to
// avoid duplicate search surfaces and use the provider's native search instead.
type NativeSearchCapable interface {
	SupportsNativeSearch() bool
}

// FailoverReason classifies why an LLM request failed for fallback decisions.
type FailoverReason string

const (
	FailoverAuth            FailoverReason = "auth"
	FailoverRateLimit       FailoverReason = "rate_limit"
	FailoverBilling         FailoverReason = "billing"
	FailoverNetwork         FailoverReason = "network"
	FailoverTimeout         FailoverReason = "timeout"
	FailoverFormat          FailoverReason = "format"
	FailoverContextOverflow FailoverReason = "context_overflow"
	FailoverOverloaded      FailoverReason = "overloaded"
	FailoverUnknown         FailoverReason = "unknown"
)

// FailoverError wraps an LLM provider error with classification metadata.
type FailoverError struct {
	Reason   FailoverReason
	Provider string
	Model    string
	Status   int
	// ResetsAt, when non-zero, is the server-provided time at which the limit is
	// expected to reset (from a 429/usage-limit response). The fallback chain
	// uses it to cool the provider down for exactly that long instead of the
	// computed backoff curve.
	ResetsAt time.Time
	Wrapped  error
}

func (e *FailoverError) Error() string {
	return fmt.Sprintf("failover(%s): provider=%s model=%s status=%d: %v",
		e.Reason, e.Provider, e.Model, e.Status, e.Wrapped)
}

func (e *FailoverError) Unwrap() error {
	return e.Wrapped
}

// IsRetriable returns true if this error should trigger fallback to next candidate.
// Non-retriable: Format errors (bad request structure, image dimension/size).
func (e *FailoverError) IsRetriable() bool {
	return e.Reason != FailoverFormat && e.Reason != FailoverContextOverflow
}

// UsageLimitError indicates a provider rejected a request because a usage or
// rate limit was reached (HTTP 429). Providers should return this (wrapped with
// %w) so callers can (a) fail fast to a fallback instead of retrying the same
// provider, and (b) honor the server-provided reset time for cooldown.
//
// Its Error() string intentionally contains "usage limit reached (429)" so that
// even text-based classification matches when errors.As is not used.
type UsageLimitError struct {
	Provider string
	Message  string
	// ResetsAt is the time the limit is expected to reset; zero if the server
	// did not provide one.
	ResetsAt time.Time
	Wrapped  error
}

func (e *UsageLimitError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "usage limit reached"
	}
	if !e.ResetsAt.IsZero() {
		return fmt.Sprintf("usage limit reached (429): %s (resets_at=%s)",
			msg, e.ResetsAt.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("usage limit reached (429): %s", msg)
}

func (e *UsageLimitError) Unwrap() error { return e.Wrapped }

// ModelConfig holds primary model and fallback list.
type ModelConfig struct {
	Primary   string
	Fallbacks []string
}
