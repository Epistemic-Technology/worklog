// Package config loads worklog configuration from the repo-committed
// .worklog/config.yml and a user-level overlay at
// ~/.config/worklog/config.yml.
package config

import (
	"errors"
	"os"
	"path/filepath"

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
}

type Config struct {
	Project    string     `yaml:"project"`
	Git        Git        `yaml:"git"`
	ClaudeCode ClaudeCode `yaml:"claude_code"`
	Agents     Agents     `yaml:"agents"`
	Summarizer Summarizer `yaml:"summarizer"`
	Reviews    Reviews    `yaml:"reviews"`
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
	}
}

// Load reads .worklog/config.yml at root and merges any user overlay
// at ~/.config/worklog/config.yml on top of it.
func Load(root string) (Config, error) {
	cfg := Default()
	repoPath := filepath.Join(root, ".worklog", "config.yml")
	if err := mergeFromFile(&cfg, repoPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return cfg, err
	}
	if home, err := os.UserHomeDir(); err == nil {
		overlay := filepath.Join(home, ".config", "worklog", "config.yml")
		if err := mergeFromFile(&cfg, overlay); err != nil && !errors.Is(err, os.ErrNotExist) {
			return cfg, err
		}
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
