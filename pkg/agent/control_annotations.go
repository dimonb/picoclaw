package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

type deliveryDirective struct {
	HasDirective      bool
	Directive         string
	ReplyToMessageID  string
	Reactions         []providers.MessageReaction
	SuppressTextReply bool
}

// controlAnnotationJSON is marshaled into <meta>{...}</meta> blocks that the
// system writes into message history to provide thread/context to the LLM.
type controlAnnotationJSON struct {
	MsgIDs    []string         `json:"msg_ids,omitempty"`
	ReplyTo   string           `json:"reply_to,omitempty"`
	Reactions []metaReactEntry `json:"reactions,omitempty"`
	Trigger   string           `json:"trigger,omitempty"`
	SourceID  string           `json:"source_id,omitempty"`
	Username  string           `json:"username,omitempty"`
	Name      string           `json:"name,omitempty"`
}

// metaReactEntry represents one reaction in a <meta> block.
type metaReactEntry struct {
	Target string `json:"target"`
	Emoji  string `json:"emoji"`
}

// buildMetaControlAnnotation returns a "<meta>{...}</meta> " prefix for a
// message, or "" if there is nothing to annotate.
func buildMetaControlAnnotation(msg providers.Message) string {
	a := controlAnnotationJSON{}

	for _, id := range msg.MessageIDs {
		if id != "" {
			a.MsgIDs = append(a.MsgIDs, id)
		}
	}

	if msg.ReplyToMessageID != "" {
		a.ReplyTo = msg.ReplyToMessageID
	}

	if msg.Sender != nil {
		a.Username = msg.Sender.Username
		a.Name = strings.TrimSpace(msg.Sender.FirstName + " " + msg.Sender.LastName)
	}

	for _, r := range msg.Reactions {
		if r.TargetMessageID != "" && r.Emoji != "" {
			a.Reactions = append(a.Reactions, metaReactEntry{Target: r.TargetMessageID, Emoji: r.Emoji})
		}
	}

	if triggerKind := strings.TrimSpace(msg.Metadata[providers.MessageMetaTriggerKind]); triggerKind != "" {
		a.Trigger = triggerKind
		a.SourceID = strings.TrimSpace(msg.Metadata[providers.MessageMetaTriggerID])
	}

	// Check if there's anything to annotate.
	isEmpty := len(a.MsgIDs) == 0 &&
		a.ReplyTo == "" && len(a.Reactions) == 0 &&
		a.Trigger == "" && a.SourceID == "" &&
		a.Username == "" && a.Name == ""
	if isEmpty {
		return ""
	}

	jsonBytes, err := json.Marshal(a)
	if err != nil {
		logger.DebugCF("agent", "Failed to marshal control annotation", map[string]any{"error": err.Error()})
		return ""
	}
	return "<meta>" + string(jsonBytes) + "</meta> "
}

// consumeLeadingMetaBlock extracts a leading <meta>...</meta> block from
// content. Returns the raw JSON string inside the tag, the remainder of the
// content after the closing tag, and ok=true on success.
func consumeLeadingMetaBlock(content string) (jsonStr, rest string, ok bool) {
	trimmed := strings.TrimLeft(content, " \t\r\n")
	if !strings.HasPrefix(trimmed, "<meta>") {
		return "", content, false
	}
	end := strings.Index(trimmed, "</meta>")
	if end == -1 {
		logger.DebugCF("agent", "Found <meta> without closing </meta>", map[string]any{
			"prefix": trimmed[:min(len(trimmed), 120)],
		})
		return "", content, false
	}
	jsonStr = trimmed[len("<meta>"):end]
	rest = trimmed[end+len("</meta>"):]
	return jsonStr, rest, true
}

// parseDeliveryDirective looks for a leading <meta>{...}</meta> block in
// rawContent. If found and the JSON is valid, the block is stripped and any
// known delivery fields (reply_to, react_to, text_reply) are applied.
// Unknown JSON fields are silently ignored.
func parseDeliveryDirective(rawContent string) (directive deliveryDirective, body string, err error) {
	jsonStr, rest, ok := consumeLeadingMetaBlock(rawContent)
	if !ok {
		return directive, strings.TrimLeft(rawContent, " \t\r\n"), nil
	}

	var delivery struct {
		ReplyTo   string           `json:"reply_to"`
		ReactTo   []metaReactEntry `json:"react_to"`
		TextReply *bool            `json:"text_reply"`
	}
	if jsonErr := json.Unmarshal([]byte(jsonStr), &delivery); jsonErr != nil {
		logger.DebugCF("agent", "Ignoring leading <meta> block with invalid JSON", map[string]any{
			"error": jsonErr.Error(),
			"json":  jsonStr,
		})
		//nolint:nilerr // invalid JSON in <meta> is not a hard error; treat as no annotation
		return directive, strings.TrimLeft(rawContent, " \t\r\n"), nil
	}

	directive = deliveryDirective{
		HasDirective: true,
		Directive:    "<meta>" + jsonStr + "</meta>",
	}
	body = strings.TrimLeft(rest, " \t\r\n")

	if id := strings.TrimSpace(delivery.ReplyTo); id != "" {
		directive.ReplyToMessageID = id
	}

	for _, r := range delivery.ReactTo {
		if strings.TrimSpace(r.Target) == "" || strings.TrimSpace(r.Emoji) == "" {
			return directive, body, fmt.Errorf("invalid react_to entry: target=%q emoji=%q", r.Target, r.Emoji)
		}
		directive.Reactions = append(directive.Reactions, providers.MessageReaction{
			TargetMessageID: strings.TrimSpace(r.Target),
			Emoji:           strings.TrimSpace(r.Emoji),
		})
	}

	if delivery.TextReply != nil {
		directive.SuppressTextReply = !*delivery.TextReply
	}

	return directive, body, nil
}
