package providers

import "strings"

const (
	MessageMetaSourceKind  = "source_kind"
	MessageMetaChannel     = "channel"
	MessageMetaChatID      = "chat_id"
	MessageMetaSenderID    = "sender_id"
	MessageMetaPeerKind    = "peer_kind"
	MessageMetaPeerID      = "peer_id"
	MessageMetaTriggerKind = "trigger_kind"
	MessageMetaTriggerID   = "trigger_id"
	MessageMetaDispatch    = "dispatch_mode"
)

const (
	MessageSourceChannel   = "channel"
	MessageSourceCron      = "cron"
	MessageSourceSystem    = "system"
	MessageSourceAssistant = "assistant"
)

const (
	MessageTriggerCron = "cron"
	DispatchModeAgent  = "agent"
	DispatchModeDirect = "direct"
)

func CloneMessageMetadata(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(src))
	for key, value := range src {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		cloned[key] = value
	}
	if len(cloned) == 0 {
		return nil
	}
	return cloned
}
