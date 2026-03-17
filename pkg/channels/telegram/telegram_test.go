package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/mymmrac/telego"
	ta "github.com/mymmrac/telego/telegoapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
)

const testToken = "1234567890:aaaabbbbaaaabbbbaaaabbbbaaaabbbbccc"

// stubCaller implements ta.Caller for testing.
type stubCaller struct {
	calls  []stubCall
	callFn func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error)
}

type stubCall struct {
	URL  string
	Data *ta.RequestData
}

func (s *stubCaller) Call(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
	s.calls = append(s.calls, stubCall{URL: url, Data: data})
	return s.callFn(ctx, url, data)
}

// stubConstructor implements ta.RequestConstructor for testing.
type stubConstructor struct{}

func (s *stubConstructor) JSONRequest(parameters any) (*ta.RequestData, error) {
	body, err := json.Marshal(parameters)
	if err != nil {
		return nil, err
	}
	return &ta.RequestData{
		ContentType: ta.ContentTypeJSON,
		BodyRaw:     body,
	}, nil
}

func (s *stubConstructor) MultipartRequest(
	parameters map[string]string,
	files map[string]ta.NamedReader,
) (*ta.RequestData, error) {
	body, err := json.Marshal(parameters)
	if err != nil {
		return nil, err
	}
	return &ta.RequestData{
		ContentType: ta.ContentTypeJSON,
		BodyRaw:     body,
	}, nil
}

// successResponse returns a ta.Response that telego will treat as a successful SendMessage.
func successResponse(t *testing.T) *ta.Response {
	return successResponseWithID(t, 1)
}

func successResponseWithID(t *testing.T, id int) *ta.Response {
	t.Helper()
	msg := &telego.Message{MessageID: id}
	b, err := json.Marshal(msg)
	require.NoError(t, err)
	return &ta.Response{Ok: true, Result: b}
}

func successBoolResponse(t *testing.T) *ta.Response {
	t.Helper()
	return &ta.Response{Ok: true, Result: []byte("true")}
}

// newTestChannel creates a TelegramChannel with a mocked bot for unit testing.
func newTestChannel(t *testing.T, caller *stubCaller) *TelegramChannel {
	t.Helper()

	bot, err := telego.NewBot(testToken,
		telego.WithAPICaller(caller),
		telego.WithRequestConstructor(&stubConstructor{}),
		telego.WithDiscardLogger(),
	)
	require.NoError(t, err)

	base := channels.NewBaseChannel("telegram", nil, nil, nil,
		channels.WithMaxMessageLength(4000),
	)
	base.SetRunning(true)

	return &TelegramChannel{
		BaseChannel: base,
		bot:         bot,
		chatIDs:     make(map[string]int64),
	}
}

func decodeCallBody(t *testing.T, call stubCall) map[string]any {
	t.Helper()

	var body map[string]any
	require.NoError(t, json.Unmarshal(call.Data.BodyRaw, &body))
	return body
}

func TestSend_Wrapper(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "12345",
		Content: "Hello, world!",
	})

	assert.NoError(t, err)
	assert.Len(t, caller.calls, 1, "wrapper should call inner function")
}

func TestSendMessageWithID_EmptyContent(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			t.Fatal("SendMessage should not be called for empty content")
			return nil, nil
		},
	}
	ch := newTestChannel(t, caller)

	msgID, err := ch.SendMessageWithID(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: ""})

	assert.NoError(t, err)
	assert.Empty(t, msgID)
	assert.Empty(t, caller.calls, "no API calls should be made for empty content")
}

func TestSendMessageWithID_ShortMessage_SingleCall(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)

	msgID, err := ch.SendMessageWithID(context.Background(), bus.OutboundMessage{
		ChatID:  "12345",
		Content: "Hello, world!",
	})

	assert.NoError(t, err)
	assert.Equal(t, "1", msgID)
	assert.Len(t, caller.calls, 1, "short message should result in exactly one SendMessage call")
}

func TestSendMessageWithID_ForumTopic_UsesThreadID(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)

	msgID, err := ch.SendMessageWithID(context.Background(), bus.OutboundMessage{
		ChatID:  "-1001234567890/42",
		Content: "Hello, topic!",
	})

	assert.NoError(t, err)
	assert.Equal(t, "1", msgID)
	require.Len(t, caller.calls, 1)
	body := decodeCallBody(t, caller.calls[0])
	assert.Equal(t, float64(42), body["message_thread_id"])
}

func TestSendMessageWithID_ReplyToMessage_UsesReplyParameters(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)

	msgID, err := ch.SendMessageWithID(context.Background(), bus.OutboundMessage{
		ChatID:           "12345",
		Content:          "Hello, thread!",
		ReplyToMessageID: "99",
	})

	assert.NoError(t, err)
	assert.Equal(t, "1", msgID)
	require.Len(t, caller.calls, 1)
	body := decodeCallBody(t, caller.calls[0])
	replyParams, ok := body["reply_parameters"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(99), replyParams["message_id"])
	assert.Equal(t, true, replyParams["allow_sending_without_reply"])
}

func TestSendMessageWithID_GeneralTopic_UsesThreadID(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)

	msgID, err := ch.SendMessageWithID(context.Background(), bus.OutboundMessage{
		ChatID:  "-1001234567890/1",
		Content: "Hello, general!",
	})

	assert.NoError(t, err)
	assert.Equal(t, "1", msgID)
	require.Len(t, caller.calls, 1)
	body := decodeCallBody(t, caller.calls[0])
	assert.Equal(t, float64(1), body["message_thread_id"])
}

func TestSendMessageWithID_LongMessage_SingleCall(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)

	longContent := strings.Repeat("a", 4000)

	msgID, err := ch.SendMessageWithID(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: longContent})

	assert.NoError(t, err)
	assert.Equal(t, "1", msgID)
	assert.Len(t, caller.calls, 1, "pre-split message within limit should result in one SendMessage call")
}

func TestSendMessageWithIDs_ReturnsAllChunkIDsAfterHTMLResplit(t *testing.T) {
	caller := &stubCaller{}
	caller.callFn = func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
		return successResponseWithID(t, len(caller.calls)), nil
	}
	ch := newTestChannel(t, caller)

	chunk := "[x](https://example.com/" + strings.Repeat("a", 20) + ") "
	content := strings.Repeat(chunk, 120)

	ids, err := ch.SendMessageWithIDs(context.Background(), bus.OutboundMessage{
		ChatID:  "12345",
		Content: content,
	})

	require.NoError(t, err)
	require.Len(t, ids, 2)
	assert.Equal(t, []string{"1", "2"}, ids)
	assert.Len(t, caller.calls, 2)
}

func TestSendMessageWithID_HTMLFallback_PerChunk(t *testing.T) {
	callCount := 0
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			callCount++
			if callCount%2 == 1 {
				return nil, errors.New("Bad Request: can't parse entities")
			}
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)

	msgID, err := ch.SendMessageWithID(
		context.Background(),
		bus.OutboundMessage{ChatID: "12345", Content: "Hello **world**"},
	)

	assert.NoError(t, err)
	assert.Equal(t, "1", msgID)
	assert.Equal(t, 2, len(caller.calls), "should have HTML attempt + plain text fallback")
}

func TestSendMessageWithID_HTMLFallback_BothFail(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return nil, errors.New("send failed")
		},
	}
	ch := newTestChannel(t, caller)

	msgID, err := ch.SendMessageWithID(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: "Hello"})

	assert.Error(t, err)
	assert.Empty(t, msgID)
	assert.True(t, errors.Is(err, channels.ErrTemporary), "error should wrap ErrTemporary")
	assert.Equal(t, 2, len(caller.calls), "should have HTML attempt + plain text attempt")
}

func TestSendMessageWithID_LongMessage_HTMLFallback_StopsOnError(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return nil, errors.New("send failed")
		},
	}
	ch := newTestChannel(t, caller)

	longContent := strings.Repeat("x", 4001)

	msgID, err := ch.SendMessageWithID(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: longContent})

	assert.Error(t, err)
	assert.Empty(t, msgID)
	assert.Equal(t, 2, len(caller.calls), "should stop after first chunk fails both HTML and plain text")
}

func TestSendMessageWithID_MarkdownShortButHTMLLong_MultipleCalls(t *testing.T) {
	callCount := 0
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			callCount++
			return successResponseWithID(t, callCount), nil
		},
	}
	ch := newTestChannel(t, caller)

	markdownContent := strings.Repeat("**a** ", 600)
	assert.LessOrEqual(t, len([]rune(markdownContent)), 4000)

	msgID, err := ch.SendMessageWithID(
		context.Background(),
		bus.OutboundMessage{ChatID: "12345", Content: markdownContent},
	)

	assert.NoError(t, err)
	assert.Greater(
		t,
		len(caller.calls),
		1,
		"markdown-short but HTML-long message should be split into multiple SendMessage calls",
	)
	assert.Equal(t, "1,2", msgID)
}

func TestEditMessage_MultipleChunkIDs(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)

	content := strings.Repeat("**a** ", 600)

	err := ch.EditMessage(context.Background(), "12345", "1,2", content)

	assert.NoError(t, err)
	assert.Len(t, caller.calls, 2, "multi-part edit should update every tracked message")
}

func TestSetMessageReaction_SendsConfiguredEmoji(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successBoolResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)

	err := ch.SetMessageReaction(context.Background(), "12345", "99", "❤️")

	require.NoError(t, err)
	require.Len(t, caller.calls, 1)
	body := decodeCallBody(t, caller.calls[0])
	assert.Equal(t, float64(99), body["message_id"])
	reaction, ok := body["reaction"].([]any)
	require.True(t, ok)
	require.Len(t, reaction, 1)
	first, ok := reaction[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "emoji", first["type"])
	assert.Equal(t, "❤️", first["emoji"])
}

func TestStartTyping_ForumTopic_UsesThreadID(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)

	stop, err := ch.StartTyping(context.Background(), "-1001234567890/42")
	require.NoError(t, err)
	stop()

	require.NotEmpty(t, caller.calls)
	body := decodeCallBody(t, caller.calls[0])
	assert.Equal(t, float64(42), body["message_thread_id"])
}

func TestStartTyping_GeneralTopic_UsesThreadID(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)

	stop, err := ch.StartTyping(context.Background(), "-1001234567890/1")
	require.NoError(t, err)
	stop()

	require.NotEmpty(t, caller.calls)
	body := decodeCallBody(t, caller.calls[0])
	assert.Equal(t, float64(1), body["message_thread_id"])
}

func TestSendPlaceholder_GroupSkipsPlaceholder(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)
	ch.config = config.DefaultConfig()
	ch.config.Channels.Telegram.Placeholder.Enabled = true
	ch.config.Channels.Telegram.Placeholder.Text = "Thinking"

	msgID, err := ch.SendPlaceholder(context.Background(), "-1001234567890/42")
	require.NoError(t, err)
	assert.Empty(t, msgID)
	assert.Empty(t, caller.calls)
}

func TestSendPlaceholder_PrivateChatSendsMessage(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)
	ch.config = config.DefaultConfig()
	ch.config.Channels.Telegram.Placeholder.Enabled = true
	ch.config.Channels.Telegram.Placeholder.Text = "Thinking"

	msgID, err := ch.SendPlaceholder(context.Background(), "12345")
	require.NoError(t, err)
	assert.Equal(t, "1", msgID)
	require.Len(t, caller.calls, 1)
	body := decodeCallBody(t, caller.calls[0])
	assert.Equal(t, "Thinking", body["text"])
}

func TestSendMedia_ForumTopic_UsesThreadID(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)

	store := media.NewFileMediaStore()
	ch.SetMediaStore(store)

	tmpFile, err := os.CreateTemp(t.TempDir(), "telegram-media-*.jpg")
	require.NoError(t, err)
	_, err = tmpFile.WriteString("hello")
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	ref, err := store.Store(tmpFile.Name(), media.MediaMeta{Filename: "photo.jpg"}, "test-scope")
	require.NoError(t, err)

	err = ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: "-1001234567890/42",
		Parts: []bus.MediaPart{{
			Type: "image",
			Ref:  ref,
		}},
	})
	require.NoError(t, err)
	require.Len(t, caller.calls, 1)
	body := decodeCallBody(t, caller.calls[0])
	assert.Equal(t, "42", fmt.Sprint(body["message_thread_id"]))
}

func TestSendMessageWithID_NotRunning(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			t.Fatal("should not be called")
			return nil, nil
		},
	}
	ch := newTestChannel(t, caller)
	ch.SetRunning(false)

	msgID, err := ch.SendMessageWithID(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: "Hello"})

	assert.ErrorIs(t, err, channels.ErrNotRunning)
	assert.Empty(t, msgID)
	assert.Empty(t, caller.calls)
}

func TestSendMessageWithID_InvalidChatID(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			t.Fatal("should not be called")
			return nil, nil
		},
	}
	ch := newTestChannel(t, caller)

	msgID, err := ch.SendMessageWithID(
		context.Background(),
		bus.OutboundMessage{ChatID: "not-a-number", Content: "Hello"},
	)

	assert.Error(t, err)
	assert.Empty(t, msgID)
	assert.True(t, errors.Is(err, channels.ErrSendFailed), "error should wrap ErrSendFailed")
	assert.Empty(t, caller.calls)
}

func TestParseTelegramChatID_Plain(t *testing.T) {
	cid, tid, err := parseTelegramChatID("12345")
	assert.NoError(t, err)
	assert.Equal(t, int64(12345), cid)
	assert.Equal(t, 0, tid)
}

func TestParseTelegramChatID_NegativeGroup(t *testing.T) {
	cid, tid, err := parseTelegramChatID("-1001234567890")
	assert.NoError(t, err)
	assert.Equal(t, int64(-1001234567890), cid)
	assert.Equal(t, 0, tid)
}

func TestParseTelegramChatID_WithThreadID(t *testing.T) {
	cid, tid, err := parseTelegramChatID("-1001234567890/42")
	assert.NoError(t, err)
	assert.Equal(t, int64(-1001234567890), cid)
	assert.Equal(t, 42, tid)
}

func TestParseTelegramChatID_GeneralTopic(t *testing.T) {
	cid, tid, err := parseTelegramChatID("-100123/1")
	assert.NoError(t, err)
	assert.Equal(t, int64(-100123), cid)
	assert.Equal(t, 1, tid)
}

func TestParseTelegramChatID_Invalid(t *testing.T) {
	_, _, err := parseTelegramChatID("not-a-number")
	assert.Error(t, err)
}

func TestParseTelegramChatID_InvalidThreadID(t *testing.T) {
	_, _, err := parseTelegramChatID("-100123/not-a-thread")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid thread ID")
}

func TestSend_WithForumThreadID(t *testing.T) {
	caller := &stubCaller{
		callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
			return successResponse(t), nil
		},
	}
	ch := newTestChannel(t, caller)

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "-1001234567890/42",
		Content: "Hello from topic",
	})

	assert.NoError(t, err)
	assert.Len(t, caller.calls, 1)
}

func TestHandleMessage_ForumTopic_SetsMetadata(t *testing.T) {
	messageBus := bus.NewMessageBus()
	ch := &TelegramChannel{
		BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
		chatIDs:     make(map[string]int64),
		ctx:         context.Background(),
	}

	msg := &telego.Message{
		Text:            "hello from topic",
		MessageID:       10,
		MessageThreadID: 42,
		Chat: telego.Chat{
			ID:      -1001234567890,
			Type:    "supergroup",
			IsForum: true,
		},
		From: &telego.User{
			ID:        7,
			FirstName: "Alice",
		},
	}

	err := ch.handleMessage(context.Background(), msg)
	require.NoError(t, err)

	inbound, ok := <-messageBus.InboundChan()
	require.True(t, ok, "expected inbound message")

	// Composite chatID should include thread ID
	assert.Equal(t, "-1001234567890/42", inbound.ChatID)

	// Peer ID should include thread ID for session key isolation
	assert.Equal(t, "group", inbound.Peer.Kind)
	assert.Equal(t, "-1001234567890/42", inbound.Peer.ID)

	// Parent peer metadata should be set for agent binding
	assert.Equal(t, "topic", inbound.Metadata["parent_peer_kind"])
	assert.Equal(t, "42", inbound.Metadata["parent_peer_id"])
}

func TestHandleMessage_NoForum_NoThreadMetadata(t *testing.T) {
	messageBus := bus.NewMessageBus()
	ch := &TelegramChannel{
		BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
		chatIDs:     make(map[string]int64),
		ctx:         context.Background(),
	}

	msg := &telego.Message{
		Text:      "regular group message",
		MessageID: 11,
		Chat: telego.Chat{
			ID:   -100999,
			Type: "group",
		},
		From: &telego.User{
			ID:        8,
			FirstName: "Bob",
		},
	}

	err := ch.handleMessage(context.Background(), msg)
	require.NoError(t, err)

	inbound, ok := <-messageBus.InboundChan()
	require.True(t, ok)

	// Plain chatID without thread suffix
	assert.Equal(t, "-100999", inbound.ChatID)

	// Peer ID should be raw chat ID (no thread suffix)
	assert.Equal(t, "group", inbound.Peer.Kind)
	assert.Equal(t, "-100999", inbound.Peer.ID)

	// No parent peer metadata
	assert.Empty(t, inbound.Metadata["parent_peer_kind"])
	assert.Empty(t, inbound.Metadata["parent_peer_id"])
}

func TestHandleMessage_ReplyThread_NonForum_NoIsolation(t *testing.T) {
	messageBus := bus.NewMessageBus()
	ch := &TelegramChannel{
		BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
		chatIDs:     make(map[string]int64),
		ctx:         context.Background(),
	}

	// In regular groups, reply threads set MessageThreadID to the original
	// message ID. This should NOT trigger per-thread session isolation.
	msg := &telego.Message{
		Text:            "reply in thread",
		MessageID:       20,
		MessageThreadID: 15,
		Chat: telego.Chat{
			ID:      -100999,
			Type:    "supergroup",
			IsForum: false,
		},
		From: &telego.User{
			ID:        9,
			FirstName: "Carol",
		},
	}

	err := ch.handleMessage(context.Background(), msg)
	require.NoError(t, err)

	inbound, ok := <-messageBus.InboundChan()
	require.True(t, ok)

	// chatID should NOT include thread suffix for non-forum groups
	assert.Equal(t, "-100999", inbound.ChatID)

	// Peer ID should be raw chat ID (shared session for whole group)
	assert.Equal(t, "group", inbound.Peer.Kind)
	assert.Equal(t, "-100999", inbound.Peer.ID)

	// No parent peer metadata
	assert.Empty(t, inbound.Metadata["parent_peer_kind"])
	assert.Empty(t, inbound.Metadata["parent_peer_id"])
}
