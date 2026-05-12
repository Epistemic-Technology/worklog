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

// anthropicCompleter is a tiny direct-HTTP client for the Anthropic
// Messages API. No SDK — hand-rolled JSON keeps the dependency surface
// small.
type anthropicCompleter struct {
	apiKey string
	model  string
	http   *http.Client
	url    string
}

func newAnthropic(apiKey, model string) *anthropicCompleter {
	return &anthropicCompleter{
		apiKey: apiKey,
		model:  model,
		http:   &http.Client{Timeout: 60 * time.Second},
		url:    "https://api.anthropic.com/v1/messages",
	}
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *anthropicCompleter) Complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	reqBody := anthropicRequest{
		Model:     c.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  []anthropicMessage{{Role: "user", Content: user}},
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
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var parsed anthropicResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("summarize: parse: %w (body=%s)", err, truncate(string(raw), 500))
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != nil {
			return "", fmt.Errorf("summarize: %s: %s", parsed.Error.Type, parsed.Error.Message)
		}
		return "", fmt.Errorf("summarize: http %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	var out strings.Builder
	for _, blk := range parsed.Content {
		if blk.Type == "text" {
			out.WriteString(blk.Text)
		}
	}
	return strings.TrimSpace(out.String()), nil
}
