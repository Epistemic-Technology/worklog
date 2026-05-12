// Package config loads worklog configuration. The global config at
// ~/.config/worklog/config.yml sets per-user defaults; the repo-committed
// .worklog/config.yml overrides those defaults for a single project.
package config

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Git struct {
	SkipMerges     bool     `yaml:"skip_merges"`
	SkipAuthors    []string `yaml:"skip_authors"`
	CollapseFixups bool     `yaml:"collapse_fixups"`
}

type ClaudeCode struct {
	Enabled          bool `yaml:"enabled"`
	StoreTranscripts bool `yaml:"store_transcripts"`
}

type Agents struct {
	Cursor bool `yaml:"cursor"`
	Aider  bool `yaml:"aider"`
	Cline  bool `yaml:"cline"`
}

type Summarizer struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	APIKey   string `yaml:"api_key,omitempty"`
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
}

type Reviews struct {
	AutoGenerate bool `yaml:"auto_generate"`
	// Persist controls whether generated reviews are written to
	// .worklog/reviews/ and whether subsequent runs read from that
	// cache instead of regenerating. Default true.
	Persist bool `yaml:"persist"`
}

type Config struct {
	Project    string     `yaml:"project"`
	Author     string     `yaml:"author,omitempty"`
	// AuthorAliases maps any author identifier (a git name, a git
	// email, an OS username, a GitHub login) to its canonical
	// attribution. Used to collapse "mikethicke" (Claude session,
	// OS user) and "Mike Thicke" (git commit author) into a single
	// person across event kinds.
	AuthorAliases map[string]string `yaml:"author_aliases,omitempty"`
	Git           Git               `yaml:"git"`
	ClaudeCode    ClaudeCode        `yaml:"claude_code"`
	Agents        Agents            `yaml:"agents"`
	Summarizer    Summarizer        `yaml:"summarizer"`
	Reviews       Reviews           `yaml:"reviews"`
}

// Default returns the config that ships with a new project.
func Default() Config {
	return Config{
		Git: Git{
			SkipMerges:     true,
			SkipAuthors:    []string{"dependabot[bot]", "renovate[bot]"},
			CollapseFixups: true,
		},
		ClaudeCode: ClaudeCode{
			Enabled: true,
		},
		Summarizer: Summarizer{
			Provider:  "anthropic",
			Model:     "claude-haiku-4-5",
			APIKeyEnv: "ANTHROPIC_API_KEY",
		},
		Reviews: Reviews{
			Persist: true,
		},
	}
}

// Load merges three layers in order: built-in defaults, the global
// config at ~/.config/worklog/config.yml, and the repo-committed
// .worklog/config.yml. Later layers override earlier ones, so the
// repo config has final say.
func Load(root string) (Config, error) {
	cfg := Default()
	if home, err := os.UserHomeDir(); err == nil {
		global := filepath.Join(home, ".config", "worklog", "config.yml")
		if err := mergeFromFile(&cfg, global); err != nil && !errors.Is(err, os.ErrNotExist) {
			return cfg, err
		}
	}
	repoPath := filepath.Join(root, ".worklog", "config.yml")
	if err := mergeFromFile(&cfg, repoPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return cfg, err
	}
	return cfg, nil
}

func mergeFromFile(cfg *Config, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, cfg)
}

// ResolveAPIKey returns the summarizer API key, preferring the
// explicit APIKey field, then the env var named by APIKeyEnv,
// then ANTHROPIC_API_KEY as a final fallback.
func (s Summarizer) ResolveAPIKey() string {
	if s.APIKey != "" {
		return s.APIKey
	}
	if s.APIKeyEnv != "" {
		if v := os.Getenv(s.APIKeyEnv); v != "" {
			return v
		}
	}
	return os.Getenv("ANTHROPIC_API_KEY")
}

// WorklogDir returns the .worklog directory under root.
func WorklogDir(root string) string {
	return filepath.Join(root, ".worklog")
}

// ResolveAuthor returns the attribution to record on notes and
// agent-session events. Precedence: explicit `author:` in config →
// GitHub username (via `gh api user`, if installed and authed) →
// OS user. The result is run through Canonicalize so attribution
// stays unified across event kinds. Returns "" only if every
// source is empty, which would be a very unusual environment.
func (c Config) ResolveAuthor() string {
	if s := strings.TrimSpace(c.Author); s != "" {
		return c.Canonicalize(s)
	}
	if s := githubUser(); s != "" {
		return c.Canonicalize(s)
	}
	return c.Canonicalize(osUser())
}

// Canonicalize maps any of the given identifiers (typically a git
// author name and email, or a single resolved username) through
// AuthorAliases. The first candidate that matches an alias key
// yields its canonical value; otherwise the first non-empty
// candidate is returned unchanged. Matching is case-insensitive
// so a single alias covers "mikethicke" and "MikeThicke" alike.
func (c Config) Canonicalize(candidates ...string) string {
	for _, cand := range candidates {
		key := strings.TrimSpace(cand)
		if key == "" {
			continue
		}
		for k, v := range c.AuthorAliases {
			if strings.EqualFold(strings.TrimSpace(k), key) && v != "" {
				return v
			}
		}
	}
	for _, cand := range candidates {
		if s := strings.TrimSpace(cand); s != "" {
			return s
		}
	}
	return ""
}

func githubUser() string {
	// --cache makes repeat calls near-instant. Errors (gh not
	// installed, not authed, offline) fall through to the next source.
	cmd := exec.Command("gh", "api", "user", "--cache", "24h", "--jq", ".login")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}
