# worklog

A per-project event log for software work. `git log` shows commits but
omits design discussions, coding-agent sessions, and decisions that
never produced a commit. `worklog` captures all of it — automatically,
via git and Claude Code hooks — into markdown files committed under
`.worklog/`, and renders day/week/month/year reviews from them.

See [`worklog-design.md`](worklog-design.md) for the full design.

## Getting started

```sh
# 1. Build and install the binary
go install ./cmd/worklog

# 2. From inside a git repo:
worklog init                    # creates .worklog/, installs hooks
export ANTHROPIC_API_KEY=...    # optional, enables LLM summaries
worklog sync                    # imports existing git history
worklog show --week             # render this week's events
```

That's it. From here, every commit you make and every Claude Code
session that ends in this repo will be captured into `.worklog/`. Run
`worklog show` whenever you want to read back.

## Installation

Requires Go 1.26+.

```sh
git clone https://github.com/mikethicke/worklog
cd worklog
go install ./cmd/worklog
```

This drops a single static binary at `$(go env GOPATH)/bin/worklog`.
Make sure that directory is on your `$PATH`.

Then, in each repo where you want a worklog:

```sh
cd path/to/your/repo
worklog init
```

`worklog init` is idempotent and does the following:

- Creates `.worklog/` with `config.yml`, `reviews/`, and `bin/`.
- Writes `.worklog/bin/capture-session`, the shim invoked by Claude Code.
- Installs `.git/hooks/post-commit` (backing up any existing hook to
  `post-commit.pre-worklog`).
- Adds a `SessionEnd` entry to `.claude/settings.json` (preserving any
  hooks already configured there).

Commit the entire `.worklog/` tree and `.claude/settings.json` so
collaborators get the same capture behavior. On their first Claude
Code session after pulling, **Claude Code will prompt them to approve
the new hook** — this is expected.

### Anthropic API key

LLM summaries call the Anthropic API. Set `ANTHROPIC_API_KEY` in your
shell. Without a key, capture still works — events are written with
deterministic fallback summaries (commit message for git, first user
prompt plus files-touched for Claude sessions). Run
`worklog resummarize` later to fill those in.

The key path can be set globally in `~/.config/worklog/config.yml` if
you'd rather not put it in your shell environment.

## Commands

### `worklog init`

Sets up `.worklog/`, installs the git hook, and registers the Claude
Code `SessionEnd` hook. Idempotent — safe to re-run.

### `worklog sync`

Reconciler. Walks git history and `~/.claude/projects/<encoded-cwd>/`,
writing event files for anything not already captured. Use this on
fresh clones, after a crashed session, or any time you suspect the
hooks missed something. Idempotent — re-running is always safe.

### `worklog note ["<text>"]`

Append a manual entry. With no argument, opens `$EDITOR` (defaulting
to `vi`) for a longer note.

```sh
worklog note "Decided to drop the OAuth flow in favor of magic links."
worklog note            # opens editor
```

### `worklog show [flags]`

Render a review to stdout. Defaults to `--week`.

```sh
worklog show --day
worklog show --week
worklog show --month
worklog show --year
worklog show --since 2026-05-01 --until 2026-05-07
worklog show --kind commit
```

`--day`/`--week`/`--month`/`--year` are resolution shortcuts.
`--since`/`--until` accept `YYYY-MM-DD` and override the range.
`--kind` filters by event kind (`commit`, `claude-session`, `note`,
…).

### `worklog review --week YYYY-Www | --month YYYY-MM | --year YYYY [--regenerate]`

Generate a clustered, LLM-summarized weekly, monthly, or yearly
review. Yearly reviews are composed from the twelve monthly reviews,
not from raw events; weekly and monthly reviews are built directly
from events in their range.

```sh
worklog review --week 2026-W19            # ISO week
worklog review --month 2026-05
worklog review --year 2026
worklog review --month 2026-05 --regenerate   # bypass cache, re-run LLM
```

By default, reviews are persisted to `.worklog/reviews/<period>.md`
and subsequent runs serve the cached file (this is configurable via
`reviews.persist` in `.worklog/config.yml`). Pass `--regenerate` to
overwrite the cached version with a fresh summarizer pass — useful
after backfilling events into a past period. Commit the persisted
reviews so they're stable and diff-able in PRs.

### `worklog resummarize`

Fills in any event files whose frontmatter still has `summary:
pending`. Useful after you set up an API key for the first time, or
after a slow batch where capture deferred summarization.

### `worklog ls [--kind KIND]`

List raw event files. Mostly for debugging.

### `worklog reset [--force]`

Delete every captured event and persisted review, leaving `config.yml`,
`bin/capture-session`, and `README.md` intact — i.e. the same state as
just after `worklog init`. Prompts for confirmation; `--force` skips
the prompt. After reset, run `worklog sync` to re-import history.

### `worklog capture-commit <sha>` and `worklog capture-claude`

Hidden — invoked by the git `post-commit` hook and the Claude Code
`SessionEnd` hook respectively. You shouldn't need to call these
directly.

## Configuration

worklog reads two files and merges them. The global config at
`~/.config/worklog/config.yml` sets per-user defaults (your preferred
summarizer model, API key path, attribution, etc.) that apply across
every repo. The per-repo config at `.worklog/config.yml` (committed)
overrides those defaults for a single project — use it for things the
whole team should share, like the project name and any team-wide git
filters. Built-in defaults fill in anything neither file specifies.

Key fields:

```yaml
project: webapp

# Attribution for notes and Claude sessions. Optional — if omitted,
# worklog uses your GitHub username (via `gh api user`), falling
# back to your OS user. Setting this trumps both.
author: alice

git:
  skip_merges: true
  skip_authors: ["dependabot[bot]", "renovate[bot]"]
  collapse_fixups: true

claude_code:
  enabled: true
  store_transcripts: false

summarizer:
  provider: anthropic
  model: claude-haiku-4-5
  api_key_env: ANTHROPIC_API_KEY

reviews:
  auto_generate: false
  persist: true             # write reviews to disk + serve cached on repeat
```
