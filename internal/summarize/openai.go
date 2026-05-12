package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openaiCompleter is a tiny direct-HTTP client for the OpenAI Chat
// Completions API. Compatible with any provider exposing the
// /v1/chat/completions shape (Groq, OpenRouter, local llama.cpp, ...).
type openaiCompleter struct {
	apiKey string
	model  string
	http   *http.Client
	url    string
}

func newOpenAI(apiKey, model string) *openaiCompleter {
	return &openaiCompleter{
		apiKey: apiKey,
		model:  model,
		http:   &http.Client{Timeout: 60 * time.Second},
		url:    "https://api.openai.com/v1/chat/completions",
	}
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiRequest struct {
	Model               string          `json:"model"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Messages            []openaiMessage `json:"messages"`
}

type openaiResponse struct {
	Choices []struct {
		Message openaiMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *openaiCompleter) Complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	msgs := make([]openaiMessage, 0, 2)
	if system != "" {
		msgs = append(msgs, openaiMessage{Role: "system", Content: system})
	}
	msgs = append(msgs, openaiMessage{Role: "user", Content: user})
	reqBody := openaiRequest{
		Model:               c.model,
		MaxCompletionTokens: maxTokens,
		Messages:            msgs,
	}
	b, err := json.Marshal(&reqBody)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var parsed openaiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("summarize: parse: %w (body=%s)", err, truncate(string(raw), 500))
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != nil {
			return "", fmt.Errorf("summarize: %s: %s", parsed.Error.Type, parsed.Error.Message)
		}
		return "", fmt.Errorf("summarize: http %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	if len(parsed.Choices) == 0 {
		return "", nil
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}
