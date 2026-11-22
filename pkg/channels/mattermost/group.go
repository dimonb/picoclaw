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
	c.discardPending(ctx, channelID, rootID)
	c.threads.done(channelID, rootID)
}

// --- explicit reactions (reaction tool) -------------------------------------

// reactionNames maps the emoji the reaction tool may use to Mattermost emoji
// names. It covers Telegram's reaction set plus common extras; any other
// Mattermost emoji (including custom ones) can be given as ":name:".
var reactionNames = map[string]string{
	"👍": "+1", "👎": "-1", "❤️": "heart", "🔥": "fire", "🥰": "smiling_face_with_3_hearts", "👏": "clap",
	"😁": "grin", "🤔": "thinking_face", "🤯": "exploding_head", "😱": "scream", "🤬": "face_with_symbols_on_mouth",
	"😢": "cry", "🎉": "tada", "🤩": "star-struck", "🤮": "face_vomiting", "💩": "hankey", "🙏": "pray",
	"👌": "ok_hand", "🕊️": "dove_of_peace", "🤡": "clown_face", "🥱": "yawning_face", "🥴": "woozy_face",
	"😍": "heart_eyes", "🐳": "whale", "🌚": "new_moon_with_face", "🌭": "hotdog", "💯": "100",
	"🤣": "rolling_on_the_floor_laughing", "⚡": "zap", "🍌": "banana", "🏆": "trophy", "💔": "broken_heart",
	"🤨": "face_with_raised_eyebrow", "😐": "neutral_face", "🍓": "strawberry", "🍾": "champagne", "💋": "kiss",
	"😈": "smiling_imp", "😴": "sleeping", "😭": "sob", "🤓": "nerd_face", "👻": "ghost",
	"👨‍💻": "male-technologist", "👀": "eyes", "🎃": "jack_o_lantern", "🙈": "see_no_evil", "😇": "innocent",
	"😨": "fearful", "🤝": "handshake", "✍️": "writing_hand", "🤗": "hugging_face", "🫡": "saluting_face",
	"🎅": "santa", "🎄": "christmas_tree", "☃️": "snowman", "💅": "nail_care", "🤪": "zany_face", "🗿": "moyai",
	"🆒": "cool", "💘": "cupid", "🙉": "hear_no_evil", "🦄": "unicorn_face", "😘": "kissing_heart", "💊": "pill",
	"🙊": "speak_no_evil", "😎": "sunglasses", "👾": "space_invader", "🤷": "shrug", "🤷‍♂️": "man-shrugging",
	"🤷‍♀️": "woman-shrugging", "😡": "rage",
	// common extras
	"😂": "joy", "😄": "smile", "🙂": "slightly_smiling_face", "😉": "wink", "😅": "sweat_smile",
	"😮": "open_mouth", "✅": "white_check_mark", "✔️": "heavy_check_mark", "❌": "x", "⚠️": "warning",
	"🚀": "rocket", "💪": "muscle", "👋": "wave", "⭐": "star", "🤞": "crossed_fingers", "🆗": "ok",
	"👆": "point_up_2", "🙌": "raised_hands", "💡": "bulb", "🐛": "bug", "📌": "pushpin", "⏳": "hourglass_flowing_sand",
}

var emojiNameRe = regexp.MustCompile(`^[a-z0-9_+\-]+$`)

// reactionNamesBare is reactionNames keyed without variation selectors, so
// "❤" and "❤️" both resolve.
var reactionNamesBare = func() map[string]string {
	m := make(map[string]string, len(reactionNames))
	for emoji, name := range reactionNames {
		m[stripVariationSelectors(emoji)] = name
	}
	return m
}()

func stripVariationSelectors(s string) string {
	return strings.NewReplacer("\uFE0F", "", "\uFE0E", "").Replace(s)
}

func mattermostEmojiName(emoji string) (string, error) {
	emoji = strings.TrimSpace(emoji)
	if name, ok := reactionNamesBare[stripVariationSelectors(emoji)]; ok {
		return name, nil
	}
	if name := strings.Trim(emoji, ":"); emojiNameRe.MatchString(name) {
		return name, nil
	}
	return "", fmt.Errorf(
		"unsupported mattermost reaction %q (use a common emoji or a Mattermost emoji name like \":thumbsup:\"): %w",
		emoji, channels.ErrSendFailed)
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
	if err := c.addReaction(ctx, postID, name); err != nil {
		return err
	}
	c.keepReaction(postID, name)
	return nil
}

// RemoveMessageReaction implements channels.MessageReactor.
func (c *MattermostChannel) RemoveMessageReaction(ctx context.Context, chatID, messageID, emoji string) error {
	name, err := mattermostEmojiName(emoji)
	if err != nil {
		return err
	}
	postID := parseMessageID(messageID).postID
	if postID == "" {
		return fmt.Errorf("mattermost reaction: empty message id: %w", channels.ErrSendFailed)
	}
	return c.removeReaction(ctx, postID, name)
}

// GetReactionSupport implements channels.MessageReactor. Like Telegram it
// reports any emoji: the channel maps it to a Mattermost name itself (see
// reactionNames) and also accepts ":name:", which an allow-list in the
// reaction tool would reject.
func (c *MattermostChannel) GetReactionSupport(context.Context, string) channels.ReactionSupport {
	return channels.ReactionSupport{AnyUnicode: true}
}
