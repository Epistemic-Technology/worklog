// Package summarize turns a transcript or diff into a short summary
// by calling the Anthropic API. If no API key is configured, callers
// fall back to deterministic non-LLM summaries via Fallback*.
package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// PendingMarker is the summary placeholder written when an eager
// summarizer is unavailable. worklog resummarize looks for this.
const PendingMarker = "pending"

// ErrNoKey indicates the summarizer is not configured.
var ErrNoKey = errors.New("summarize: no API key configured")

// Client is a tiny Anthropic Messages API client. Zero value is not
// usable — use New.
type Client struct {
	APIKey string
	Model  string
	HTTP   *http.Client
	URL    string
}

func New(apiKey, model string) *Client {
	return &Client{
		APIKey: apiKey,
		Model:  model,
		HTTP:   &http.Client{Timeout: 60 * time.Second},
		URL:    "https://api.anthropic.com/v1/messages",
	}
}

// Configured reports whether the client has the bits it needs to
// actually call out. Callers should switch to Fallback* if not.
func (c *Client) Configured() bool {
	return c != nil && c.APIKey != "" && c.Model != ""
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
}

type responseBody struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Complete sends a single-turn request to the Anthropic API and
// returns the assistant text. Errors are returned verbatim; callers
// decide whether to fall back.
func (c *Client) Complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	if !c.Configured() {
		return "", ErrNoKey
	}
	reqBody := request{
		Model:     c.Model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  []message{{Role: "user", Content: user}},
	}
	b, err := json.Marshal(&reqBody)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var parsed responseBody
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

const summaryMaxChars = 200

// Commit produces a one-line summary plus a multi-line body for a
// commit. If the client isn't configured or fails, the fallback is
// used.
func (c *Client) Commit(ctx context.Context, message, diffstat string) (summary, body string) {
	body = formatCommitBody(message, diffstat)
	if !c.Configured() {
		summary = FallbackCommitSummary(message)
		return
	}
	prompt := fmt.Sprintf("Commit message:\n%s\n\nFiles changed:\n%s", message, diffstat)
	sys := "You write one-line summaries of git commits in 80 characters or fewer. Reply with the summary text only — no quotes, no preamble. Focus on what changed and why."
	out, err := c.Complete(ctx, sys, prompt, 200)
	if err != nil || out == "" {
		summary = FallbackCommitSummary(message)
		return
	}
	summary = clip(firstLine(out), summaryMaxChars)
	return
}

// Session produces a one-line summary plus body for an agent session
// given a transcript excerpt and a list of files touched.
func (c *Client) Session(ctx context.Context, firstPrompt, transcript string, files []string) (summary, body string) {
	body = formatSessionBody(firstPrompt, files)
	if !c.Configured() {
		summary = FallbackSessionSummary(firstPrompt)
		return
	}
	prompt := fmt.Sprintf("First user prompt:\n%s\n\nFiles touched:\n%s\n\nTranscript excerpt (may be truncated):\n%s",
		firstPrompt, strings.Join(files, "\n"), truncate(transcript, 12000))
	sys := "You summarize coding-agent sessions in one line (80 chars or fewer) followed by a short paragraph (3-5 sentences). Reply in this format:\nSUMMARY: <one line>\nDETAIL: <paragraph>\n"
	out, err := c.Complete(ctx, sys, prompt, 600)
	if err != nil || out == "" {
		summary = FallbackSessionSummary(firstPrompt)
		return
	}
	s, d := parseSummaryDetail(out)
	if s == "" {
		summary = FallbackSessionSummary(firstPrompt)
		return
	}
	summary = clip(s, summaryMaxChars)
	if d != "" {
		body = d + "\n\n" + body
	}
	return
}

// MonthlyReview composes a monthly narrative from event one-liners.
func (c *Client) MonthlyReview(ctx context.Context, period string, lines []string) string {
	joined := strings.Join(lines, "\n")
	if !c.Configured() {
		return FallbackReview(period, lines)
	}
	sys := "You write monthly engineering retrospectives. Given a list of dated event one-liners, output GitHub-flavored markdown with a brief Summary section and a Threads section that clusters related events. Keep it terse."
	prompt := fmt.Sprintf("Period: %s\n\nEvents (one per line):\n%s", period, joined)
	out, err := c.Complete(ctx, sys, prompt, 2000)
	if err != nil || out == "" {
		return FallbackReview(period, lines)
	}
	return out
}

// YearlyReview composes a yearly narrative from monthly review markdown.
func (c *Client) YearlyReview(ctx context.Context, year string, monthlies map[string]string) string {
	if !c.Configured() {
		return FallbackYearly(year, monthlies)
	}
	var sb strings.Builder
	keys := sortedKeys(monthlies)
	for _, k := range keys {
		sb.WriteString("## " + k + "\n")
		sb.WriteString(monthlies[k])
		sb.WriteString("\n\n")
	}
	sys := "You write yearly engineering retrospectives by composing twelve monthly reviews. Output GitHub-flavored markdown with a Summary section and a Threads section that highlights themes spanning multiple months. Keep it terse."
	prompt := fmt.Sprintf("Year: %s\n\nMonthly reviews:\n%s", year, sb.String())
	out, err := c.Complete(ctx, sys, prompt, 3000)
	if err != nil || out == "" {
		return FallbackYearly(year, monthlies)
	}
	return out
}

// FallbackCommitSummary takes the first line of the commit message.
func FallbackCommitSummary(message string) string {
	return clip(firstLine(message), summaryMaxChars)
}

// FallbackSessionSummary derives a summary from the first user prompt.
func FallbackSessionSummary(firstPrompt string) string {
	s := clip(firstLine(firstPrompt), summaryMaxChars)
	if s == "" {
		return "Claude Code session"
	}
	return s
}

// FallbackReview emits a deterministic bullet list when no LLM is
// available.
func FallbackReview(period string, lines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Summary\n\n%d events in %s. No LLM summary available; raw events below.\n\n## Events\n\n", len(lines), period)
	for _, l := range lines {
		b.WriteString("- ")
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// FallbackYearly concatenates monthly markdown headed by month.
func FallbackYearly(year string, monthlies map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Summary\n\nNo LLM summary available; monthly reviews concatenated below.\n\n")
	for _, k := range sortedKeys(monthlies) {
		b.WriteString("## " + k + "\n\n")
		b.WriteString(monthlies[k])
		b.WriteString("\n\n")
	}
	return b.String()
}

func formatCommitBody(message, diffstat string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(message))
	b.WriteString("\n")
	if diffstat != "" {
		b.WriteString("\n```\n")
		b.WriteString(strings.TrimSpace(diffstat))
		b.WriteString("\n```\n")
	}
	return b.String()
}

func formatSessionBody(firstPrompt string, files []string) string {
	var b strings.Builder
	if firstPrompt != "" {
		b.WriteString("**First prompt:** ")
		b.WriteString(firstLine(firstPrompt))
		b.WriteString("\n\n")
	}
	if len(files) > 0 {
		b.WriteString("**Files touched:**\n")
		for _, f := range files {
			b.WriteString("- ")
			b.WriteString(f)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func parseSummaryDetail(out string) (string, string) {
	var summary, detail string
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(l, "SUMMARY:"):
			summary = strings.TrimSpace(strings.TrimPrefix(l, "SUMMARY:"))
		case strings.HasPrefix(l, "DETAIL:"):
			detail = strings.TrimSpace(strings.TrimPrefix(l, "DETAIL:"))
		default:
			if detail != "" {
				detail += " " + l
			}
		}
	}
	if summary == "" {
		// Tolerate models that ignored the format.
		summary = firstLine(out)
	}
	return summary, detail
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// stdlib sort would pull in another import; tiny insertion sort.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
