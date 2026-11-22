package agent

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// groupChatGuidanceFile, when present in the agent workspace, replaces
// defaultGroupChatGuidance verbatim (tune group etiquette without a rebuild).
const groupChatGuidanceFile = "GROUP_CHAT.md"

// defaultGroupChatGuidance is the group-chat etiquette added to the dynamic
// (non-cached) context for group/channel conversations. Keep it compact and
// stable: it is identical for every message of a chat apart from the
// "addressed" line appended by groupChatNote.
const defaultGroupChatGuidance = `## Group Chat
You are one member of a group chat with several people. Not every message is meant for you; the sender of each message is shown in its [from:...] annotation. Behave like a thoughtful human colleague in the chat:
- Speak when you are addressed (mentioned, replied to, called by name, asked directly), when the topic is about you or your work (something you did, said or are responsible for), when someone asks the room something you can genuinely answer, or when you can fix an important mistake or misunderstanding.
- Stay quiet for small talk between others, things already answered, conversations that don't involve you, and when you would only add agreement, "+1" or filler. Don't jump into every message and don't repeat what someone already said.
- When you speak, be short, conversational and to the point; no long essays unless asked. Give one reply for a burst of messages, addressing people by name when helpful.
- If a lightweight acknowledgement fits better than a message and the reaction tool is available, react instead and stay quiet.
- Keep following a conversation you joined: follow-ups in the same thread or topic are probably addressed to you.
To stay quiet, answer with exactly ` + bus.SilentReplyToken + ` and nothing else.`

func isGroupChatType(chatType string) bool {
	switch strings.ToLower(strings.TrimSpace(chatType)) {
	case "group", "channel":
		return true
	default:
		return false
	}
}

// groupChatNote returns the group-chat section of the dynamic context, or ""
// for direct chats.
func (cb *ContextBuilder) groupChatNote(req PromptBuildRequest) string {
	if !isGroupChatType(req.ChatType) {
		return ""
	}
	guidance := defaultGroupChatGuidance
	if cb != nil && cb.workspace != "" {
		if data, err := os.ReadFile(filepath.Join(cb.workspace, groupChatGuidanceFile)); err == nil {
			if custom := strings.TrimSpace(string(data)); custom != "" {
				guidance = custom
			}
		}
	}
	addressed := "no"
	if req.Mentioned {
		addressed = "yes"
	}
	return guidance + "\nThe current message is addressed to you (mention or reply to you): " + addressed
}
