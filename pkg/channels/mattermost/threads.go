package mattermost

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// Sessions are per Mattermost channel (InboundContext.ChatID is the plain
// channel ID), while replies go into the thread of the post being answered.
// The thread is therefore carried per message, never per session:
//
//	MessageID         "<channel>:<post>"         top-level post (it is its own thread root)
//	                  "<channel>:<root>:<post>"  reply inside the thread <root>
//	ReplyToMessageID  "<channel>:<root>"         set for replies inside a thread
//
// The same encoding is used for the IDs Send returns, so the message tool's
// reply_to_message_id and edit/delete calls accept them directly.

const (
	indicatorTTL   = 30 * time.Minute
	threadQueueTTL = 30 * time.Minute
	// activeTurnMaxAge bounds how long a thread counts as having an
	// unfinished turn without a reply (a turn that was stopped or crashed
	// never ends): its indicators and tool-feedback post are not swept as
	// stale before that.
	activeTurnMaxAge = 6 * time.Hour
	// metadata keys used by the agent on outbound messages (pkg/agent).
	outboundKindKey   = "outbound_kind"
	outboundKindFinal = "final"
)

type msgRef struct {
	channelID string
	rootID    string
	postID    string
}

// threadRoot is the post ID to use as root_id when replying to this message.
func (r msgRef) threadRoot() string {
	if r.rootID != "" {
		return r.rootID
	}
	return r.postID
}

func encodeMessageID(channelID, rootID, postID string) string {
	if postID == "" {
		return ""
	}
	if rootID == "" || rootID == postID {
		return channelID + ":" + postID
	}
	return channelID + ":" + rootID + ":" + postID
}

// parseMessageID accepts a bare post ID or one of the encoded forms above.
func parseMessageID(id string) msgRef {
	id = strings.TrimSpace(id)
	if id == "" {
		return msgRef{}
	}
	parts := strings.Split(id, ":")
	switch len(parts) {
	case 1:
		return msgRef{postID: parts[0]}
	case 2:
		return msgRef{channelID: parts[0], postID: parts[1]}
	case 3:
		return msgRef{channelID: parts[0], rootID: parts[1], postID: parts[2]}
	default:
		return msgRef{}
	}
}

// Reply placement is one mechanism for DMs and channels; only the default
// differs. By default a reply goes where defaultReplyRoot says (DM: mirror
// the user, channel/group: the post's thread). The agent chooses otherwise by
// sending through the message tool: reply_to_message_id=<id> posts into that
// message's thread, no reply_to_message_id posts at the top level (the tool
// sends a bare outbound context); it then answers NO_REPLY.

// defaultReplyRoot is the thread root a reply to ref goes to by default. In a
// DM the bot mirrors the user: a top-level post gets a top-level reply ("")
// and a post inside a thread is answered in that thread. In channels and
// groups replies go into the post's thread.
func defaultReplyRoot(chatType string, ref msgRef) string {
	if isDirectChat(chatType) {
		return ref.rootID
	}
	return ref.threadRoot()
}

// replyPlacement describes a default reply root for the agent's prompt.
func replyPlacement(replyRoot string) string {
	if replyRoot == "" {
		return bus.ReplyPlacementTopLevel
	}
	return bus.ReplyPlacementThread
}

func isDirectChat(chatType string) bool {
	return strings.EqualFold(strings.TrimSpace(chatType), "direct")
}

// resolveTarget picks the channel and thread root for an outbound message.
// Priority: an explicit "<channel>/<root>" chat ID, then the explicit reply
// target (always threads under it), then the per-turn inbound context: the
// thread it was replying in, else the default placement for the message being
// answered (defaultReplyRoot). References that point into another channel are
// ignored. With no reply target the post is top-level.
func resolveTarget(chatID string, outCtx *bus.InboundContext, replyTo string) (channelID, rootID string) {
	if strings.TrimSpace(chatID) == "" && outCtx != nil {
		chatID = outCtx.ChatID
	}
	channelID, rootID, _ = strings.Cut(strings.TrimSpace(chatID), "/")
	if rootID != "" || channelID == "" {
		return channelID, rootID
	}
	inChannel := func(ref msgRef) bool {
		return ref.postID != "" && (ref.channelID == "" || ref.channelID == channelID)
	}
	candidates := []string{replyTo}
	if outCtx != nil {
		candidates = append(candidates, outCtx.ReplyToMessageID)
	}
	for _, candidate := range candidates {
		if ref := parseMessageID(candidate); inChannel(ref) {
			return channelID, ref.threadRoot()
		}
	}
	if outCtx != nil {
		if ref := parseMessageID(outCtx.MessageID); inChannel(ref) {
			return channelID, defaultReplyRoot(outCtx.ChatType, ref)
		}
	}
	return channelID, ""
}

// CanonicalMessageRef implements channels.MessageRefCanonicalizer:
// "<channel>:<post>" for "<channel>:<post>", "<channel>:<root>:<post>" or a
// bare post ID in chatID's channel.
func (c *MattermostChannel) CanonicalMessageRef(chatID, messageID string) string {
	ref := parseMessageID(messageID)
	if ref.postID == "" {
		return ""
	}
	channelID := ref.channelID
	if channelID == "" {
		channelID, _, _ = strings.Cut(strings.TrimSpace(chatID), "/")
	}
	if channelID == "" {
		return ""
	}
	return channelID + ":" + ref.postID
}

func isFinalOutbound(ctx bus.InboundContext) bool {
	return strings.EqualFold(strings.TrimSpace(ctx.Raw[outboundKindKey]), outboundKindFinal)
}

func threadKey(channelID, rootID string) string { return channelID + "/" + rootID }

// --- per-thread "working on it" indicators ----------------------------------

// threadIndicators is the pending UX (typing, 👀, placeholder) for a thread
// the bot is about to answer. It is tracked per thread inside the channel
// (not by the Manager, which keys on chat ID = channel) so that concurrent
// posts in different threads of one channel never consume each other's
// placeholder.
type threadIndicators struct {
	stopTyping     func()
	undoReaction   func()
	reactionPostID string // post carrying the automatic 👀
	placeholderID  string
	created        time.Time
}

func (t *threadIndicators) stop() {
	if t == nil {
		return
	}
	if t.stopTyping != nil {
		t.stopTyping()
	}
	if t.undoReaction != nil {
		t.undoReaction()
	}
}

// beginIndicators shows the configured indicators for an addressed post,
// once per thread until a reply consumes them.
func (c *MattermostChannel) beginIndicators(ctx context.Context, channelID, rootID, postID string) {
	c.sweepFeedback(ctx)
	key := threadKey(channelID, rootID)
	c.indMu.Lock()
	for k, ind := range c.indicators {
		// A turn may run longer than indicatorTTL (one long tool call):
		// its typing and placeholder stay until the reply takes them.
		if time.Since(ind.created) > indicatorTTL && !c.threads.turnActive(k) {
			ind.stop()
			delete(c.indicators, k)
		}
	}
	_, exists := c.indicators[key]
	c.indMu.Unlock()
	if exists {
		return
	}

	ind := &threadIndicators{created: time.Now()}
	if c.bc.Typing.Enabled {
		ind.stopTyping = c.startTyping(channelID, rootID)
	}
	if postID != "" && c.botUserID != "" && !c.config.DisableInboundReaction {
		if err := c.addReaction(ctx, postID, reactionEmoji); err == nil {
			ind.reactionPostID = postID
			var once sync.Once
			ind.undoReaction = func() {
				once.Do(func() {
					undoCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_ = c.removeReaction(undoCtx, postID, reactionEmoji)
				})
			}
		}
	}
	if c.bc.Placeholder.Enabled {
		if id, err := c.createPost(ctx, channelID, rootID, c.bc.Placeholder.GetRandomText(), nil); err == nil {
			ind.placeholderID = id
		} else {
			logger.DebugCF("mattermost", "placeholder failed", map[string]any{"error": err.Error()})
		}
	}

	c.indMu.Lock()
	c.indicators[key] = ind
	c.indMu.Unlock()
}

// keepReaction is called when the agent itself reacts to postID with emoji
// name: if that is the automatic 👀 still pending on the post, it is no
// longer removed when the reply goes out (or the reply is NO_REPLY).
func (c *MattermostChannel) keepReaction(postID, name string) {
	if name != reactionEmoji {
		return
	}
	c.indMu.Lock()
	defer c.indMu.Unlock()
	for _, ind := range c.indicators {
		if ind.reactionPostID == postID {
			ind.undoReaction = nil
		}
	}
}

// takeIndicators removes the thread's pending indicators, stops typing and
// the 👀 reaction, and returns them so the caller can reuse the placeholder.
func (c *MattermostChannel) takeIndicators(channelID, rootID string) threadIndicators {
	key := threadKey(channelID, rootID)
	c.indMu.Lock()
	ind, ok := c.indicators[key]
	delete(c.indicators, key)
	c.indMu.Unlock()
	if !ok {
		return threadIndicators{}
	}
	ind.stop()
	return *ind
}

// --- cross-thread turn ordering ---------------------------------------------

// threadQueue approximates, per channel, which thread the agent's running
// turn belongs to. All threads of a channel share one session, and the agent
// steers a message that arrives during a running turn into that turn. That
// is right for a follow-up in the same thread, but an addressed post from a
// *different* thread must get its own turn (and its own threaded reply), so
// it is published with DeferWhileBusy. Deferral only has an effect while the
// session is busy, so a stale entry is harmless.
type threadQueue struct {
	mu        sync.Mutex
	byChannel map[string][]queuedThread
	// active marks threads with an unfinished turn: set when an addressed
	// post of the thread is handed to the agent or the turn posts tool
	// feedback there, cleared when the turn ends (done).
	active map[string]time.Time
}

type queuedThread struct {
	root string
	at   time.Time
}

// admit records a post in thread root and reports whether it must be
// deferred. Non-addressed posts are never deferred: they are steered into a
// running turn, so a burst of chatter is handled by one turn.
func (q *threadQueue) admit(channelID, root string, addressed bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.byChannel == nil {
		q.byChannel = make(map[string][]queuedThread)
	}
	now := time.Now()
	if q.active == nil {
		q.active = make(map[string]time.Time)
	}
	for key, at := range q.active {
		if now.Sub(at) > activeTurnMaxAge {
			delete(q.active, key)
		}
	}
	if addressed {
		// Only an addressed post (or an edit) is sure to get a turn of its
		// own thread, ended by done. Chatter steered into another thread's
		// turn never sees done for its own thread; such a turn's thread is
		// marked by its tool feedback instead (markActive).
		q.active[threadKey(channelID, root)] = now
	}
	list := q.byChannel[channelID]
	for len(list) > 0 && now.Sub(list[0].at) > threadQueueTTL {
		list = list[1:]
	}
	if len(list) == 0 {
		q.byChannel[channelID] = []queuedThread{{root: root, at: now}}
		return false
	}
	if list[0].root == root {
		list[0].at = now
		q.byChannel[channelID] = list
		return false
	}
	if !addressed {
		q.byChannel[channelID] = list
		return false
	}
	q.byChannel[channelID] = append(list, queuedThread{root: root, at: now})
	return true
}

// done marks the turn for thread root as finished. Turns of one session run
// one at a time in queue order, so entries before it are finished too.
func (q *threadQueue) done(channelID, root string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.active, threadKey(channelID, root))
	list := q.byChannel[channelID]
	for i, t := range list {
		if t.root == root {
			for _, earlier := range list[:i] {
				delete(q.active, threadKey(channelID, earlier.root))
			}
			if rest := list[i+1:]; len(rest) > 0 {
				q.byChannel[channelID] = rest
			} else {
				delete(q.byChannel, channelID)
			}
			return
		}
	}
}

// markActive marks the thread with key "<channel>/<root>" as having a
// running turn (the turn posted tool feedback there).
func (q *threadQueue) markActive(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.active == nil {
		q.active = make(map[string]time.Time)
	}
	q.active[key] = time.Now()
}

// turnActive reports whether the thread with key "<channel>/<root>" has an
// unfinished turn (see activeTurnMaxAge).
func (q *threadQueue) turnActive(key string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	at, ok := q.active[key]
	return ok && time.Since(at) < activeTurnMaxAge
}

// --- sender profiles ----------------------------------------------------------

type userInfo struct {
	username    string
	displayName string
}

// lookupUser returns the username and display name of a user (cached), used
// for the agent's "[from:Name (@user)]" annotations in group conversations.
func (c *MattermostChannel) lookupUser(ctx context.Context, userID, fallbackUsername string) userInfo {
	if v, ok := c.users.Load(userID); ok {
		if info, ok := v.(userInfo); ok {
			return info
		}
	}
	info := userInfo{username: strings.TrimPrefix(fallbackUsername, "@")}
	var u struct {
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Nickname  string `json:"nickname"`
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.apiJSON(reqCtx, http.MethodGet, "/api/v4/users/"+url.PathEscape(userID), nil, &u); err != nil {
		return info // not cached: retried on the next post
	}
	if u.Username != "" {
		info.username = u.Username
	}
	info.displayName = strings.TrimSpace(u.Nickname)
	if info.displayName == "" {
		info.displayName = strings.TrimSpace(u.FirstName + " " + u.LastName)
	}
	c.users.Store(userID, info)
	return info
}
