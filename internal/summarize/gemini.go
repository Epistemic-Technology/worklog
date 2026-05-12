package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// geminiCompleter is a tiny direct-HTTP client for the Gemini
// generateContent API.
type geminiCompleter struct {
	apiKey  string
	model   string
	http    *http.Client
	baseURL string
}

func newGemini(apiKey, model string) *geminiCompleter {
	return &geminiCompleter{
		apiKey:  apiKey,
		model:   model,
		http:    &http.Client{Timeout: 60 * time.Second},
		baseURL: "https://generativelanguage.googleapis.com/v1beta/models",
	}
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

type geminiRequest struct {
	SystemInstruction *geminiContent         `json:"system_instruction,omitempty"`
	Contents          []geminiContent        `json:"contents"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

func (c *geminiCompleter) Complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	reqBody := geminiRequest{
		Contents: []geminiContent{
			{Role: "user", Parts: []geminiPart{{Text: user}}},
		},
		GenerationConfig: geminiGenerationConfig{MaxOutputTokens: maxTokens},
	}
	if system != "" {
		reqBody.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: system}}}
	}
	b, err := json.Marshal(&reqBody)
	if err != nil {
		return "", err
	}
	endpoint := fmt.Sprintf("%s/%s:generateContent", c.baseURL, url.PathEscape(c.model))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var parsed geminiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("summarize: parse: %w (body=%s)", err, truncate(string(raw), 500))
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != nil {
			return "", fmt.Errorf("summarize: %s: %s", parsed.Error.Status, parsed.Error.Message)
		}
		return "", fmt.Errorf("summarize: http %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	var out strings.Builder
	for _, cand := range parsed.Candidates {
		for _, p := range cand.Content.Parts {
			out.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(out.String()), nil
}
