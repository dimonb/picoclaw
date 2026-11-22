package bus

import "strings"

// SilentReplyToken is the final answer an agent gives when it decides not to
// reply (e.g. group chatter that does not concern it). Nothing is delivered
// for such a turn; it stays in session history so the model sees it kept
// quiet.
const SilentReplyToken = "NO_REPLY"

// MessageKindSilent marks (in OutboundMessage.Context.Raw["message_kind"]) the
// content-less outbound published for a silent turn. Channels never deliver
// it; the Manager uses it to clean up typing, reactions, placeholders and
// tool feedback shown for the turn.
const MessageKindSilent = "silent"

// silentReplyTrim are wrappers / trailing characters tolerated around the
// token ("NO_REPLY.", "`NO_REPLY`", "**NO_REPLY**").
const silentReplyTrim = " \t\r\n.!。…*`\"'"

// IsSilentReply reports whether text is the silent-reply token, tolerating
// surrounding whitespace, markdown wrappers and trailing punctuation.
func IsSilentReply(text string) bool {
	return strings.Trim(text, silentReplyTrim) == SilentReplyToken
}

// IsSilentReplyPrefix reports whether streamed text could still turn out to
// be the silent-reply token, so streaming must hold it back.
func IsSilentReplyPrefix(text string) bool {
	trimmed := strings.TrimLeft(text, silentReplyTrim)
	if trimmed == "" {
		return true
	}
	core := strings.TrimRight(trimmed, silentReplyTrim)
	if core == SilentReplyToken {
		return true
	}
	return core == trimmed && strings.HasPrefix(SilentReplyToken, core)
}
