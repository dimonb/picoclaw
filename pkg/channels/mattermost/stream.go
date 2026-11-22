package mattermost

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// BeginStream implements channels.StreamingCapable (opt-in via
// settings.streaming.enabled). Partial output is posted once and then
// edited in place via PUT /posts/{id}/patch.
func (c *MattermostChannel) BeginStream(ctx context.Context, chatID string) (channels.Streamer, error) {
	if !c.config.Streaming.Enabled {
		return nil, fmt.Errorf("streaming disabled in config")
	}
	channelID, rootID := parseChatID(chatID)
	if channelID == "" {
		return nil, fmt.Errorf("invalid mattermost chat ID %q", chatID)
	}
	cfg := c.config.Streaming.WithDefaults(2, 100)
	return &mattermostStreamer{
		ch:        c,
		chatID:    chatID,
		channelID: channelID,
		rootID:    rootID,
		throttle:  time.Duration(cfg.ThrottleSeconds) * time.Second,
		minGrowth: cfg.MinGrowthChars,
	}, nil
}

type mattermostStreamer struct {
	ch        *MattermostChannel
	chatID    string
	channelID string
	rootID    string
	throttle  time.Duration
	minGrowth int

	mu        sync.Mutex
	postID    string
	lastLen   int
	lastAt    time.Time
	finalized bool
	delivered []string
}

// Update shows the partial content, creating the post on first call and
// editing it afterwards. Updates are throttled by time and growth.
func (s *mattermostStreamer) Update(ctx context.Context, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized || content == "" {
		return nil
	}
	now := time.Now()
	if s.postID != "" && now.Sub(s.lastAt) < s.throttle && len(content)-s.lastLen < s.minGrowth {
		return nil
	}

	// While streaming, only the head fits in one post; Finalize splits.
	display := content
	if runes := []rune(content); len(runes) > maxMessageLength {
		display = string(runes[:maxMessageLength-1]) + "…"
	}
	if s.postID == "" {
		id, err := s.ch.createPost(ctx, s.channelID, s.rootID, display, nil)
		if err != nil {
			return fmt.Errorf("mattermost stream: %w", err)
		}
		s.postID = id
	} else if err := s.ch.EditMessage(ctx, s.chatID, s.postID, display); err != nil {
		return fmt.Errorf("mattermost stream: %w", err)
	}
	s.lastLen = len(content)
	s.lastAt = now
	return nil
}

// Finalize writes the full content: the streamed post is edited to the
// first chunk and any overflow is posted as follow-up messages.
func (s *mattermostStreamer) Finalize(ctx context.Context, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized {
		return nil
	}

	chunks := channels.SplitMessage(content, maxMessageLength)
	if len(chunks) == 0 {
		// Nothing to show: drop the partial post, if any.
		if s.postID != "" {
			_ = s.ch.DeleteMessage(ctx, s.chatID, s.postID)
			s.postID = ""
		}
		s.finalized = true
		return nil
	}

	ids := make([]string, 0, len(chunks))
	if s.postID != "" {
		if err := s.ch.EditMessage(ctx, s.chatID, s.postID, chunks[0]); err != nil {
			return fmt.Errorf("mattermost stream finalize: %w", err)
		}
		ids = append(ids, s.postID)
		chunks = chunks[1:]
	}
	for _, chunk := range chunks {
		id, err := s.ch.createPost(ctx, s.channelID, s.rootID, chunk, nil)
		if err != nil {
			s.delivered = ids
			return fmt.Errorf("mattermost stream finalize: %w", err)
		}
		ids = append(ids, id)
	}
	s.delivered = ids
	s.finalized = true
	return nil
}

// Cancel removes a partially streamed post when the stream is abandoned
// (the agent then delivers the reply through the normal Send path).
func (s *mattermostStreamer) Cancel(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized || s.postID == "" {
		return
	}
	if err := s.ch.DeleteMessage(ctx, s.chatID, s.postID); err != nil {
		logger.DebugCF("mattermost", "failed to delete canceled stream post", map[string]any{"error": err.Error()})
	}
	s.postID = ""
	s.finalized = true
}

// DeliveredMessageIDs reports the post IDs that carry the final content.
func (s *mattermostStreamer) DeliveredMessageIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.delivered...)
}
