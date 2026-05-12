// Package event handles read and write of worklog event files.
//
// An event is a markdown file with YAML frontmatter living under
// .worklog/YYYY-MM/<timestamp>-<kind>-<shortid>.md. The filename
// embeds a stable source id so the same upstream commit or session
// always maps to the same path — capture is idempotent by stat.
package event

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	KindCommit        = "commit"
	KindClaudeSession = "claude-session"
	KindAgentSession  = "agent-session"
	KindNote          = "note"
	KindPR            = "pr"
	KindTag           = "tag"
)

// Frontmatter mirrors the YAML block at the top of an event file.
type Frontmatter struct {
	Time      time.Time `yaml:"time"`
	Kind      string    `yaml:"kind"`
	Author    string    `yaml:"author,omitempty"`
	Refs      []string  `yaml:"refs,omitempty"`
	Summary   string    `yaml:"summary,omitempty"`
	Tags      []string  `yaml:"tags,omitempty"`
	SessionID string    `yaml:"session_id,omitempty"`
	Thread    string    `yaml:"thread,omitempty"`
}

// Event is a parsed event file. Path is set when read from disk.
type Event struct {
	Frontmatter
	Body string
	Path string
}

// Filename returns the canonical filename for an event, given its
// timestamp, kind, and a short source id. Colons in the time are
// replaced with dashes to stay portable across filesystems.
func Filename(t time.Time, kind, shortID string) string {
	ts := t.Format("2006-01-02T15-04-05")
	return fmt.Sprintf("%s-%s-%s.md", ts, kind, shortID)
}

// MonthDir returns the YYYY-MM shard directory under root.
func MonthDir(root string, t time.Time) string {
	return filepath.Join(root, t.Format("2006-01"))
}

// Path returns the full path for an event under root.
func Path(root string, t time.Time, kind, shortID string) string {
	return filepath.Join(MonthDir(root, t), Filename(t, kind, shortID))
}

// Exists reports whether an event file at path is already on disk.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Write atomically writes an event file. Parent directories are created.
// Returns os.ErrExist if the target file already exists — capture is
// expected to short-circuit on Exists before calling Write.
func Write(path string, fm Frontmatter, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if fm.Time.IsZero() {
		return fmt.Errorf("event: time is required")
	}
	if fm.Kind == "" {
		return fmt.Errorf("event: kind is required")
	}

	yamlBytes, err := yaml.Marshal(&fm)
	if err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.Write(yamlBytes)
	b.WriteString("---\n")
	if body != "" {
		if !strings.HasPrefix(body, "\n") {
			b.WriteString("\n")
		}
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteString("\n")
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".worklog-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	// O_EXCL semantics: refuse to clobber an existing event file.
	if _, err := os.Stat(path); err == nil {
		os.Remove(tmpName)
		return os.ErrExist
	}
	return os.Rename(tmpName, path)
}

// Read parses a single event file.
func Read(path string) (*Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	if !scanner.Scan() || scanner.Text() != "---" {
		return nil, fmt.Errorf("event: %s: missing frontmatter delimiter", path)
	}

	var yamlBuf strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "---" {
			break
		}
		yamlBuf.WriteString(line)
		yamlBuf.WriteString("\n")
	}

	var bodyBuf strings.Builder
	first := true
	for scanner.Scan() {
		if first {
			first = false
			if scanner.Text() == "" {
				continue
			}
		}
		bodyBuf.WriteString(scanner.Text())
		bodyBuf.WriteString("\n")
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	var fm Frontmatter
	if err := yaml.Unmarshal([]byte(yamlBuf.String()), &fm); err != nil {
		return nil, fmt.Errorf("event: %s: %w", path, err)
	}
	return &Event{Frontmatter: fm, Body: bodyBuf.String(), Path: path}, nil
}

// List walks root and returns events whose time falls within [from, to].
// A zero from or to is treated as unbounded on that side. Results are
// sorted ascending by time.
func List(root string, from, to time.Time) ([]*Event, error) {
	var out []*Event
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Only descend into YYYY-MM shard directories.
			if path == root {
				return nil
			}
			if !isMonthShard(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		// Only read files inside a YYYY-MM shard.
		if !isMonthShard(filepath.Base(filepath.Dir(path))) {
			return nil
		}
		ev, err := Read(path)
		if err != nil {
			return err
		}
		if !from.IsZero() && ev.Time.Before(from) {
			return nil
		}
		if !to.IsZero() && ev.Time.After(to) {
			return nil
		}
		out = append(out, ev)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// IsMonthShard reports whether name is a YYYY-MM shard directory.
func IsMonthShard(name string) bool {
	return isMonthShard(name)
}

// isMonthShard reports whether name is a YYYY-MM shard directory.
func isMonthShard(name string) bool {
	if len(name) != 7 || name[4] != '-' {
		return false
	}
	for i, r := range name {
		if i == 4 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// FindByRef returns the path to the first event whose refs include ref,
// or empty if none. Used as a slow fallback when an event was renamed
// or the filename heuristic doesn't match.
func FindByRef(root, ref string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if !isMonthShard(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		if !isMonthShard(filepath.Base(filepath.Dir(path))) {
			return nil
		}
		ev, err := Read(path)
		if err != nil {
			return nil
		}
		for _, r := range ev.Refs {
			if r == ref {
				found = path
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found, err
}
