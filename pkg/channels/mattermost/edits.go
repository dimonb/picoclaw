package mattermost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Edited user posts (WebSocket "post_edited"). Minimal behavior: an edit is
// passed to the agent as a new message with the same message ID, prefixed
// with editedPrefix, when
//   - the bot handled the original post recently (postMemoryTTL) and the text
//     actually changed (Mattermost also emits post_edited for metadata such as
//     link previews), or
//   - the edit adds an @mention of the bot to a recent channel post.
//
// Nothing is shown up front for a correction (no 👀/typing/placeholder); the
// agent decides whether the change matters and otherwise answers NO_REPLY.
// The edit stays in the session history either way.

const (
	editedPrefix  = "[edited earlier message; respond only if the change matters] "
	postMemoryTTL = 24 * time.Hour
	postMemoryMax = 5000
)

type rememberedPost struct {
	text string
	at   time.Time
}

// postMemory remembers the text of posts delivered to the agent.
type postMemory struct {
	mu    sync.Mutex
	posts map[string]rememberedPost
}

func (m *postMemory) put(id, text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.posts == nil {
		m.posts = make(map[string]rememberedPost)
	}
	now := time.Now()
	if len(m.posts) >= postMemoryMax {
		for k, p := range m.posts {
			if now.Sub(p.at) > postMemoryTTL || len(m.posts) >= postMemoryMax {
				delete(m.posts, k)
			}
		}
	}
	m.posts[id] = rememberedPost{text: text, at: now}
}

func (m *postMemory) get(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.posts[id]
	if !ok || time.Since(p.at) > postMemoryTTL {
		return "", false
	}
	return p.text, true
}

func (c *MattermostChannel) rememberPostText(post mmPost) { c.seenPosts.put(post.ID, post.Message) }

// editedPostSeen reports whether the bot handled post before and whether its
// text differs from what the bot saw. An unseen post only counts when it was
// really edited (edit_at) and is recent.
func (c *MattermostChannel) editedPostSeen(post mmPost) (seen, changed bool) {
	if text, ok := c.seenPosts.get(post.ID); ok {
		return true, text != post.Message
	}
	recent := post.CreateAt > 0 && time.Since(time.UnixMilli(post.CreateAt)) < postMemoryTTL
	return false, post.EditAt > 0 && recent
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
	if err := c.apiJSON(reqCtx, http.MethodGet, "/api/v4/channels/"+url.PathEscape(channelID), nil, &info); err != nil ||
		info.Type == "" {
		return mmChannelInfo{}, false
	}
	c.chanInfo.Store(channelID, info)
	return info, true
}
