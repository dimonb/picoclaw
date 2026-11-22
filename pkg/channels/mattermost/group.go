package mattermost

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
)

// Group modes (settings.group_mode) for channel/group posts. DMs are always
// handled.
const (
	// groupModeSmart (default): every post in channels the bot is a member of
	// reaches the agent, which decides whether to speak (NO_REPLY = stay
	// quiet). Mentions and thread follow-ups are flagged as addressed.
	groupModeSmart = "smart"
	// groupModeMention: only @mentions, follow-ups in threads the bot takes
	// part in, and group_trigger prefixes reach the agent.
	groupModeMention = "mention"
)

func normalizeGroupMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", groupModeSmart:
		return groupModeSmart, nil
	case groupModeMention:
		return groupModeMention, nil
	default:
		return "", fmt.Errorf("mattermost group_mode must be %q or %q, got %q", groupModeSmart, groupModeMention, mode)
	}
}

// markBotThread records that the bot posted in thread root of channelID.
func (c *MattermostChannel) markBotThread(channelID, rootID string) {
	if channelID != "" && rootID != "" {
		c.botThreads.Store(threadKey(channelID, rootID), true)
	}
}

// botInThread reports whether the bot takes part in a thread, so a follow-up
// there counts as addressed (like replying to the bot in Telegram). Threads
// the bot posted in during this process are known; others (e.g. after a
// restart) are looked up once via /posts/{root}/thread and cached.
func (c *MattermostChannel) botInThread(ctx context.Context, channelID, rootID string) bool {
	key := threadKey(channelID, rootID)
	if v, ok := c.botThreads.Load(key); ok {
		in, _ := v.(bool)
		return in
	}
	var thread struct {
		Posts map[string]struct {
			UserID string `json:"user_id"`
		} `json:"posts"`
	}
	if err := c.apiJSON(ctx, http.MethodGet, "/api/v4/posts/"+url.PathEscape(rootID)+"/thread", nil, &thread); err != nil {
		return false // not cached: retried on the next post
	}
	in := false
	for _, p := range thread.Posts {
		if p.UserID == c.botUserID {
			in = true
			break
		}
	}
	c.botThreads.LoadOrStore(key, in)
	return in
}

// HandleSilentReply implements channels.SilentReplyHandler: the agent chose
// not to answer (NO_REPLY), so the thread's pending typing/👀/placeholder are
// removed and the thread's turn is marked finished.
func (c *MattermostChannel) HandleSilentReply(ctx context.Context, msg bus.OutboundMessage) {
	channelID, rootID := resolveTarget(msg.ChatID, &msg.Context, msg.ReplyToMessageID)
	if channelID == "" {
		return
	}
	if pending := c.takeIndicators(channelID, rootID); pending.placeholderID != "" {
		_ = c.deletePost(ctx, pending.placeholderID)
	}
	c.threads.done(channelID, rootID)
}

// --- explicit reactions (reaction tool) -------------------------------------

// reactionNames maps the emoji the reaction tool may use to Mattermost
// emoji names.
var reactionNames = map[string]string{
	"👍": "+1", "👎": "-1", "👀": "eyes", "❤️": "heart", "😂": "joy", "😄": "smile", "🎉": "tada",
	"✅": "white_check_mark", "🙏": "pray", "🔥": "fire", "👏": "clap", "🤔": "thinking_face",
	"😢": "cry", "😮": "open_mouth", "🚀": "rocket", "💯": "100", "👌": "ok_hand", "❌": "x",
}

var emojiNameRe = regexp.MustCompile(`^[a-z0-9_+\-]+$`)

func mattermostEmojiName(emoji string) (string, error) {
	emoji = strings.TrimSpace(emoji)
	if name, ok := reactionNames[emoji]; ok {
		return name, nil
	}
	if name := strings.Trim(emoji, ":"); emojiNameRe.MatchString(name) {
		return name, nil
	}
	return "", fmt.Errorf("unsupported mattermost reaction %q: %w", emoji, channels.ErrSendFailed)
}

// SetMessageReaction implements channels.MessageReactor.
func (c *MattermostChannel) SetMessageReaction(ctx context.Context, chatID, messageID, emoji string) error {
	name, err := mattermostEmojiName(emoji)
	if err != nil {
		return err
	}
	postID := parseMessageID(messageID).postID
	if postID == "" {
		return fmt.Errorf("mattermost reaction: empty message id: %w", channels.ErrSendFailed)
	}
	return c.addReaction(ctx, postID, name)
}

// RemoveMessageReaction implements channels.MessageReactor.
func (c *MattermostChannel) RemoveMessageReaction(ctx context.Context, chatID, messageID, emoji string) error {
	name, err := mattermostEmojiName(emoji)
	if err != nil {
		return err
	}
	return c.removeReaction(ctx, parseMessageID(messageID).postID, name)
}

// GetReactionSupport implements channels.MessageReactor.
func (c *MattermostChannel) GetReactionSupport(context.Context, string) channels.ReactionSupport {
	allowed := make([]string, 0, len(reactionNames))
	for emoji := range reactionNames {
		allowed = append(allowed, emoji)
	}
	return channels.ReactionSupport{Allowed: sortedStrings(allowed)}
}

func sortedStrings(in []string) []string {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
	return in
}
