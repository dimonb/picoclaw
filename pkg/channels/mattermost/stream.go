package mattermost

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// BeginStream implements channels.StreamingCapable (opt-in via
// settings.streaming.enabled). Partial output is posted once and then
// edited in place via PUT /posts/{id}/patch. The thread comes from the
// turn's inbound context carried in ctx (chatID is only the channel).
func (c *MattermostChannel) BeginStream(ctx context.Context, chatID string) (channels.Streamer, error) {
	if !c.config.Streaming.Enabled {
		return nil, fmt.Errorf("streaming disabled in config")
	}
	var turn *bus.InboundContext
	if inbound, ok := bus.InboundContextFrom(ctx); ok {
		turn = &inbound
	}
	channelID, rootID := resolveTarget(chatID, turn, "")
	if channelID == "" {
		return nil, fmt.Errorf("invalid mattermost chat ID %q", chatID)
	}
	cfg := c.config.Streaming.WithDefaults(2, 100)
	return &mattermostStreamer{
		ch:        c,
		channelID: channelID,
		rootID:    rootID,
		throttle:  time.Duration(cfg.ThrottleSeconds) * time.Second,
		minGrowth: cfg.MinGrowthChars,
	}, nil
}

type mattermostStreamer struct {
	ch        *MattermostChannel
	channelID string
	rootID    string
	throttle  time.Duration
	minGrowth int

	mu        sync.Mutex
	started   bool // pending thread indicators consumed
	postID    string
	lastLen   int
	lastAt    time.Time
	finalized bool
	delivered []string
}

// start consumes the thread's pending indicators (stops typing/👀) and
// adopts its placeholder (or tool-feedback) post, if any, as the post to edit.
func (s *mattermostStreamer) start(ctx context.Context) {
	if s.started {
		return
	}
	s.started = true
	if reuse := s.ch.takeReusablePost(ctx, s.channelID, s.rootID); reuse != "" {
		if s.postID == "" {
			s.postID = reuse
		} else {
			_ = s.ch.deletePost(ctx, reuse)
		}
	}
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

	s.start(ctx)
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
	} else if err := s.ch.patchPost(ctx, s.postID, display); err != nil {
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
	s.start(ctx)

	chunks := channels.SplitMessage(content, maxMessageLength)
	if len(chunks) == 0 {
		// Nothing to show: drop the partial post, if any.
		if s.postID != "" {
			_ = s.ch.deletePost(ctx, s.postID)
			s.postID = ""
		}
		s.finalized = true
		s.ch.threads.done(s.channelID, s.rootID)
		return nil
	}

	ids := make([]string, 0, len(chunks))
	if s.postID != "" {
		if err := s.ch.patchPost(ctx, s.postID, chunks[0]); err != nil {
			return fmt.Errorf("mattermost stream finalize: %w", err)
		}
		ids = append(ids, encodeMessageID(s.channelID, s.rootID, s.postID))
		chunks = chunks[1:]
	}
	for _, chunk := range chunks {
		id, err := s.ch.createPost(ctx, s.channelID, s.rootID, chunk, nil)
		if err != nil {
			s.delivered = ids
			return fmt.Errorf("mattermost stream finalize: %w", err)
		}
		ids = append(ids, encodeMessageID(s.channelID, s.rootID, id))
	}
	s.delivered = ids
	s.finalized = true
	s.ch.threads.done(s.channelID, s.rootID)
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
	if err := s.ch.deletePost(ctx, s.postID); err != nil {
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
