// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package providers

import (
	"context"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers/openai_compat"
)

type HTTPProvider struct {
	delegate *openai_compat.Provider
}

// NewHTTPProvider forwards optional provider-specific compatibility flags
// without changing the shared HTTP provider interface.
func NewHTTPProvider(apiKey, apiBase, proxy string, opts ...openai_compat.Option) *HTTPProvider {
	return &HTTPProvider{
		delegate: openai_compat.NewProvider(apiKey, apiBase, proxy, opts...),
	}
}

func NewHTTPProviderWithMaxTokensField(apiKey, apiBase, proxy, maxTokensField string) *HTTPProvider {
	return NewHTTPProviderWithMaxTokensFieldAndRequestTimeout(apiKey, apiBase, proxy, maxTokensField, 0)
}

func NewHTTPProviderWithMaxTokensFieldAndRequestTimeout(
	apiKey, apiBase, proxy, maxTokensField string,
	requestTimeoutSeconds int,
	opts ...openai_compat.Option,
) *HTTPProvider {
	// Apply the legacy defaults first, then append any protocol-specific
	// behavior switches such as OpenAI's /responses preference.
	providerOpts := make([]openai_compat.Option, 0, 2+len(opts))
	providerOpts = append(providerOpts,
		openai_compat.WithMaxTokensField(maxTokensField),
		openai_compat.WithRequestTimeout(time.Duration(requestTimeoutSeconds)*time.Second),
	)
	providerOpts = append(providerOpts, opts...)

	return &HTTPProvider{
		delegate: openai_compat.NewProvider(apiKey, apiBase, proxy, providerOpts...),
	}
}

func (p *HTTPProvider) Chat(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	return p.delegate.Chat(ctx, messages, tools, model, options)
}

// ChatStream implements providers.StreamingProvider by delegating to the
// OpenAI-compatible streaming endpoint (SSE with stream: true).
func (p *HTTPProvider) ChatStream(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
	onChunk func(accumulated string),
) (*LLMResponse, error) {
	return p.delegate.ChatStream(ctx, messages, tools, model, options, onChunk)
}

func (p *HTTPProvider) GetDefaultModel() string {
	return ""
}

func (p *HTTPProvider) SupportsNativeSearch() bool {
	return p.delegate.SupportsNativeSearch()
}
