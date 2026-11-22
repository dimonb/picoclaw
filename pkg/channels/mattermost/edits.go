package mattermost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Edited user posts (WebSocket "post_edited"). The channel remembers the
// posts it has seen (postMemory). An edit is passed to the agent, as a new
// message with the same message ID prefixed with editedPrefix, only when:
//   - the agent handled the post and its text actually changed (Mattermost
//     also emits post_edited for metadata such as link previews): a
//     correction, shown without indicators — the agent decides whether it
//     matters and otherwise answers NO_REPLY;
//   - the post was seen but not handled (mention mode without a mention, or
//     no content, e.g. just "@bot") and the edit changed it: a new request,
//     subject to the usual filters (mention mode still needs a mention).
//
// Posts the channel does not remember — older than postMemoryTTL, evicted,
// or from before a restart — are ignored, so editing an old post that
// mentions the bot does not wake it. The edit stays in the session history.

const (
	editedPrefix  = "[edited earlier message; respond only if the change matters] "
	postMemoryTTL = 24 * time.Hour
	postMemoryMax = 5000
)

type editKind int

const (
	editIgnore editKind = iota
	editCorrection
	editNewRequest
)

type rememberedPost struct {
	text      string
	mentioned bool // the text mentioned the bot
	delivered bool // handed to the agent
	at        time.Time
}

// postMemory remembers recently seen posts. When full, the oldest entries
// are evicted first.
type postMemory struct {
	mu    sync.Mutex
	posts map[string]*rememberedPost
	order []string // first-seen order, for oldest-first eviction
	max   int      // 0 = postMemoryMax
}

func (m *postMemory) entry(id string, create bool) *rememberedPost {
	if m.posts == nil {
		m.posts = make(map[string]*rememberedPost)
	}
	p, ok := m.posts[id]
	if ok && time.Since(p.at) > postMemoryTTL {
		if !create {
			return nil // expired: as good as unknown
		}
		*p = rememberedPost{at: time.Now()}
	}
	if ok || !create {
		return p
	}
	p = &rememberedPost{at: time.Now()}
	m.posts[id] = p
	m.order = append(m.order, id)
	limit := m.max
	if limit <= 0 {
		limit = postMemoryMax
	}
	for len(m.order) > limit {
		delete(m.posts, m.order[0])
		m.order = m.order[1:]
	}
	return p
}

// observe records a post the channel received (handled or not).
func (m *postMemory) observe(id, text string, mentioned bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.entry(id, true)
	p.text, p.mentioned = text, mentioned
}

// markDelivered records that the post (with this text) reached the agent.
func (m *postMemory) markDelivered(id, text string, mentioned bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.entry(id, true)
	p.text, p.mentioned, p.delivered = text, mentioned, true
}

// classifyEdit decides what an edit of post id (new text, mentioned) means
// and records the new text so a repeated event is ignored.
func (m *postMemory) classifyEdit(id, text string, mentioned bool) editKind {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.entry(id, false)
	if p == nil {
		return editIgnore
	}
	defer func() { p.text, p.mentioned = text, mentioned }()
	switch {
	case p.delivered && p.text != text:
		return editCorrection
	case !p.delivered && p.text != text:
		return editNewRequest
	default:
		return editIgnore
	}
}

// handlePostEdited handles a "post_edited" event. Its payload carries only
// the post, so channel type, name and team come from earlier events or the
// API.
func (c *MattermostChannel) handlePostEdited(raw json.RawMessage) {
	var ev struct {
		Post string `json:"post"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return
	}
	var post mmPost
	if err := json.Unmarshal([]byte(ev.Post), &post); err != nil || post.ID == "" || post.ChannelID == "" {
		return
	}
	if post.EditAt == 0 {
		return // not a text edit
	}
	info, ok := c.channelInfo(post.ChannelID)
	if !ok {
		return
	}
	c.handlePost(postedData{
		ChannelType: info.Type,
		ChannelName: info.Name,
		TeamID:      info.TeamID,
		Post:        ev.Post,
	}, post, true)
}

type mmChannelInfo struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	TeamID string `json:"team_id"`
}

func (c *MattermostChannel) rememberChannel(channelID string, data postedData) {
	if data.ChannelType == "" {
		return
	}
	c.chanInfo.Store(channelID, mmChannelInfo{Type: data.ChannelType, Name: data.ChannelName, TeamID: data.TeamID})
}

func (c *MattermostChannel) channelInfo(channelID string) (mmChannelInfo, bool) {
	if v, ok := c.chanInfo.Load(channelID); ok {
		if info, ok := v.(mmChannelInfo); ok {
			return info, true
		}
	}
	var info mmChannelInfo
	reqCtx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	if err := c.apiJSON(
		reqCtx,
		http.MethodGet,
		"/api/v4/channels/"+url.PathEscape(channelID),
		nil,
		&info,
	); err != nil ||
		info.Type == "" {
		return mmChannelInfo{}, false
	}
	c.chanInfo.Store(channelID, info)
	return info, true
}
