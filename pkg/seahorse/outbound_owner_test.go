package seahorse

import (
	"context"
	"testing"
)

func TestHasOutboundDelivery(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, d := range []OutboundDelivery{
		{Channel: "mattermost", ChatID: "chan1", MessageID: "chan1:root1:post1", Content: "a"},
		{Channel: "telegram", ChatID: "-100/42", MessageID: "-100:42:7", Content: "b"},
	} {
		if err := s.RecordOutboundDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		channel, chat, ref string
		want               bool
	}{
		{"mattermost", "chan1", "chan1:root1:post1", true},
		{"mattermost", "chan1", "chan1:post1", true}, // same post, short form
		{"mattermost", "chan2", "chan1:root1:post1", false},
		{"mattermost", "chan1", "chan1:post2", false},
		{"telegram", "-100", "-100:42:7", true},
		{"telegram", "-100/42", "-100:7", true},
		{"telegram", "-200", "-100:42:7", false},
		{"slack", "chan1", "chan1:post1", false},
	} {
		got, err := s.HasOutboundDelivery(ctx, tc.channel, tc.chat, tc.ref)
		if err != nil || got != tc.want {
			t.Errorf("%+v: got %v err %v", tc, got, err)
		}
	}
}
