package seahorse

import (
	"context"
	"sort"
	"testing"
)

func TestOutboundDeliveryRefs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, d := range []OutboundDelivery{
		{Channel: "mattermost", ChatID: "chan1", MessageID: "chan1:root1:post1", SessionKey: "s", Content: "a"},
		{Channel: "telegram", ChatID: "-100/42", MessageID: "-100:42:7", SessionKey: "s", Content: "b"},
		{Channel: "telegram", ChatID: "-200", MessageID: "-200:7", SessionKey: "s", Content: "c"},
		// Recorded by an edit in older versions (no session): never "own".
		{Channel: "telegram", ChatID: "-100", MessageID: "-300:7", Content: "edited"},
		{Channel: "matrix", ChatID: "!r:example.org", MessageID: "$mine:example.org", SessionKey: "s"},
	} {
		if err := s.RecordOutboundDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		channel, chat, suffix string
		want                  []string
	}{
		{"mattermost", "chan1", "post1", []string{"chan1:root1:post1"}},
		{"mattermost", "chan2", "post1", nil},
		{"telegram", "-100", "7", []string{"-100:42:7"}}, // topic row matches the chat; edit row skipped
		{"telegram", "-200", "7", []string{"-200:7"}},
		{"matrix", "!r:example.org", "$mine:example.org", []string{"$mine:example.org"}},
		{"matrix", "!r:example.org", "$theirs:example.org", nil},
	} {
		got, err := s.OutboundDeliveryRefs(ctx, tc.channel, tc.chat, tc.suffix, 0)
		sort.Strings(got)
		if err != nil || len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
			t.Errorf("%+v: got %v err %v", tc, got, err)
		}
	}
}
