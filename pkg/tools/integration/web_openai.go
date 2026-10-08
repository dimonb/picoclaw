package integrationtools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/auth"
	oauthprovider "github.com/sipeed/picoclaw/pkg/providers/oauth"
)

const (
	// openAISearchEndpoint is the standalone search endpoint the Codex app calls
	// (codex-rs/codex-api/src/endpoint/search.rs: "alpha/search" relative to the
	// ChatGPT Codex base URL). It is internal and unversioned: keep another
	// backend enabled to fall back on.
	openAISearchEndpoint        = "https://chatgpt.com/backend-api/codex/alpha/search"
	openAISearchTimeout         = 60 * time.Second
	openAISearchDefaultModel    = "gpt-5.5"
	openAISearchDefaultMaxToken = 2000
	openAISearchUserAgent       = "codex_cli_rs/0.160.0 (picoclaw)"
	openAISearchMaxErrorBody    = 512
)

// openAICitationMarker matches the inline citation markers the endpoint puts in
// its text (U+E200 "cite" U+E202 "turn0search0" U+E201). They point into the
// Codex client's own result store, which picoclaw does not have.
var openAICitationMarker = regexp.MustCompile("[^]*")

// OpenAISearchProvider searches through OpenAI's standalone web search with
// the ChatGPT (Codex) OAuth login.
type OpenAISearchProvider struct {
	tokenSource     func() (string, string, error)
	model           string
	maxOutputTokens int
	sessionID       string
	endpoint        string
	client          *http.Client
}

type openAISearchRequest struct {
	ID              string                `json:"id"`
	Model           string                `json:"model"`
	Commands        openAISearchCommands  `json:"commands"`
	Settings        *openAISearchSettings `json:"settings,omitempty"`
	MaxOutputTokens int                   `json:"max_output_tokens,omitempty"`
}

type openAISearchCommands struct {
	SearchQuery    []openAISearchQuery `json:"search_query"`
	ResponseLength string              `json:"response_length,omitempty"`
}

type openAISearchQuery struct {
	Q       string `json:"q"`
	Recency int    `json:"recency,omitempty"`
}

type openAISearchSettings struct {
	AllowedCallers    []string `json:"allowed_callers,omitempty"`
	ExternalWebAccess bool     `json:"external_web_access"`
}

type openAISearchResponse struct {
	Output string `json:"output"`
}

// openAICredentialReady reports whether a ChatGPT OAuth login is stored; the
// search endpoint does not take API keys.
func openAICredentialReady() bool {
	cred, err := auth.GetCredential("openai")
	return err == nil && cred != nil && cred.AuthMethod == "oauth" && cred.AccessToken != ""
}

func newOpenAISearchProvider(opts WebSearchToolOptions, client *http.Client) *OpenAISearchProvider {
	tokenSource := opts.OpenAITokenSource
	if tokenSource == nil {
		tokenSource = oauthprovider.CreateCodexTokenSource()
	}
	model := strings.TrimSpace(opts.OpenAIModel)
	if model == "" {
		model = openAISearchDefaultModel
	}
	maxTokens := opts.OpenAIMaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = openAISearchDefaultMaxToken
	}
	endpoint := opts.OpenAIEndpoint
	if endpoint == "" {
		endpoint = openAISearchEndpoint
	}
	return &OpenAISearchProvider{
		tokenSource:     tokenSource,
		model:           model,
		maxOutputTokens: maxTokens,
		sessionID:       uuid.NewString(),
		endpoint:        endpoint,
		client:          client,
	}
}

func (p *OpenAISearchProvider) Search(ctx context.Context, query string, count int, rangeCode string) (string, error) {
	token, accountID, err := p.tokenSource()
	if err != nil {
		return "", fmt.Errorf("openai auth: %w", err)
	}

	responseLength := "short"
	if count > 5 {
		responseLength = "medium"
	}
	body, err := json.Marshal(openAISearchRequest{
		ID:    p.sessionID,
		Model: p.model,
		Commands: openAISearchCommands{
			SearchQuery:    []openAISearchQuery{{Q: query, Recency: openAISearchRecencyDays(rangeCode)}},
			ResponseLength: responseLength,
		},
		Settings: &openAISearchSettings{
			AllowedCallers:    []string{"direct"},
			ExternalWebAccess: true,
		},
		MaxOutputTokens: p.maxOutputTokens,
	})
	if err != nil {
		return "", fmt.Errorf("failed to encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	req.Header.Set("Originator", "codex_cli_rs")
	req.Header.Set("User-Agent", openAISearchUserAgent)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(respBody))
		if len(snippet) > openAISearchMaxErrorBody {
			snippet = snippet[:openAISearchMaxErrorBody] + "..."
		}
		return "", fmt.Errorf("openai search: status %d: %s", resp.StatusCode, snippet)
	}

	var parsed openAISearchResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}
	output := strings.TrimSpace(openAICitationMarker.ReplaceAllString(parsed.Output, ""))
	if output == "" {
		return fmt.Sprintf("No results for: %s", query), nil
	}
	return fmt.Sprintf("Results for: %s (via OpenAI)\n\n%s", query, output), nil
}

// openAISearchRecencyDays maps the tool's range filter onto the endpoint's
// recency, a number of recent days.
func openAISearchRecencyDays(rangeCode string) int {
	switch rangeCode {
	case "d":
		return 1
	case "w":
		return 7
	case "m":
		return 30
	case "y":
		return 365
	default:
		return 0
	}
}
