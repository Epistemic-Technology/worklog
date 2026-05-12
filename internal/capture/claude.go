package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mikethicke/worklog/internal/config"
	"github.com/mikethicke/worklog/internal/event"
	"github.com/mikethicke/worklog/internal/summarize"
)

// ClaudeSessionPayload is the JSON the SessionEnd hook delivers on stdin.
type ClaudeSessionPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	Reason         string `json:"reason"`
}

// SessionSummary is what we extract from a parsed JSONL transcript.
type SessionSummary struct {
	SessionID    string
	StartTime    time.Time
	EndTime      time.Time
	FirstPrompt  string
	FilesTouched []string
	Excerpt      string
}

// ClaudeProjectsDir returns ~/.claude/projects, or empty on error.
func ClaudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// EncodeCWD mirrors how Claude Code derives a project directory name
// from a working directory: every path separator becomes a dash.
func EncodeCWD(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	abs = filepath.Clean(abs)
	return strings.ReplaceAll(abs, string(filepath.Separator), "-")
}

// CaptureClaudeFromPayload writes an event file from a SessionEnd hook
// payload. Returns the path on success or "" if filtered/already captured.
func CaptureClaudeFromPayload(ctx context.Context, root string, cfg config.Config, sum *summarize.Client, p ClaudeSessionPayload) (string, error) {
	if !cfg.ClaudeCode.Enabled {
		return "", nil
	}
	if p.TranscriptPath == "" {
		return "", errors.New("capture: missing transcript_path")
	}
	return captureClaudeFromTranscript(ctx, root, cfg, sum, p.TranscriptPath, p.SessionID)
}

// SyncClaude walks ~/.claude/projects/<encoded-cwd>/ and captures any
// sessions that don't yet have event files.
func SyncClaude(ctx context.Context, root string, cfg config.Config, sum *summarize.Client) (int, error) {
	if !cfg.ClaudeCode.Enabled {
		return 0, nil
	}
	dir := filepath.Join(ClaudeProjectsDir(), EncodeCWD(root))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		sessionID := strings.TrimSuffix(e.Name(), ".jsonl")
		path, err := captureClaudeFromTranscript(ctx, root, cfg, sum, filepath.Join(dir, e.Name()), sessionID)
		if err != nil {
			return count, fmt.Errorf("capture session %s: %w", sessionID, err)
		}
		if path != "" {
			count++
		}
	}
	return count, nil
}

func captureClaudeFromTranscript(ctx context.Context, root string, cfg config.Config, sum *summarize.Client, transcriptPath, sessionID string) (string, error) {
	info, err := parseClaudeTranscript(transcriptPath)
	if err != nil {
		return "", err
	}
	if sessionID == "" {
		sessionID = info.SessionID
	}
	if sessionID == "" {
		return "", fmt.Errorf("capture: no session id for %s", transcriptPath)
	}
	if info.StartTime.IsZero() {
		// Empty transcript — skip.
		return "", nil
	}
	short := sessionID
	if len(short) > 8 {
		short = short[:8]
	}
	path := event.Path(config.WorklogDir(root), info.StartTime, "cc", short)
	if event.Exists(path) {
		return "", nil
	}
	summary, body := sum.Session(ctx, info.FirstPrompt, info.Excerpt, info.FilesTouched)
	if summary == "" {
		summary = summarize.PendingMarker
	}
	fm := event.Frontmatter{
		Time:      info.StartTime,
		Kind:      event.KindClaudeSession,
		Author:    cfg.ResolveAuthor(),
		Refs:      []string{"claude:" + sessionID},
		Summary:   summary,
		SessionID: sessionID,
	}
	if err := event.Write(path, fm, body); err != nil {
		return "", err
	}
	return path, nil
}

// parseClaudeTranscript reads a Claude Code JSONL session file and
// extracts the fields the summarizer needs. Robust to unknown line
// types — anything not recognized is skipped.
func parseClaudeTranscript(path string) (SessionSummary, error) {
	var info SessionSummary
	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)

	fileSet := map[string]struct{}{}
	var excerpt strings.Builder
	const excerptBudget = 12 * 1024

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		// Pull timestamp if present at the top level.
		if ts, ok := raw["timestamp"]; ok {
			var s string
			if json.Unmarshal(ts, &s) == nil {
				if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
					if info.StartTime.IsZero() || t.Before(info.StartTime) {
						info.StartTime = t
					}
					if t.After(info.EndTime) {
						info.EndTime = t
					}
				}
			}
		}
		if sid, ok := raw["sessionId"]; ok && info.SessionID == "" {
			var s string
			if json.Unmarshal(sid, &s) == nil {
				info.SessionID = s
			}
		}

		var t string
		if err := json.Unmarshal(raw["type"], &t); err != nil {
			continue
		}
		switch t {
		case "user":
			extractFromUser(raw["message"], &info, fileSet, &excerpt, excerptBudget)
		case "assistant":
			extractFromAssistant(raw["message"], fileSet, &excerpt, excerptBudget)
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return info, err
	}
	info.FilesTouched = sortedUnique(fileSet)
	info.Excerpt = excerpt.String()
	return info, nil
}

func extractFromUser(rawMsg json.RawMessage, info *SessionSummary, files map[string]struct{}, excerpt *strings.Builder, budget int) {
	if len(rawMsg) == 0 {
		return
	}
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(rawMsg, &msg); err != nil {
		return
	}
	// content may be string or array
	var asString string
	if err := json.Unmarshal(msg.Content, &asString); err == nil {
		if info.FirstPrompt == "" && !looksLikeToolNoise(asString) {
			info.FirstPrompt = asString
		}
		appendExcerpt(excerpt, "USER: "+asString, budget)
		return
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(msg.Content, &blocks); err == nil {
		for _, blk := range blocks {
			var bt string
			_ = json.Unmarshal(blk["type"], &bt)
			switch bt {
			case "text":
				var txt string
				_ = json.Unmarshal(blk["text"], &txt)
				if info.FirstPrompt == "" && !looksLikeToolNoise(txt) {
					info.FirstPrompt = txt
				}
				appendExcerpt(excerpt, "USER: "+txt, budget)
			}
		}
	}
}

func extractFromAssistant(rawMsg json.RawMessage, files map[string]struct{}, excerpt *strings.Builder, budget int) {
	if len(rawMsg) == 0 {
		return
	}
	var msg struct {
		Content []map[string]json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(rawMsg, &msg); err != nil {
		return
	}
	for _, blk := range msg.Content {
		var bt string
		_ = json.Unmarshal(blk["type"], &bt)
		switch bt {
		case "text":
			var txt string
			_ = json.Unmarshal(blk["text"], &txt)
			appendExcerpt(excerpt, "ASSISTANT: "+txt, budget)
		case "tool_use":
			var name string
			_ = json.Unmarshal(blk["name"], &name)
			switch name {
			case "Edit", "Write", "NotebookEdit", "MultiEdit", "Read":
				var input map[string]json.RawMessage
				if err := json.Unmarshal(blk["input"], &input); err == nil {
					if fp, ok := input["file_path"]; ok {
						var s string
						if json.Unmarshal(fp, &s) == nil && s != "" && name != "Read" {
							files[s] = struct{}{}
						}
					}
				}
			}
		}
	}
}

func appendExcerpt(b *strings.Builder, s string, budget int) {
	if b.Len() >= budget {
		return
	}
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	b.WriteString(s)
	b.WriteString("\n")
}

func looksLikeToolNoise(s string) bool {
	// Tool-result and system messages tend to start with sentinel tags.
	trim := strings.TrimSpace(s)
	return strings.HasPrefix(trim, "<command-") ||
		strings.HasPrefix(trim, "<local-command-") ||
		strings.HasPrefix(trim, "<system-reminder>") ||
		strings.HasPrefix(trim, "<bash-")
}

func sortedUnique(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
