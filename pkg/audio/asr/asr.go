package asr

import (
	"context"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

const elevenLabsSupportedModelID = "scribe_v1"

func ElevenLabsSupportedModelID() string {
	return elevenLabsSupportedModelID
}

type Transcriber interface {
	Name() string
	Transcribe(ctx context.Context, audioFilePath string) (*TranscriptionResponse, error)
}

type languageKey struct{}

// WithLanguage attaches a language hint (ISO-639-1) for the transcription
// started with ctx. Transcribers that cannot take one ignore it.
func WithLanguage(ctx context.Context, lang string) context.Context {
	if lang == "" {
		return ctx
	}
	return context.WithValue(ctx, languageKey{}, lang)
}

func languageFrom(ctx context.Context) string {
	lang, _ := ctx.Value(languageKey{}).(string)
	return lang
}

type promptKey struct{}

// WithPrompt attaches recognizer context (topic, names, vocabulary) for the
// transcription started with ctx. Transcribers that cannot take one ignore it.
func WithPrompt(ctx context.Context, prompt string) context.Context {
	if prompt == "" {
		return ctx
	}
	return context.WithValue(ctx, promptKey{}, prompt)
}

func promptFrom(ctx context.Context) string {
	prompt, _ := ctx.Value(promptKey{}).(string)
	return prompt
}

// TranscriberForModel returns the transcriber for a model_list entry, or nil
// when the entry is missing or is not a transcription model.
func TranscriberForModel(cfg *config.Config, modelName string) Transcriber {
	if cfg == nil || strings.TrimSpace(modelName) == "" {
		return nil
	}
	modelCfg, err := cfg.GetModelConfig(strings.TrimSpace(modelName))
	if err != nil {
		return nil
	}
	return transcriberFromModelConfig(modelCfg)
}

type TranscriptionResponse struct {
	Text     string  `json:"text"`
	Language string  `json:"language,omitempty"`
	Duration float64 `json:"duration,omitempty"`
}

func supportsAudioTranscription(modelCfg *config.ModelConfig) bool {
	protocol, _ := providers.ExtractProtocol(modelCfg)

	switch protocol {
	case "openai", "azure",
		"litellm", "openrouter", "groq", "zhipu", "gemini", "nvidia",
		"ollama", "moonshot", "shengsuanyun", "deepseek", "cerebras",
		"vivgrid", "volcengine", "vllm", "qwen-portal", "qwen-intl", "qwen-us",
		"mistral", "avian", "minimax", "longcat", "modelscope", "novita",
		"alibaba-coding", "zai":
		// These protocols all go through the OpenAI-compatible or Azure provider path in
		// providers.CreateProviderFromConfig, so they are the only ones that can supply
		// the audio media payload shape expected by NewAudioModelTranscriber.

		// TODO: Further restrict this by modelID, since not every model under these
		// protocols supports audio transcription.
		return true
	default:
		return false
	}
}

func supportsWhisperTranscription(modelCfg *config.ModelConfig) bool {
	protocol, _ := providers.ExtractProtocol(modelCfg)

	switch protocol {
	case "openai", "litellm", "openrouter", "groq", "zhipu", "gemini", "nvidia",
		"ollama", "moonshot", "shengsuanyun", "deepseek", "cerebras",
		"vivgrid", "volcengine", "vllm", "qwen-portal", "qwen-intl", "qwen-us",
		"mistral", "avian", "minimax", "longcat", "modelscope", "novita",
		"alibaba-coding", "zai", "mimo":
		return true
	default:
		return false
	}
}

func whisperModelID(modelCfg *config.ModelConfig) string {
	if modelCfg == nil || modelCfg.APIKey() == "" {
		return ""
	}

	if !supportsWhisperTranscription(modelCfg) {
		return ""
	}

	_, modelID := providers.ExtractProtocol(modelCfg)
	if strings.Contains(strings.ToLower(modelID), "whisper") {
		return modelID
	}
	return ""
}

// transcriptionEndpointModelID is whisperModelID widened to the dedicated
// transcription models (gpt-transcribe, gpt-4o-transcribe, ...), which take the
// same /audio/transcriptions request. Only an explicitly chosen
// voice.model_name gets this; the legacy model_list scan stays whisper-only.
func transcriptionEndpointModelID(modelCfg *config.ModelConfig) string {
	if modelID := whisperModelID(modelCfg); modelID != "" {
		return modelID
	}
	if modelCfg == nil || modelCfg.APIKey() == "" || !supportsWhisperTranscription(modelCfg) {
		return ""
	}
	_, modelID := providers.ExtractProtocol(modelCfg)
	if strings.Contains(strings.ToLower(modelID), "transcribe") {
		return modelID
	}
	return ""
}

func isElevenLabsTranscriptionModel(modelCfg *config.ModelConfig) bool {
	if modelCfg == nil || modelCfg.APIKey() == "" {
		return false
	}

	protocol, _ := providers.ExtractProtocol(modelCfg)
	return protocol == "elevenlabs"
}

func transcriberFromModelConfig(modelCfg *config.ModelConfig) Transcriber {
	if modelCfg == nil {
		return nil
	}

	if isElevenLabsTranscriptionModel(modelCfg) {
		_, modelID := providers.ExtractProtocol(modelCfg)
		return NewElevenLabsTranscriber(modelCfg.APIKey(), modelCfg.APIBase, modelID)
	}
	if modelID := transcriptionEndpointModelID(modelCfg); modelID != "" {
		return NewWhisperTranscriber(modelCfg)
	}
	if supportsAudioTranscription(modelCfg) {
		return NewAudioModelTranscriber(modelCfg)
	}
	return nil
}

func fallbackTranscriberFromModelConfig(modelCfg *config.ModelConfig) Transcriber {
	if modelCfg == nil {
		return nil
	}

	if isElevenLabsTranscriptionModel(modelCfg) {
		_, modelID := providers.ExtractProtocol(modelCfg)
		return NewElevenLabsTranscriber(modelCfg.APIKey(), modelCfg.APIBase, modelID)
	}
	if modelID := whisperModelID(modelCfg); modelID != "" {
		return NewWhisperTranscriber(modelCfg)
	}
	return nil
}

// DetectTranscriber inspects cfg and returns the appropriate Transcriber, or
// nil if no supported transcription provider is configured.
func DetectTranscriber(cfg *config.Config) Transcriber {
	if cfg == nil {
		return nil
	}

	if modelName := strings.TrimSpace(cfg.Voice.ModelName); modelName != "" {
		modelCfg, err := cfg.GetModelConfig(modelName)
		if err == nil {
			if tr := transcriberFromModelConfig(modelCfg); tr != nil {
				return tr
			}
		}
	}

	// Fall back to compatibility scanning for legacy auto-detected ASR providers.
	for _, mc := range cfg.ModelList {
		if tr := fallbackTranscriberFromModelConfig(mc); tr != nil {
			return tr
		}
	}
	return nil
}
