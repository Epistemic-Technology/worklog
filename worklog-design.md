# Worklog: Design Doc

## Problem

Tracking work on a software project is harder than it should be. `git log` shows commits but is full of noise (merges, fixup chains, bot commits) and omits a lot of the actual work — design discussions, coding-agent sessions, decisions that didn't produce a commit. We want a unified, per-project event log that captures all of this automatically and lets us review history at multiple resolutions (day, week, month, year).

## Goals

- Per-project log, committed to the repo, shared across collaborators.
- Automatic capture. Anything that requires the user to remember will be lost.
- Conflict-free concurrent appends.
- Readable reviews at multiple time resolutions.
- Idempotent capture — re-running is always safe.
- Implementation in Go, distributed as a single static binary.

## Non-goals

- Replacing git.
- Real-time collaboration or a server component.
- Cross-project aggregation (deferred to a later phase).
- Storing raw agent transcripts by default.
- A GUI.

## Overview

An **event log** model. Every "thing that happened" — a commit, a Claude Code session, a manual note — becomes one markdown file in `.worklog/` with YAML frontmatter. Capture is automated via git hooks and Claude Code's `SessionEnd` hook. A `worklog sync` command serves as a reconciler that walks the same sources and writes any events the hooks missed; it is the source of truth. Reviews at week/month/year resolutions are derived by composing event summaries upward, with monthly and yearly summaries committed as files so they're stable and cheap to read.

## Data model

### Directory layout

```
.worklog/
  config.yml
  2026-05/
    2026-05-09T14-23-00-git-abc1234.md
    2026-05-09T15-01-12-cc-def456.md
    2026-05-09T16-30-44-note-a1b2c3.md
  reviews/
    2026-05.md
    2026.md
  bin/
    capture-session       # shim invoked by Claude Code hook
```

Sharding by month keeps directories browsable. The `bin/` directory holds short shell shims that hooks call into; the actual logic is in the Go binary.

### Event file

YAML frontmatter plus a markdown body. The filename encodes timestamp, kind, and a source ID — that's what makes capture idempotent (the dedup check is a single `stat`).

```markdown
---
time: 2026-05-09T14:23:00-04:00
kind: commit
author: alice
refs:
  - git:abc1234567890
summary: Refactor auth middleware to use new session abstraction
---
The old `requireUser` wrapper is gone; routes now declare auth needs
via the new decorator. Touched 12 files.
```

Required fields: `time`, `kind`. Optional: `author`, `refs`, `summary`, `tags`, `session_id`, `thread`. Filenames use `T` as the date/time separator and replace `:` with `-` to stay portable across filesystems.

Event kinds:

- `commit` — git commit, post-filter
- `claude-session` — Claude Code session
- `agent-session` — other coding-agent session
- `note` — manual entry
- `pr` — pull request opened/merged (optional)
- `tag` — release or annotated tag

The frontmatter `summary` is the one-liner; the body is freeform detail. For commits, the body is the commit message and a list of files. For agent sessions, the body is an LLM-generated paragraph plus files touched.

### Review files

Monthly and yearly reviews are committed under `.worklog/reviews/` so they're stable, diff-able in PRs, and don't need to be regenerated for every read.

```markdown
---
period: 2026-05
generated_at: 2026-06-01T09:00:00Z
event_count: 47
---
## Summary
Two main threads this month: the auth refactor and exploratory work
on a vector DB integration.

## Threads
### Auth refactor (week 1-2)
- Replaced `requireUser` wrapper with route-level decorators
- ...
```

Yearly reviews are composed from monthly reviews, never from raw events. This bounds token cost and keeps higher-level summaries stable when you re-run them.

## Architecture

Four components, each an internal Go package:

### 1. Capture (hooks)

The fast path. Two hooks ship out of the box:

- **Git `post-commit` hook**, installed by `worklog init` into `.git/hooks/post-commit`. Calls `worklog capture-commit $(git rev-parse HEAD)`. Skips merge commits and configured bot authors.
- **Claude Code `SessionEnd` hook**, configured in `.claude/settings.json` (committed to the repo). Calls `.worklog/bin/capture-session`, which reads the hook's JSON payload (`session_id`, `transcript_path`, `cwd`, `reason`) from stdin and invokes `worklog capture-claude`.

Hooks must return in under a second. If summarization is slow, capture writes a stub event file with `summary: pending` and a background pass fills it in later. The hook is never the thing that calls the LLM synchronously.

### 2. Reconciler (sync)

`worklog sync` walks every source and writes event files for anything missing. It's the safety net for fresh clones, disabled hooks, crashed sessions, and agents that don't support hooks.

- **Git**: `git log --no-merges` plus configured filters. Compares SHAs against existing event files.
- **Claude Code**: walks `~/.claude/projects/<encoded-cwd>/` and writes event files for any session not already represented.
- **Other agents** (Aider, Cursor, Cline): parsers behind feature flags, deferred to a later phase.

Idempotency comes for free from filenames embedding source IDs.

### 3. Summarizer

A small package that turns a transcript or diff into a short summary by calling an LLM API. Two modes:

- **Eager**, during `sync` or capture — blocks until the summary is written.
- **Lazy**, via `worklog resummarize` — picks up event files with `summary: pending` and fills them in.

If no API key is configured, capture still writes the event file using a fallback: commit message for git, first user prompt plus files-touched for agent sessions. We never drop events because of a missing key.

### 4. Renderer (review)

Reads event files in a date range, optionally groups and summarizes, prints markdown.

- `worklog show --week` — events with one-liner summaries, grouped by day.
- `worklog show --month` — events clustered into themes via the LLM, with a brief narrative.
- `worklog show --year` — narrative composed from the twelve monthly reviews.

Monthly and yearly outputs can be persisted with `--write`, which writes to `.worklog/reviews/`. The expectation is that someone (or a CI job) generates the monthly review at month boundary and commits it.

## CLI surface

```
worklog init                        Set up .worklog/, install git hook,
                                    write .claude/settings.json snippet,
                                    create config.yml.

worklog capture-commit <sha>        Internal: invoked by git post-commit hook.
worklog capture-claude              Internal: invoked by Claude Code hook,
                                    reads JSON from stdin.

worklog note ["<text>"]             Manual entry. Opens $EDITOR if no text.

worklog sync                        Reconcile: write event files for any
                                    git commits or agent sessions not yet
                                    captured.

worklog resummarize [--pending]     Fill in pending summaries via the LLM.

worklog show [--day|--week|--month|--year]
             [--since DATE] [--until DATE]
                                    Render a review to stdout.

worklog review --month YYYY-MM --write
worklog review --year  YYYY    --write
                                    Generate and persist a review file.

worklog ls [--kind KIND]            List raw event files.
```

### Go layout

Standard module layout:

```
cmd/worklog/main.go
internal/
  event/       Event file read/write, frontmatter parsing.
  capture/     Git, Claude Code, and (later) other-agent ingestion.
  summarize/   LLM client and fallback strategies.
  render/      Show and review composition.
  config/      Loading of .worklog/config.yml and user overlay.
```

Suggested dependencies, kept minimal:

- `github.com/spf13/cobra` for the CLI (or `urfave/cli` — either is fine).
- `gopkg.in/yaml.v3` for frontmatter and config.
- Standard library `net/http` for the LLM API; no SDK required.
- `github.com/google/uuid` for the random suffix in note filenames.

Goal: `go build ./cmd/worklog` produces a single static binary with a small dependency tree, no cgo, cross-compiles cleanly.

## Configuration

`.worklog/config.yml`, committed:

```yaml
project: webapp

git:
  skip_merges: true
  skip_authors: ["dependabot[bot]", "renovate[bot]"]
  collapse_fixups: true

claude_code:
  enabled: true
  store_transcripts: false   # if true, copy to .worklog/transcripts/

agents:
  cursor: false
  aider: false
  cline: false

summarizer:
  provider: anthropic
  model: claude-haiku-4-5
  # API key read from env (ANTHROPIC_API_KEY), never committed.

reviews:
  auto_generate: false       # if true, write monthly review at month end.
```

A global config at `~/.config/worklog/config.yml` sets per-user defaults (API key path, summarizer model, attribution, etc.) that apply across every repo. The per-repo `.worklog/config.yml` overrides those defaults for that project. Built-in defaults fill in anything neither file specifies.

## Concurrency and merges

The per-event-file design means capture is conflict-free by construction. Two collaborators committing event files in parallel produce different paths; git merges them without thinking. Two `worklog note` invocations at the same instant get different timestamps and a random suffix.

Review files are the one place conflicts can happen, since they're per-month. The convention: treat them as generated artifacts, regenerate via `worklog review --month --write` in a single PR. Optionally a scheduled CI job owns review generation.

## Failure modes

- **Claude Code `SessionEnd` doesn't fire** on crashes or hard kills. `worklog sync` catches these by scanning the on-disk session directory.
- **Hook approval friction**: Claude Code prompts users to approve hooks on first session after a config change. The `worklog init` README should call this out so collaborators aren't surprised.
- **Missing API key** doesn't drop events; falls back to non-LLM summaries.
- **Slow summarization**: capture writes `summary: pending` and returns; `worklog resummarize` fills in later.

## Open questions

1. **Threads / epics.** Should we have a first-class field for grouping related events (a feature, a refactor, an incident)? Render-time clustering is probably enough for monthly summaries, but a manual `thread:` field in frontmatter is cheap to add and would make navigation easier. Defer until we feel the lack.
2. **Cross-project rollup.** Initial scope is strictly per-project. A `worklog rollup ~/code/*` that produces a personal weekly review across repos is an obvious phase-2 addition.
3. **Transcript storage.** Default off. For archival, opt in per repo; put transcripts under `.worklog/transcripts/` and consider git-lfs if they get large.
4. **Other agents.** Aider's `.aider.chat.history.md` is in-repo and easy to parse. Cursor and Cline use VS Code globalStorage, per-user, which makes per-project attribution awkward. Phase 1 ships with Claude Code + git only; adapters are added on demand.
5. **Summarizer cost.** Per-session summarization is cheap with a small model, but high-activity projects could see real numbers. Add a per-`sync`-run cap and a `--dry-run` option.
6. **Renames and edits.** What happens if someone edits an event file's frontmatter? The file remains authoritative — sync never overwrites existing files, only creates missing ones. This means manual annotations survive, but it also means a buggy capture writing wrong data needs to be fixed by hand.

## Phase 1 scope

To keep the first cut honest:

- `worklog init`, `sync`, `note`, `show`, `review`.
- Git ingestion with merge and bot filtering.
- Claude Code ingestion via `SessionEnd` hook plus reconciler.
- Anthropic summarizer with non-LLM fallback.
- Markdown event files and monthly review composition.

Out of scope for v1: other agents, GitHub PR ingestion, threads, cross-project rollup, transcript storage, scheduled review generation.

This is a thing you can build in a couple of weekends and start using on day one.
