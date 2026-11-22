package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"

	ta "github.com/mymmrac/telego/telegoapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

// DeleteMessage accepts the opaque refs the agent sees ("chat:msg",
// "chat:topic:msg"), a bare ID with chatID, and rejects refs into another chat.
func TestDeleteMessage_OpaqueIDs(t *testing.T) {
	caller := &stubCaller{
		callFn: func(context.Context, string, *ta.RequestData) (*ta.Response, error) {
			return &ta.Response{Ok: true, Result: []byte("true")}, nil
		},
	}
	ch := newTestChannel(t, caller)
	ctx := context.Background()

	for _, tc := range []struct{ chatID, messageID string }{
		{"12345", "12345:7"},
		{"12345/9", "12345:9:8"},
		{"12345", "6"},
	} {
		require.NoError(t, ch.DeleteMessage(ctx, tc.chatID, tc.messageID), "%+v", tc)
	}
	require.Len(t, caller.calls, 3)
	for i, want := range []string{`"message_id":7`, `"message_id":8`, `"message_id":6`} {
		assert.Contains(t, caller.calls[i].URL, "deleteMessage")
		body := string(caller.calls[i].Data.BodyRaw)
		assert.Contains(t, body, want)
		assert.Contains(t, body, `"chat_id":12345`)
	}

	err := ch.DeleteMessage(ctx, "12345", "999:7")
	require.Error(t, err)
	assert.True(t, errors.Is(err, channels.ErrNotOwnMessage), "err = %v", err)
	assert.Len(t, caller.calls, 3, "a ref into another chat must not reach the API")
}

// The streamer reports the delivered final as the opaque ref the agent sees,
// not a bare message ID (history would show msgs:#5).
func TestStreamer_DeliveredMessageIDsAreOpaque(t *testing.T) {
	caller := &stubCaller{
		callFn: func(_ context.Context, url string, _ *ta.RequestData) (*ta.Response, error) {
			if strings.Contains(url, "sendMessage") && !strings.Contains(url, "Draft") {
				return successResponseWithMessageID(t, 5), nil
			}
			return &ta.Response{Ok: true, Result: []byte("true")}, nil
		},
	}
	ch := newTestChannel(t, caller)
	ch.tgCfg.Streaming = config.StreamingConfig{Enabled: true}

	for chatID, want := range map[string]string{"12345": "12345:5", "12345/42": "12345:42:5"} {
		st, err := ch.BeginStream(context.Background(), chatID)
		require.NoError(t, err)
		require.NoError(t, st.Finalize(context.Background(), "hello"))
		receipt, ok := st.(interface{ DeliveredMessageIDs() []string })
		require.True(t, ok)
		assert.Equal(t, []string{want}, receipt.DeliveredMessageIDs(), "chat %s", chatID)
	}
}

// Review 2, fix 1: with the real Telegram channel behind the Manager, editing
// the bot's message -100:77 while handling group -200 must not make message
// 77 in -200 (someone else's, bot is admin) deletable.
func TestManagerEditCrossChatThenDeleteRefused(t *testing.T) {
	caller := &stubCaller{
		callFn: func(context.Context, string, *ta.RequestData) (*ta.Response, error) {
			return successResponseWithMessageID(t, 77), nil
		},
	}
	ch := newTestChannel(t, caller)
	cfg := &config.Config{Channels: config.ChannelsConfig{"telegram": &config.Channel{Type: "telegram"}}}
	m, err := channels.NewManager(cfg, bus.NewMessageBus(), nil)
	require.NoError(t, err)
	m.RegisterChannel("telegram", ch)
	ctx := context.Background()

	require.NoError(t, m.EditMessage(ctx, "telegram", "-200", "-100:77", "fixed"))
	require.Len(t, caller.calls, 1)
	assert.Contains(t, caller.calls[0].URL, "editMessage")
	assert.Contains(t, string(caller.calls[0].Data.BodyRaw), `"chat_id":-100`)

	for _, id := range []string{"77", "-200:77"} {
		err := m.DeleteMessage(ctx, "telegram", "-200", id)
		assert.True(t, errors.Is(err, channels.ErrNotOwnMessage), "delete %q: err = %v", id, err)
	}
	for _, call := range caller.calls {
		assert.NotContains(t, call.URL, "deleteMessage", "nothing may be deleted")
	}
}

func TestCanonicalMessageRef(t *testing.T) {
	ch := newTestChannel(t, &stubCaller{})
	for _, tc := range []struct{ chatID, ref, want string }{
		{"-100", "-100:7", "-100:7"},
		{"-100/42", "-100:42:7", "-100:7"},
		{"-100/42", "7", "-100:7"},
		{"-200", "-100:7", "-100:7"},
		{"-100", "x", ""},
	} {
		assert.Equal(t, tc.want, ch.CanonicalMessageRef(tc.chatID, tc.ref), "%+v", tc)
	}
}
