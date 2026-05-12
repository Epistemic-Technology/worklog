package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mikethicke/worklog/internal/config"
)

const (
	postCommitHook = `#!/bin/sh
# worklog: capture each commit. Background so we don't slow down git.
worklog capture-commit "$(git rev-parse HEAD)" >/dev/null 2>&1 &
exit 0
`

	captureSessionShim = `#!/bin/sh
# worklog: invoked by Claude Code SessionEnd hook. Forwards stdin JSON
# to ` + "`worklog capture-claude`" + `. Stdout/stderr are appended to
# .git/worklog-capture.log so silent hook failures (missing binary on
# PATH, parse errors, API failures) are debuggable. The log lives
# under .git/ so it's never committed.
root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
log="$root/.git/worklog-capture.log"
{
  printf '[%s] capture-session start (pid=%s)\n' "$(date -u +%FT%TZ)" "$$"
  worklog capture-claude
  rc=$?
  printf '[%s] capture-session exit=%d\n' "$(date -u +%FT%TZ)" "$rc"
  exit "$rc"
} >>"$log" 2>&1
`

	defaultConfig = `# Per-repo worklog config. Committed and shared with collaborators —
# keep team-wide settings here. Per-user defaults (API key path,
# preferred summarizer model, attribution) belong in
# ~/.config/worklog/config.yml; anything set here overrides those
# defaults for this project.

project: %s

# Uncomment any of the sections below to override the global defaults
# for this project.

# author: yourname            # overrides gh login / OS user
#
# author_aliases:             # unify attribution across event kinds
#   mikethicke: Mike Thicke   # e.g. map OS user to git commit name
#   mike@example.com: Mike Thicke
#
# git:
#   skip_merges: true
#   skip_authors: ["dependabot[bot]", "renovate[bot]"]
#   collapse_fixups: true
#
# claude_code:
#   enabled: true
#   store_transcripts: false
#
# agents:
#   cursor: false
#   aider: false
#   cline: false
#
# summarizer:
#   provider: anthropic
#   model: claude-haiku-4-5
#   api_key_env: ANTHROPIC_API_KEY
#
# reviews:
#   auto_generate: false
#   persist: true             # cache reviews to .worklog/reviews/
`
)

func runInit(root string) error {
	steps := []func(string) error{
		ensureWorklogDirs,
		writeDefaultConfig,
		writeCaptureSessionShim,
		installPostCommitHook,
		writeClaudeSettings,
		writeReadme,
	}
	for _, step := range steps {
		if err := step(root); err != nil {
			return err
		}
	}
	fmt.Println("worklog initialized in", config.WorklogDir(root))
	fmt.Println("Next steps:")
	fmt.Println("  - Set ANTHROPIC_API_KEY to enable LLM summaries")
	fmt.Println("  - Run `worklog sync` to import existing git history")
	fmt.Println("  - The Claude Code SessionEnd hook will prompt for approval on next session")
	return nil
}

func ensureWorklogDirs(root string) error {
	dirs := []string{
		config.WorklogDir(root),
		filepath.Join(config.WorklogDir(root), "reviews"),
		filepath.Join(config.WorklogDir(root), "bin"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func writeDefaultConfig(root string) error {
	path := filepath.Join(config.WorklogDir(root), "config.yml")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	project := filepath.Base(root)
	return os.WriteFile(path, []byte(fmt.Sprintf(defaultConfig, project)), 0o644)
}

func writeCaptureSessionShim(root string) error {
	path := filepath.Join(config.WorklogDir(root), "bin", "capture-session")
	if err := os.WriteFile(path, []byte(captureSessionShim), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

func installPostCommitHook(root string) error {
	hookPath := filepath.Join(root, ".git", "hooks", "post-commit")
	if existing, err := os.ReadFile(hookPath); err == nil {
		if containsWorklog(string(existing)) {
			return nil
		}
		// Append, but back the old one up first.
		if err := os.WriteFile(hookPath+".pre-worklog", existing, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(hookPath, []byte(postCommitHook), 0o755)
}

func containsWorklog(s string) bool {
	return strings.Contains(s, "worklog capture-commit")
}

// writeClaudeSettings adds a SessionEnd hook to .claude/settings.json
// without clobbering any existing hooks the user already has.
func writeClaudeSettings(root string) error {
	dir := filepath.Join(root, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "settings.json")

	settings := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &settings); err != nil {
			return fmt.Errorf("parsing existing %s: %w", path, err)
		}
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	sessionEnd, _ := hooks["SessionEnd"].([]any)
	cmd := ".worklog/bin/capture-session"
	if alreadyHasWorklogHook(sessionEnd, cmd) {
		return nil
	}
	sessionEnd = append(sessionEnd, map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": cmd,
			},
		},
	})
	hooks["SessionEnd"] = sessionEnd
	settings["hooks"] = hooks

	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func alreadyHasWorklogHook(sessionEnd []any, want string) bool {
	for _, entry := range sessionEnd {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := m["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if hm == nil {
				continue
			}
			if cmd, _ := hm["command"].(string); cmd == want {
				return true
			}
		}
	}
	return false
}

func writeReadme(root string) error {
	path := filepath.Join(config.WorklogDir(root), "README.md")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	const body = `# worklog

This directory is managed by ` + "`worklog`" + `. Each event (a commit, a
Claude Code session, a manual note) lives as a markdown file sharded
by month. Reviews under ` + "`reviews/`" + ` are generated from those events.

## What to commit

Commit the entire ` + "`.worklog/`" + ` tree. Event files are designed to merge
cleanly: each file's name embeds a timestamp and source id, so two
collaborators capturing in parallel will never write the same path.

## Hooks

` + "`worklog init`" + ` installed:

- ` + "`.git/hooks/post-commit`" + ` — calls ` + "`worklog capture-commit`" + ` in the
  background for each commit.
- ` + "`.claude/settings.json`" + ` — registers a SessionEnd hook that runs
  ` + "`.worklog/bin/capture-session`" + `. **Claude Code will prompt every
  collaborator to approve this hook on first session after a config
  change** — this is expected.

If you skip a session or commit (hook disabled, hard crash, fresh
clone), ` + "`worklog sync`" + ` walks the same sources and writes anything
missing.

## API key

LLM summaries call the Anthropic API. Set ` + "`ANTHROPIC_API_KEY`" + ` in your
shell, or point worklog at a different env var globally via
` + "`~/.config/worklog/config.yml`" + `. Without a key, events are still captured
using deterministic fallback summaries; run ` + "`worklog resummarize`" + `
later to fill them in.

## Configuration layers

This file (` + "`.worklog/config.yml`" + `) is committed and shared with
collaborators — keep team-wide settings here. Per-user defaults
(API key path, preferred model, attribution) belong in
` + "`~/.config/worklog/config.yml`" + `; this file overrides them for this
project.
`
	return os.WriteFile(path, []byte(body), 0o644)
}

