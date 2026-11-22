package mattermost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/identity"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/utils"
)

const (
	wsPingInterval = 30 * time.Second
	wsPongWait     = 75 * time.Second
	wsWriteWait    = 10 * time.Second
)

// wsEvent is a frame received on the Mattermost WebSocket: either a server
// event (Event set) or a reply to one of our actions (SeqReply set).
type wsEvent struct {
	Event    string          `json:"event"`
	Data     json.RawMessage `json:"data"`
	Seq      int64           `json:"seq"`
	Status   string          `json:"status"`
	SeqReply int64           `json:"seq_reply"`
	Error    json.RawMessage `json:"error"`
}

// postedData is the payload of a "posted" event. Note that post and
// mentions are JSON documents encoded as strings.
type postedData struct {
	ChannelType string `json:"channel_type"`
	ChannelName string `json:"channel_name"`
	Post        string `json:"post"`
	Mentions    string `json:"mentions"`
	SenderName  string `json:"sender_name"`
	TeamID      string `json:"team_id"`
}

type mmPost struct {
	ID        string         `json:"id"`
	CreateAt  int64          `json:"create_at"`
	EditAt    int64          `json:"edit_at"`
	UserID    string         `json:"user_id"`
	ChannelID string         `json:"channel_id"`
	RootID    string         `json:"root_id"`
	Message   string         `json:"message"`
	Type      string         `json:"type"`
	Props     map[string]any `json:"props"`
	FileIDs   []string       `json:"file_ids"`
	Metadata  struct {
		Files []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"files"`
	} `json:"metadata"`
}

// runWebSocket keeps a WebSocket session alive, reconnecting with
// exponential backoff until the channel context is canceled.
func (c *MattermostChannel) runWebSocket() {
	defer c.wg.Done()
	var (
		connectionID string
		lastSeq      int64 = -1
	)
	backoff := c.reconnectMin
	for {
		if c.ctx.Err() != nil {
			return
		}
		established, err := c.serveConnection(&connectionID, &lastSeq)
		if c.ctx.Err() != nil {
			return
		}
		if established {
			backoff = c.reconnectMin
		}
		logger.WarnCF("mattermost", "WebSocket disconnected, reconnecting", map[string]any{
			"error":   fmt.Sprint(err),
			"backoff": backoff.String(),
		})
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, c.reconnectMax)
	}
}

func (c *MattermostChannel) websocketURL(connectionID string, nextSeq int64) string {
	u, _ := url.Parse(c.baseURL + "/api/v4/websocket")
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	if connectionID != "" {
		// Lets the server replay events missed while we were disconnected.
		q := u.Query()
		q.Set("connection_id", connectionID)
		q.Set("sequence_number", strconv.FormatInt(nextSeq, 10))
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// serveConnection dials, authenticates and reads one WebSocket session until
// it fails. established reports whether the server accepted the session
// (sent "hello"), which resets the reconnect backoff.
func (c *MattermostChannel) serveConnection(connectionID *string, lastSeq *int64) (established bool, err error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.config.BotToken.String())
	conn, resp, err := c.dialer.DialContext(c.ctx, c.websocketURL(*connectionID, *lastSeq+1), header)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	c.connMu.Lock()
	if c.ctx.Err() != nil {
		c.connMu.Unlock()
		conn.Close()
		return false, c.ctx.Err()
	}
	c.conn = conn
	c.connMu.Unlock()
	defer c.closeConn()

	// Header auth is normally enough; the challenge covers proxies that strip
	// the Authorization header on upgrade (ignored once authenticated).
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	if err := conn.WriteJSON(map[string]any{
		"seq":    c.seq.Add(1),
		"action": "authentication_challenge",
		"data":   map[string]string{"token": c.config.BotToken.String()},
	}); err != nil {
		return false, fmt.Errorf("auth challenge: %w", err)
	}

	extend := func() { _ = conn.SetReadDeadline(time.Now().Add(wsPongWait)) }
	extend()
	conn.SetPongHandler(func(string) error { extend(); return nil })
	conn.SetPingHandler(func(data string) error {
		extend()
		err := conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(wsWriteWait))
		if err == websocket.ErrCloseSent {
			return nil
		}
		return err
	})

	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)); err != nil {
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return established, fmt.Errorf("read: %w", err)
		}
		extend()

		var ev wsEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			logger.DebugCF("mattermost", "Ignoring malformed WebSocket frame", map[string]any{"error": err.Error()})
			continue
		}
		if ev.Event == "" {
			if ev.Status != "" && ev.Status != "OK" {
				logger.WarnCF("mattermost", "WebSocket action failed", map[string]any{
					"seq_reply": ev.SeqReply, "status": ev.Status, "error": string(ev.Error),
				})
			}
			continue
		}

		if ev.Event == "hello" {
			var hello struct {
				ConnectionID string `json:"connection_id"`
			}
			_ = json.Unmarshal(ev.Data, &hello)
			if hello.ConnectionID != *connectionID {
				logger.InfoCF("mattermost", "WebSocket connected", map[string]any{
					"connection_id": hello.ConnectionID,
					"resumed":       false,
				})
			}
			*connectionID = hello.ConnectionID
			*lastSeq = ev.Seq
			established = true
			continue
		}
		if *lastSeq >= 0 && ev.Seq != *lastSeq+1 {
			logger.DebugCF("mattermost", "WebSocket sequence gap", map[string]any{
				"expected": *lastSeq + 1, "got": ev.Seq,
			})
		}
		*lastSeq = ev.Seq

		if ev.Event != "posted" && ev.Event != "post_edited" {
			continue
		}
		select {
		case c.events <- ev:
		case <-c.ctx.Done():
			return established, c.ctx.Err()
		}
	}
}

func (c *MattermostChannel) closeConn() {
	c.connMu.Lock()
	conn := c.conn
	c.conn = nil
	c.connMu.Unlock()
	if conn == nil {
		return
	}
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	_ = conn.Close()
}

// eventWorker handles inbound posts sequentially (preserving order) so slow
// downstream work never stalls the WebSocket reader.
func (c *MattermostChannel) eventWorker() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case ev := <-c.events:
			if ev.Event == "post_edited" {
				c.handlePostEdited(ev.Data)
			} else {
				c.handlePosted(ev.Data)
			}
		}
	}
}

func (c *MattermostChannel) handlePosted(raw json.RawMessage) {
	var data postedData
	if err := json.Unmarshal(raw, &data); err != nil {
		return
	}
	var post mmPost
	if err := json.Unmarshal([]byte(data.Post), &post); err != nil || post.ID == "" || post.ChannelID == "" {
		return
	}
	c.rememberChannel(post.ChannelID, data)
	c.handlePost(data, post, false)
}

// handlePost processes a new post or (edited=true) a user's edit of one.
func (c *MattermostChannel) handlePost(data postedData, post mmPost, edited bool) {
	// Own posts, system messages (join/leave/header changes…) and other bots
	// (avoids bot-to-bot loops) are never processed.
	if post.UserID == "" || post.UserID == c.botUserID || post.Type != "" || isTruthy(post.Props["from_bot"]) {
		return
	}

	isDM := data.ChannelType == "D"
	if !isDM && c.teamID != "" && data.TeamID != "" && data.TeamID != c.teamID {
		return
	}

	sender := bus.SenderInfo{
		Platform:    config.ChannelMattermost,
		PlatformID:  post.UserID,
		CanonicalID: identity.BuildCanonicalID(config.ChannelMattermost, post.UserID),
	}
	if !c.IsAllowedSender(sender) {
		logger.DebugCF("mattermost", "Message rejected by allowlist", map[string]any{"user_id": post.UserID})
		return
	}

	mentioned := len(c.findMentions(post.Message)) > 0
	if !mentioned && mentionsContain(data.Mentions, c.botUserID) {
		// @channel/@all/@here put every member (the bot included) into
		// mentions; only a direct mention should wake the bot.
		mentioned = !channelWideMentionRe.MatchString(post.Message)
	}
	content := c.stripBotMention(post.Message)

	// An edit reaches the agent only as a correction of a post it handled
	// (text changed) or as a new request for a post it saw but did not
	// handle (see edits.go); the filters below still apply to the latter.
	// Posts the bot does not remember (old, or from before a restart) are
	// ignored. A correction shows nothing up front; the agent decides.
	seen := false
	if edited {
		switch c.seenPosts.classifyEdit(post.ID, post.Message, mentioned) {
		case editCorrection:
			seen = true
		case editNewRequest:
		default:
			return
		}
	} else {
		c.seenPosts.observe(post.ID, post.Message, mentioned)
	}

	// DMs are always addressed. In channels a post is addressed when it
	// mentions the bot, follows up in a thread the bot takes part in, or
	// matches a group_trigger prefix. Mention mode drops everything else;
	// smart mode hands it to the agent, which may answer NO_REPLY.
	addressed := isDM
	if !isDM {
		addressed = mentioned || (post.RootID != "" && c.botInThread(c.ctx, post.ChannelID, post.RootID))
		if !addressed && len(c.bc.GroupTrigger.Prefixes) > 0 {
			if ok, cleaned := c.ShouldRespondInGroup(false, content); ok {
				addressed, content = true, cleaned
			}
		}
		if !addressed && !seen && (c.groupMode == groupModeMention || c.bc.GroupTrigger.MentionOnly) {
			return
		}
	}

	// One session per channel: the chat ID is the plain channel ID and the
	// thread travels with the message (see threads.go).
	chatID := post.ChannelID
	rootID := post.RootID
	if rootID == "" {
		rootID = post.ID
	}

	var mediaPaths []string
	if edited {
		if strings.TrimSpace(content) == "" {
			return
		}
		content = editedPrefix + content
	} else {
		var fileNote string
		mediaPaths, fileNote = c.downloadFiles(post, chatID)
		content += fileNote
		if strings.TrimSpace(content) == "" {
			return
		}
	}
	c.seenPosts.markDelivered(post.ID, post.Message, mentioned)

	chatType := "channel"
	switch data.ChannelType {
	case "D":
		chatType = "direct"
	case "G":
		chatType = "group"
	}
	// Where the reply goes by default (DM top-level post: top level, ""),
	// which is also where indicators show and the turn is tracked.
	replyRoot := defaultReplyRoot(chatType, msgRef{channelID: post.ChannelID, rootID: post.RootID, postID: post.ID})

	user := c.lookupUser(c.ctx, post.UserID, data.SenderName)
	sender.Username = user.username
	sender.DisplayName = user.displayName

	inboundCtx := bus.InboundContext{
		Channel:   c.Name(),
		ChatID:    chatID,
		ChatType:  chatType,
		SenderID:  post.UserID,
		MessageID: encodeMessageID(post.ChannelID, post.RootID, post.ID),
		Mentioned: mentioned || (!isDM && addressed),
		Raw: map[string]string{
			"platform":    config.ChannelMattermost,
			"post_id":     post.ID,
			"root_id":     post.RootID,
			"thread_root": rootID,
			"reply_root":  replyRoot,
			// Lets the agent describe the thread/top-level reply choice.
			bus.InboundRawReplyPlacement: replyPlacement(replyRoot),
			"channel_id":                 post.ChannelID,
			"channel_type":               data.ChannelType,
			"channel_name":               data.ChannelName,
			"sender_name":                user.username,
			"team_id":                    data.TeamID,
		},
	}
	if post.RootID != "" {
		inboundCtx.ReplyToMessageID = encodeMessageID(post.ChannelID, "", post.RootID)
	}
	if data.TeamID != "" {
		inboundCtx.SpaceID = data.TeamID
		inboundCtx.SpaceType = "team"
	}

	if edited {
		inboundCtx.Raw["edited"] = "true"
		addressed = addressed && !seen // a correction is never an "addressed" new request
	}
	if addressed {
		c.beginIndicators(c.ctx, post.ChannelID, replyRoot, post.ID)
	}
	// An edit is a message of its own thread: if another thread's turn is
	// running it waits for its own turn, so a reply cannot land in the wrong
	// thread (the agent may still choose NO_REPLY).
	deferWhileBusy := c.threads.admit(post.ChannelID, replyRoot, addressed || edited)

	logger.DebugCF("mattermost", "Received message", map[string]any{
		"sender_id": post.UserID,
		"chat_id":   chatID,
		"thread":    replyRoot,
		"deferred":  deferWhileBusy,
		"preview":   utils.Truncate(content, 50),
	})
	c.HandleInboundContextWithOptions(c.ctx, chatID, content, mediaPaths, inboundCtx,
		channels.InboundOptions{DeferWhileBusy: deferWhileBusy}, sender)
}

// channelWideMentionRe detects Mattermost's channel-wide mention keywords.
var channelWideMentionRe = regexp.MustCompile(`(?i)(^|[^a-z0-9_.\-])@(channel|all|here)\b`)

func isTruthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	}
	return false
}

// mentionsContain reports whether the event's "mentions" field (a JSON array
// encoded as a string) contains userID.
func mentionsContain(mentions, userID string) bool {
	if mentions == "" || userID == "" {
		return false
	}
	var ids []string
	if err := json.Unmarshal([]byte(mentions), &ids); err != nil {
		return false
	}
	for _, id := range ids {
		if id == userID {
			return true
		}
	}
	return false
}

func isUsernameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '.' || b == '_' || b == '-'
}

// findMentions returns byte ranges of "@botname" occurrences that are real
// mentions: not part of an e-mail address or of a longer username
// ("@bot-two", "@bot.smith"), while allowing trailing punctuation ("@bot.").
func (c *MattermostChannel) findMentions(text string) [][]int {
	if c.mentionRe == nil {
		return nil
	}
	matches := c.mentionRe.FindAllStringIndex(text, -1)
	out := make([][]int, 0, len(matches))
	for _, loc := range matches {
		start, end := loc[0], loc[1]
		if start > 0 && isUsernameByte(text[start-1]) {
			continue
		}
		if end < len(text) {
			next := text[end]
			if next == '.' {
				if end+1 < len(text) && isUsernameByte(text[end+1]) && text[end+1] != '.' {
					continue
				}
			} else if isUsernameByte(next) {
				continue
			}
		}
		out = append(out, loc)
	}
	return out
}

func (c *MattermostChannel) stripBotMention(text string) string {
	locs := c.findMentions(text)
	if len(locs) == 0 {
		return strings.TrimSpace(text)
	}
	var b strings.Builder
	prev := 0
	for _, loc := range locs {
		b.WriteString(text[prev:loc[0]])
		prev = loc[1]
	}
	b.WriteString(text[prev:])
	return strings.TrimSpace(b.String())
}

// downloadFiles fetches post attachments into the media store and returns
// their refs plus a "[file: name]" note per file for the agent.
func (c *MattermostChannel) downloadFiles(post mmPost, chatID string) ([]string, string) {
	type fileRef struct{ id, name string }
	files := make([]fileRef, 0, max(len(post.Metadata.Files), len(post.FileIDs)))
	for _, f := range post.Metadata.Files {
		files = append(files, fileRef{f.ID, f.Name})
	}
	if len(files) == 0 {
		for _, id := range post.FileIDs {
			files = append(files, fileRef{id, id})
		}
	}
	if len(files) == 0 {
		return nil, ""
	}

	scope := channels.BuildMediaScope(c.Name(), chatID, post.ID)
	refs := make([]string, 0, len(files))
	var note strings.Builder
	for _, f := range files {
		if f.id == "" {
			continue
		}
		localPath := utils.DownloadFile(c.baseURL+"/api/v4/files/"+url.PathEscape(f.id), f.name,
			utils.DownloadOptions{
				LoggerPrefix: "mattermost",
				ExtraHeaders: map[string]string{"Authorization": "Bearer " + c.config.BotToken.String()},
			})
		if localPath == "" {
			continue
		}
		ref := localPath
		if store := c.GetMediaStore(); store != nil {
			if r, err := store.Store(localPath, media.MediaMeta{
				Filename:       f.name,
				Source:         config.ChannelMattermost,
				CleanupPolicy:  media.CleanupPolicyDeleteOnCleanup,
				RetentionClass: media.RetentionClassPermanent,
			}, scope); err == nil {
				ref = r
			}
		}
		refs = append(refs, ref)
		fmt.Fprintf(&note, "\n[file: %s]", f.name)
	}
	return refs, note.String()
}
