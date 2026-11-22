package mattermost

import (
	"context"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// Tool feedback (agents.defaults.tool_feedback): progress messages such as
// "🔧 web_search …" are kept in one post per thread that is edited in place,
// like Telegram's tracked tool-feedback message. They do not consume the
// thread's placeholder or stop typing. When the answer arrives the feedback
// post is reused for it (if there is no placeholder) or deleted.
//
// With tool_feedback.separate_messages the Manager calls
// ClearToolFeedbackMessage before every send, so each feedback becomes its
// own post and stays in the chat (as in Telegram).
//
// A tracked post that no answer or cleanup claims (e.g. the turn was stopped)
// is deleted after indicatorTTL by sweepFeedback.

const toolFeedbackKind = "tool_feedback"

type feedbackPost struct {
	id string
	at time.Time
}

func isToolFeedback(msg bus.OutboundMessage) bool {
	return strings.EqualFold(strings.TrimSpace(msg.Context.Raw["message_kind"]), toolFeedbackKind)
}

// sendToolFeedback shows content in the thread's tool-feedback post.
func (c *MattermostChannel) sendToolFeedback(ctx context.Context, channelID, rootID, content string) ([]string, error) {
	c.sweepFeedback(ctx)
	key := threadKey(channelID, rootID)
	content = truncateRunes(content, maxMessageLength)
	c.indMu.Lock()
	postID := c.feedback[key].id
	c.indMu.Unlock()
	if postID != "" {
		if err := c.patchPost(ctx, postID, content); err == nil {
			// Still in use: a long turn must not have its live progress
			// post swept as stale.
			c.indMu.Lock()
			if fb, ok := c.feedback[key]; ok && fb.id == postID {
				c.feedback[key] = feedbackPost{id: postID, at: time.Now()}
			}
			c.indMu.Unlock()
			return []string{encodeMessageID(channelID, rootID, postID)}, nil
		}
	}
	postID, err := c.createPost(ctx, channelID, rootID, content, nil)
	if err != nil {
		return nil, err
	}
	c.indMu.Lock()
	c.feedback[key] = feedbackPost{id: postID, at: time.Now()}
	c.indMu.Unlock()
	return []string{encodeMessageID(channelID, rootID, postID)}, nil
}

// sweepFeedback deletes tool-feedback posts older than indicatorTTL that no
// answer or cleanup claimed.
func (c *MattermostChannel) sweepFeedback(ctx context.Context) {
	var stale []string
	c.indMu.Lock()
	for key, fb := range c.feedback {
		if time.Since(fb.at) > indicatorTTL {
			stale = append(stale, fb.id)
			delete(c.feedback, key)
		}
	}
	c.indMu.Unlock()
	for _, id := range stale {
		_ = c.deletePost(ctx, id)
	}
}

// RecordToolFeedbackMessage and ClearToolFeedbackMessage implement the
// Manager's tool-feedback tracker (chatID is the key from
// ToolFeedbackMessageChatID). Clear forgets the post without deleting it.
func (c *MattermostChannel) RecordToolFeedbackMessage(chatID, messageID, _ string) {
	if postID := parseMessageID(messageID).postID; postID != "" {
		c.indMu.Lock()
		c.feedback[chatID] = feedbackPost{id: postID, at: time.Now()}
		c.indMu.Unlock()
	}
}

func (c *MattermostChannel) ClearToolFeedbackMessage(chatID string) {
	c.indMu.Lock()
	delete(c.feedback, chatID)
	c.indMu.Unlock()
}

func (c *MattermostChannel) takeFeedback(channelID, rootID string) string {
	key := threadKey(channelID, rootID)
	c.indMu.Lock()
	defer c.indMu.Unlock()
	postID := c.feedback[key].id
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
