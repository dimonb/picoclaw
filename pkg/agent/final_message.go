package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
)

type finalMetaAction struct {
	ReplyTo       string `json:"reply_to,omitempty"`
	EditMessageID string `json:"edit_message_id,omitempty"`
	SendFinal     *bool  `json:"send_final,omitempty"`
	Reaction      *struct {
		MessageID string `json:"message_id,omitempty"`
		Emoji     string `json:"emoji,omitempty"`
	} `json:"reaction,omitempty"`
}

var leadingMessageAnnotationPattern = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*`)

// parseFinalMeta only recognizes a leading <meta>...</meta> control block.
// Mid-string tags are ignored by design because the contract is "prefix your
// final answer with a single <meta> block"; this avoids accidentally parsing
// literal examples inside normal assistant text.
func parseFinalMeta(raw string) (finalMetaAction, string, error) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "<meta>") {
		return finalMetaAction{}, raw, nil
	}
	end := strings.Index(trimmed, "</meta>")
	if end < 0 {
		return finalMetaAction{}, raw, fmt.Errorf("missing </meta> closing tag")
	}
	jsonPart := strings.TrimSpace(trimmed[len("<meta>"):end])
	body := strings.TrimSpace(trimmed[end+len("</meta>"):])
	if jsonPart == "" {
		return finalMetaAction{}, body, fmt.Errorf("empty meta json")
	}
	var meta finalMetaAction
	if err := json.Unmarshal([]byte(jsonPart), &meta); err != nil {
		return finalMetaAction{}, raw, err
	}
	return meta, body, nil
}

func resolveFinalResponse(rawContent string) agentResponse {
	meta, body, err := parseFinalMeta(rawContent)
	if err != nil {
		logger.WarnCF("agent", "Ignoring invalid <meta> block in final response", map[string]any{"error": err.Error()})
		content, stripped := stripLeadingMessageAnnotations(rawContent)
		logStrippedMessageAnnotations(stripped)
		return agentResponse{Content: content}
	}
	content, stripped := stripLeadingMessageAnnotations(body)
	logStrippedMessageAnnotations(stripped)
	response := agentResponse{Content: content}
	if meta.ReplyTo != "" {
		response.ReplyToMessageID = strings.TrimSpace(meta.ReplyTo)
	}
	if meta.EditMessageID != "" {
		response.EditMessageID = strings.TrimSpace(meta.EditMessageID)
	}
	if meta.Reaction != nil && strings.TrimSpace(meta.Reaction.MessageID) != "" &&
		strings.TrimSpace(meta.Reaction.Emoji) != "" {
		response.Reaction = &finalReactionAction{
			MessageID: strings.TrimSpace(meta.Reaction.MessageID),
			Emoji:     strings.TrimSpace(meta.Reaction.Emoji),
		}
	}
	if meta.SendFinal != nil {
		response.SkipFinalSend = !*meta.SendFinal
	}
	if response.EditMessageID != "" {
		response.ReplyToMessageID = ""
	}
	if response.SkipFinalSend {
		response.Content = ""
	}
	if response.ReplyToMessageID != "" || response.EditMessageID != "" || response.Reaction != nil ||
		response.SkipFinalSend {
		fields := map[string]any{
			"reply_to_message_id": response.ReplyToMessageID,
			"edit_message_id":     response.EditMessageID,
			"send_final":          !response.SkipFinalSend,
			"content_len":         len(response.Content),
		}
		if response.Reaction != nil {
			fields["reaction_message_id"] = response.Reaction.MessageID
			fields["reaction_emoji"] = response.Reaction.Emoji
		}
		logger.DebugCF("agent", "Parsed final <meta> delivery control", fields)
	}
	return response
}

func stripLeadingMessageAnnotations(text string) (string, string) {
	var stripped strings.Builder

	for {
		match := leadingMessageAnnotationPattern.FindStringSubmatchIndex(text)
		if match == nil || match[0] != 0 {
			return text, stripped.String()
		}

		body := strings.ToLower(text[match[2]:match[3]])
		if !strings.Contains(body, "msgs:#") &&
			!strings.Contains(body, "reply_to:#") &&
			!strings.HasPrefix(body, "from:") {
			return text, stripped.String()
		}

		stripped.WriteString(text[match[0]:match[1]])
		text = text[match[1]:]
	}
}

func logStrippedMessageAnnotations(stripped string) {
	if stripped == "" {
		return
	}

	logger.DebugCF("agent", "Stripped leading message annotations from final response",
		map[string]any{
			"stripped": stripped,
		})
}
