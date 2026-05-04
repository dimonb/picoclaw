package matrix

import (
	"testing"

	"maunium.net/go/mautrix/id"
)

func TestParseMatrixOpaqueID(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantRoom  id.RoomID
		wantEvent id.EventID
		wantErr   bool
	}{
		{
			name:      "round-trip room and event",
			input:     "!abc:example.org $evt-1:example.org",
			wantRoom:  id.RoomID("!abc:example.org"),
			wantEvent: id.EventID("$evt-1:example.org"),
		},
		{
			name:      "event id with sigil dollar only",
			input:     "!room:server $eventid",
			wantRoom:  id.RoomID("!room:server"),
			wantEvent: id.EventID("$eventid"),
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "missing event part",
			input:   "!abc:example.org",
			wantErr: true,
		},
		{
			name:    "three segments split by space",
			input:   "!abc:example.org $evt:example.org extra",
			wantErr: true,
		},
		{
			name:    "wrong delimiter (colon-only)",
			input:   "!abc:example.org:$evt:example.org",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roomID, eventID, err := parseMatrixOpaqueID(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got room=%q event=%q",
						tt.input, roomID, eventID)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if roomID != tt.wantRoom {
				t.Errorf("roomID = %q, want %q", roomID, tt.wantRoom)
			}
			if eventID != tt.wantEvent {
				t.Errorf("eventID = %q, want %q", eventID, tt.wantEvent)
			}
		})
	}
}
