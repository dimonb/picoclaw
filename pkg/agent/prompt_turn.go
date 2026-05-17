package agent

import (
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func promptBuildRequestForTurn(
	ts *turnState,
	history []providers.Message,
	summary string,
	currentMessage string,
	media []string,
	cfg *config.Config,
) PromptBuildRequest {
	req := PromptBuildRequest{
		History:           history,
		Summary:           summary,
		CurrentMessage:    currentMessage,
		Media:             append([]string(nil), media...),
		Channel:           ts.channel,
		ChatID:            ts.chatID,
		SenderID:          ts.opts.Dispatch.SenderID(),
		SenderDisplayName: ts.opts.SenderDisplayName,
		SenderUsername:    ts.opts.SenderUsername,
		MessageID:         ts.opts.Dispatch.MessageID(),
		ReplyToMessageID:  ts.opts.Dispatch.ReplyToMessageID(),
		ActiveSkills:      activeSkillNames(ts.agent, ts.opts),
		Overlays:          promptOverlaysForOptions(ts.opts),
	}
	hasCallableTools := true
	if ts.profile.Enabled {
		hasCallableTools = turnProfileHasCallableTools(ts.profile, ts.agent.Tools.ToProviderDefs()) ||
			turnProfileNativeSearchCallable(cfg, ts.profile, ts.agent)
	}
	if turnProfileSystemPromptOff(ts.profile) {
		req.SuppressDefaultSystemPrompt = true
		req.SuppressSkillContext = true
		req.ToolUseFallback = hasCallableTools
	}
	if ts.profile.Enabled && !hasCallableTools {
		req.SuppressToolUseRule = true
	}
	if turnProfileSkillsOff(ts.profile) {
		req.SuppressSkillContext = true
	}
	if turnProfileCustomSkills(ts.profile) {
		req.AllowedSkills = append([]string(nil), ts.profile.AllowedSkills...)
	}
	if ts.profile.Enabled && ts.profile.ToolsMode == config.TurnProfileModeCustom {
		req.AllowedTools = append([]string(nil), ts.profile.AllowedTools...)
	}
	return req
}

func turnProfileNativeSearchCallable(
	cfg *config.Config,
	profile config.EffectiveTurnProfile,
	agent *AgentInstance,
) bool {
	if cfg == nil || agent == nil {
		return false
	}
	if !cfg.Tools.IsToolEnabled("web") || !cfg.Tools.Web.PreferNative {
		return false
	}
	if !turnProfileToolAllowed(profile, "web_search") {
		return false
	}
	nativeProvider, ok := agent.Provider.(providers.NativeSearchCapable)
	return ok && nativeProvider.SupportsNativeSearch()
}

func promptBuildRequestForProcessOptions(
	agent *AgentInstance,
	opts processOptions,
	history []providers.Message,
	summary string,
	currentMessage string,
	media []string,
) PromptBuildRequest {
	req := PromptBuildRequest{
		History:           history,
		Summary:           summary,
		CurrentMessage:    currentMessage,
		Media:             append([]string(nil), media...),
		Channel:           opts.Channel,
		ChatID:            opts.ChatID,
		SenderID:          opts.SenderID,
		SenderDisplayName: opts.SenderDisplayName,
		ActiveSkills:      activeSkillNames(agent, opts),
		Overlays:          promptOverlaysForOptions(opts),
	}
	profile := opts.TurnProfile
	hasCallableTools := true
	if profile.Enabled && agent != nil {
		hasCallableTools = turnProfileHasCallableTools(profile, agent.Tools.ToProviderDefs())
	}
	if turnProfileSystemPromptOff(profile) {
		req.SuppressDefaultSystemPrompt = true
		req.SuppressSkillContext = true
		req.ToolUseFallback = hasCallableTools
	}
	if profile.Enabled && !hasCallableTools {
		req.SuppressToolUseRule = true
	}
	if turnProfileSkillsOff(profile) {
		req.SuppressSkillContext = true
	}
	if turnProfileCustomSkills(profile) {
		req.AllowedSkills = append([]string(nil), profile.AllowedSkills...)
	}
	if profile.Enabled && profile.ToolsMode == config.TurnProfileModeCustom {
		req.AllowedTools = append([]string(nil), profile.AllowedTools...)
	}
	return req
}

func promptOverlaysForOptions(opts processOptions) []PromptPart {
	systemPrompt := strings.TrimSpace(opts.SystemPromptOverride)
	if systemPrompt == "" {
		return nil
	}

	return []PromptPart{
		{
			ID:      "instruction.subturn_profile",
			Layer:   PromptLayerInstruction,
			Slot:    PromptSlotWorkspace,
			Source:  PromptSource{ID: PromptSourceSubTurnProfile, Name: "subturn.profile"},
			Title:   "SubTurn System Instructions",
			Content: systemPrompt,
			Stable:  false,
			Cache:   PromptCacheNone,
		},
	}
}

func promptContentBlock(part PromptPart, cache *providers.CacheControl) providers.ContentBlock {
	if cache == nil {
		cache = cacheControlForPromptPart(part)
	}
	return providers.ContentBlock{
		Type:         "text",
		Text:         part.Content,
		CacheControl: cache,
		PromptLayer:  string(part.Layer),
		PromptSlot:   string(part.Slot),
		PromptSource: string(part.Source.ID),
	}
}

func cacheControlForPromptPart(part PromptPart) *providers.CacheControl {
	switch part.Cache {
	case PromptCacheEphemeral:
		return &providers.CacheControl{Type: "ephemeral"}
	default:
		return nil
	}
}

func promptMessageWithMetadata(
	msg providers.Message,
	layer PromptLayer,
	slot PromptSlot,
	source PromptSourceID,
) providers.Message {
	msg.PromptLayer = string(layer)
	msg.PromptSlot = string(slot)
	msg.PromptSource = string(source)
	return msg
}

func promptMessageWithDefaultMetadata(
	msg providers.Message,
	layer PromptLayer,
	slot PromptSlot,
	source PromptSourceID,
) providers.Message {
	if strings.TrimSpace(msg.PromptSource) != "" {
		return msg
	}
	return promptMessageWithMetadata(msg, layer, slot, source)
}

// formatMessageEnvelope prefixes msg.Content with a bracketed annotation
// like `[from:Dmitrii Balabanov (@dimonb); msgs:#2403, reply_to:#1979] `
// when msg carries a channel-native MessageID or non-empty Metadata. Used
// at prompt-build time to surface inbound facts (sender identity, message
// IDs, reply threading) to the LLM. Storage layers (seahorse, session
// JSONL) keep Content raw; the envelope is a render-time concern applied
// uniformly to history and the current user message.
//
// The system prompt instructs the model to treat this annotation as
// read-only context metadata and not to reproduce it in replies.
func formatMessageEnvelope(msg providers.Message) string {
	prefix := messageAnnotationPrefix(msg)
	if prefix == "" {
		return msg.Content
	}
	return prefix + msg.Content
}

// messageAnnotationPrefix produces the `[from:…; msgs:#…, reply_to:#…] `
// bracket prefix (including the trailing space) for the message, or an
// empty string when no useful inbound facts are available.
func messageAnnotationPrefix(msg providers.Message) string {
	parts := make([]string, 0, 2)
	if sender := senderAnnotationPart(msg.Metadata); sender != "" {
		parts = append(parts, sender)
	}
	if thread := threadAnnotationPart(msg); thread != "" {
		parts = append(parts, thread)
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("[%s] ", strings.Join(parts, "; "))
}

// senderAnnotationPart renders the "from:Name (@handle)" segment from
// MessageMetadata. Display name and username are both optional; whichever
// is present is used, and if both are present they are combined as
// "from:Name (@handle)". Returns an empty string when neither is set.
func senderAnnotationPart(md *providers.MessageMetadata) string {
	if md == nil {
		return ""
	}
	name := strings.TrimSpace(md.SenderDisplayName)
	username := strings.TrimSpace(md.SenderUsername)
	if username != "" && !strings.HasPrefix(username, "@") {
		username = "@" + username
	}
	switch {
	case name != "" && username != "":
		return fmt.Sprintf("from:%s (%s)", name, username)
	case name != "":
		return fmt.Sprintf("from:%s", name)
	case username != "":
		return fmt.Sprintf("from:%s", username)
	default:
		return ""
	}
}

// threadAnnotationPart renders the "msgs:#…, reply_to:#…" segment.
// The pieces are optional and combined whichever are present.
func threadAnnotationPart(msg providers.Message) string {
	msgID := strings.TrimSpace(msg.MessageID)
	replyTo := ""
	if msg.Metadata != nil {
		replyTo = strings.TrimSpace(msg.Metadata.ReplyToMessageID)
	}
	switch {
	case msgID != "" && replyTo != "":
		return fmt.Sprintf("msgs:#%s, reply_to:#%s", msgID, replyTo)
	case msgID != "":
		return fmt.Sprintf("msgs:#%s", msgID)
	case replyTo != "":
		return fmt.Sprintf("reply_to:#%s", replyTo)
	default:
		return ""
	}
}

// inboundMessageMetadata builds the per-message metadata bag from the
// dispatch context. Returns nil when no useful fields are populated.
func inboundMessageMetadata(opts processOptions) *providers.MessageMetadata {
	md := &providers.MessageMetadata{
		SenderID:          opts.Dispatch.SenderID(),
		SenderDisplayName: opts.SenderDisplayName,
		SenderUsername:    opts.SenderUsername,
		ReplyToMessageID:  opts.Dispatch.ReplyToMessageID(),
	}
	if md.IsEmpty() {
		return nil
	}
	return md
}

func userPromptMessage(
	content string,
	media []string,
	messageID string,
	metadata *providers.MessageMetadata,
) providers.Message {
	msg := providers.Message{
		Role:      "user",
		Content:   content,
		MessageID: messageID,
		Metadata:  metadata,
	}
	if len(media) > 0 {
		msg.Media = append([]string(nil), media...)
	}
	return promptMessageWithMetadata(msg, PromptLayerTurn, PromptSlotMessage, PromptSourceUserMessage)
}

func toolResultPromptMessage(content, toolCallID string, media []string) providers.Message {
	msg := providers.Message{
		Role:       "tool",
		Content:    content,
		ToolCallID: toolCallID,
	}
	if len(media) > 0 {
		msg.Media = append([]string(nil), media...)
	}
	return promptMessageWithMetadata(msg, PromptLayerTurn, PromptSlotToolResult, PromptSourceToolResult)
}

func toolImageFollowUpPromptMessage(media []string) providers.Message {
	msg := providers.Message{
		Role:    "user",
		Content: "[Loaded image from tool result above]",
	}
	if len(media) > 0 {
		msg.Media = append([]string(nil), media...)
	}
	return promptMessageWithMetadata(msg, PromptLayerTurn, PromptSlotToolResult, PromptSourceToolResult)
}

func steeringPromptMessage(msg providers.Message) providers.Message {
	return promptMessageWithDefaultMetadata(msg, PromptLayerTurn, PromptSlotSteering, PromptSourceSteering)
}

func subTurnResultPromptMessage(content string) providers.Message {
	return promptMessageWithMetadata(
		providers.Message{Role: "user", Content: fmt.Sprintf("[SubTurn Result] %s", content)},
		PromptLayerTurn,
		PromptSlotSubTurn,
		PromptSourceSubTurnResult,
	)
}

func interruptPromptMessage(content string) providers.Message {
	return promptMessageWithMetadata(
		providers.Message{Role: "user", Content: content},
		PromptLayerTurn,
		PromptSlotInterrupt,
		PromptSourceInterrupt,
	)
}
