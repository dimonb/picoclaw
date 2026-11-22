package mattermost

import (
	"context"
	"strings"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// Tool feedback (agents.defaults.tool_feedback): progress messages such as
// "🔧 web_search …" are kept in one post per thread that is edited in place,
// like Telegram's tracked tool-feedback message. They do not consume the
// thread's placeholder or stop typing. When the answer arrives the feedback
// post is reused for it (if there is no placeholder) or deleted.

const toolFeedbackKind = "tool_feedback"

func isToolFeedback(msg bus.OutboundMessage) bool {
	return strings.EqualFold(strings.TrimSpace(msg.Context.Raw["message_kind"]), toolFeedbackKind)
}

// sendToolFeedback shows content in the thread's tool-feedback post.
func (c *MattermostChannel) sendToolFeedback(ctx context.Context, channelID, rootID, content string) ([]string, error) {
	key := threadKey(channelID, rootID)
	content = truncateRunes(content, maxMessageLength)
	c.indMu.Lock()
	postID := c.feedback[key]
	c.indMu.Unlock()
	if postID != "" {
		if err := c.patchPost(ctx, postID, content); err == nil {
			return []string{encodeMessageID(channelID, rootID, postID)}, nil
		}
	}
	postID, err := c.createPost(ctx, channelID, rootID, content, nil)
	if err != nil {
		return nil, err
	}
	c.indMu.Lock()
	c.feedback[key] = postID
	c.indMu.Unlock()
	return []string{encodeMessageID(channelID, rootID, postID)}, nil
}

func (c *MattermostChannel) takeFeedback(channelID, rootID string) string {
	key := threadKey(channelID, rootID)
	c.indMu.Lock()
	defer c.indMu.Unlock()
	postID := c.feedback[key]
	delete(c.feedback, key)
	return postID
}

// takeReusablePost ends the thread's pending indicators and returns a post
// the reply can be edited into: the placeholder, else the tool-feedback post.
// Whatever is not reused is deleted.
func (c *MattermostChannel) takeReusablePost(ctx context.Context, channelID, rootID string) string {
	placeholder := c.takeIndicators(channelID, rootID).placeholderID
	feedback := c.takeFeedback(channelID, rootID)
	if placeholder == "" {
		return feedback
	}
	if feedback != "" {
		_ = c.deletePost(ctx, feedback)
	}
	return placeholder
}

// discardPending ends the thread's indicators and deletes its placeholder and
// tool-feedback posts (silent reply, media-only reply).
func (c *MattermostChannel) discardPending(ctx context.Context, channelID, rootID string) {
	if postID := c.takeReusablePost(ctx, channelID, rootID); postID != "" {
		_ = c.deletePost(ctx, postID)
	}
}

// ToolFeedbackMessageChatID lets the channel Manager address the tool-feedback
// post of the thread a message belongs to (key "<channel>/<root>").
func (c *MattermostChannel) ToolFeedbackMessageChatID(chatID string, outboundCtx *bus.InboundContext) string {
	channelID, rootID := resolveTarget(chatID, outboundCtx, "")
	if channelID == "" {
		return ""
	}
	return threadKey(channelID, rootID)
}

// DismissToolFeedbackMessage deletes the tool-feedback post tracked under
// chatID ("<channel>/<root>", see ToolFeedbackMessageChatID).
func (c *MattermostChannel) DismissToolFeedbackMessage(ctx context.Context, chatID string) {
	channelID, rootID, _ := strings.Cut(chatID, "/")
	if postID := c.takeFeedback(channelID, rootID); postID != "" {
		_ = c.deletePost(ctx, postID)
	}
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
