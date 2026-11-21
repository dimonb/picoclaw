package telegram

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ta "github.com/mymmrac/telego/telegoapi"
	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/media"
)

func TestNativeReceiptInternalSplitPartialFailure(t *testing.T) {
	calls := 0
	ch := newTestChannel(
		t,
		&stubCaller{callFn: func(_ context.Context, _ string, _ *ta.RequestData) (*ta.Response, error) {
			calls++
			if calls == 1 {
				return successResponseWithMessageID(t, 71), nil
			}
			return nil, errors.New("failed")
		}},
	)
	var got []bus.OutboundDelivery
	ctx := bus.WithOutboundReceiptObserver(context.Background(), func(d bus.OutboundDelivery) { got = append(got, d) })
	source := strings.Repeat("<&> ", 1800)
	ids, err := ch.Send(ctx, bus.OutboundMessage{ChatID: "12345", Content: source})
	require.Error(t, err)
	require.Len(t, ids, 1)
	require.Len(t, got, 1)
	require.Equal(t, "71", got[0].MessageID)
	require.NotEmpty(t, got[0].Content)
	require.Less(t, len(got[0].Content), len(source))
	require.Contains(t, source, strings.TrimSpace(got[0].Content))
}

func TestNativeReceiptParsedAndPlainFallback(t *testing.T) {
	for _, format := range []telegramFormat{telegramFormatHTML, telegramFormatMarkdownV2, telegramFormatRich} {
		for _, fallback := range []bool{false, true} {
			calls := 0
			ch := newTestChannel(
				t,
				&stubCaller{callFn: func(_ context.Context, _ string, _ *ta.RequestData) (*ta.Response, error) {
					calls++
					if fallback && calls == 1 {
						return nil, errors.New("parse failed")
					}
					return successResponseWithMessageID(t, 72), nil
				}},
			)
			var got []bus.OutboundDelivery
			ctx := bus.WithOutboundReceiptObserver(
				context.Background(),
				func(d bus.OutboundDelivery) { got = append(got, d) },
			)
			_, err := ch.sendChunk(
				ctx,
				sendChunkParams{chatID: 12345, content: "rendered", mdFallback: "**source**", format: format},
			)
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, "**source**", got[0].Content)
		}
	}
}

func TestNativeReceiptMediaSkippedPartialAndGroupCaptions(t *testing.T) {
	for _, group := range []bool{false, true} {
		calls := 0
		ch := newTestChannelWithConstructor(
			t,
			&stubCaller{callFn: func(_ context.Context, url string, _ *ta.RequestData) (*ta.Response, error) {
				calls++
				if strings.Contains(url, "sendMessage") {
					return successResponseWithMessageID(t, 80), nil
				}
				if group {
					return successMediaGroupResponse(t, 81, 82), nil
				}
				if calls == 1 {
					return successResponseWithMessageID(t, 81), nil
				}
				return nil, errors.New("second file failed")
			}},
			&multipartRecordingConstructor{},
		)
		store := media.NewFileMediaStore()
		ch.SetMediaStore(store)
		path := filepath.Join(t.TempDir(), "image.png")
		require.NoError(t, os.WriteFile(path, []byte("image"), 0o600))
		ref, err := store.Store(path, media.MediaMeta{Filename: "image.png"}, "scope")
		require.NoError(t, err)
		parts := []bus.MediaPart{
			{Type: "file", Ref: "missing"},
			{Type: "file", Ref: ref, Caption: "visible"},
			{Type: "file", Ref: ref, Caption: "not sent"},
		}
		if group {
			parts = []bus.MediaPart{
				{Type: "image", Ref: ref, Caption: strings.Repeat("caption ", 160)},
				{Type: "image", Ref: ref, Caption: "not shown"},
			}
		}
		var got []bus.OutboundDelivery
		ctx := bus.WithOutboundReceiptObserver(
			context.Background(),
			func(d bus.OutboundDelivery) { got = append(got, d) },
		)
		_, err = ch.SendMedia(ctx, bus.OutboundMediaMessage{ChatID: "12345", Parts: parts})
		if group {
			require.NoError(t, err)
			require.Len(t, got, 3)
			require.NotEmpty(t, got[0].Content)
			require.Empty(t, got[0].Parts)
			require.Len(t, got[1].Parts, 1)
			require.Len(t, got[2].Parts, 1)
			require.Empty(t, got[1].Parts[0].Caption)
			require.Empty(t, got[2].Parts[0].Caption)
		} else {
			require.Error(t, err)
			require.Len(t, got, 1)
			require.Equal(t, "visible", got[0].Parts[0].Caption)
			require.Equal(t, ref, got[0].Parts[0].Ref)
		}
	}
}
