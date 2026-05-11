// Package capture writes event files from upstream sources — git
// commits, Claude Code sessions, and (later) other agents.
package capture

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/mikethicke/worklog/internal/config"
	"github.com/mikethicke/worklog/internal/event"
	"github.com/mikethicke/worklog/internal/summarize"
)

// CommitInfo is the fields we pull from `git show` for an event.
type CommitInfo struct {
	SHA        string
	Time       time.Time
	AuthorName string
	AuthorEmail string
	Subject    string
	Body       string
	Diffstat   string
	IsMerge    bool
}

// GitCommit reads a single commit by SHA and returns its event-relevant fields.
func GitCommit(ctx context.Context, repo, sha string) (CommitInfo, error) {
	var info CommitInfo
	out, err := runGit(ctx, repo, "show", "-s",
		"--format=%H%n%aI%n%aN%n%aE%n%P%n%s%n%B%x00", sha)
	if err != nil {
		return info, err
	}
	// %B can contain newlines; we use NUL as terminator.
	parts := strings.SplitN(string(out), "\x00", 2)
	header := parts[0]
	lines := strings.SplitN(header, "\n", 7)
	if len(lines) < 7 {
		return info, fmt.Errorf("capture: unexpected git show output for %s", sha)
	}
	info.SHA = strings.TrimSpace(lines[0])
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(lines[1]))
	if err != nil {
		return info, fmt.Errorf("capture: parsing commit time: %w", err)
	}
	info.Time = t
	info.AuthorName = strings.TrimSpace(lines[2])
	info.AuthorEmail = strings.TrimSpace(lines[3])
	parents := strings.Fields(strings.TrimSpace(lines[4]))
	info.IsMerge = len(parents) > 1
	info.Subject = strings.TrimSpace(lines[5])
	info.Body = strings.TrimRight(lines[6], "\n")

	stat, err := runGit(ctx, repo, "show", "--stat", "--format=", sha)
	if err == nil {
		info.Diffstat = strings.TrimSpace(string(stat))
	}
	return info, nil
}

// CaptureCommit writes an event file for a single SHA, applying
// configured filters. It is safe to call twice for the same SHA;
// the second call short-circuits via the file existence check.
func CaptureCommit(ctx context.Context, root string, cfg config.Config, sum *summarize.Client, sha string) (string, error) {
	info, err := GitCommit(ctx, root, sha)
	if err != nil {
		return "", err
	}
	if info.IsMerge && cfg.Git.SkipMerges {
		return "", nil
	}
	if matchesAuthor(info.AuthorName, info.AuthorEmail, cfg.Git.SkipAuthors) {
		return "", nil
	}
	short := info.SHA
	if len(short) > 7 {
		short = short[:7]
	}
	path := event.Path(config.WorklogDir(root), info.Time, "git", short)
	if event.Exists(path) {
		return "", nil
	}
	message := info.Body
	if message == "" {
		message = info.Subject
	}
	summary, body := sum.Commit(ctx, message, info.Diffstat)
	if summary == "" {
		summary = summarize.PendingMarker
	}
	fm := event.Frontmatter{
		Time:    info.Time,
		Kind:    event.KindCommit,
		Author:  info.AuthorName,
		Refs:    []string{"git:" + info.SHA},
		Summary: summary,
	}
	if err := event.Write(path, fm, body); err != nil {
		return "", err
	}
	return path, nil
}

// SyncGit walks `git log --no-merges` and captures any commits that
// don't already have event files.
func SyncGit(ctx context.Context, root string, cfg config.Config, sum *summarize.Client) (int, error) {
	args := []string{"log", "--format=%H"}
	if cfg.Git.SkipMerges {
		args = append(args, "--no-merges")
	}
	out, err := runGit(ctx, root, args...)
	if err != nil {
		// A fresh repo with zero commits is fine — return 0.
		if strings.Contains(err.Error(), "does not have any commits yet") {
			return 0, nil
		}
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		sha := strings.TrimSpace(line)
		if sha == "" {
			continue
		}
		path, err := CaptureCommit(ctx, root, cfg, sum, sha)
		if err != nil {
			return count, fmt.Errorf("capture commit %s: %w", sha, err)
		}
		if path != "" {
			count++
		}
	}
	return count, nil
}

func runGit(ctx context.Context, repo string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w (stderr=%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func matchesAuthor(name, email string, skip []string) bool {
	for _, s := range skip {
		if s == "" {
			continue
		}
		if strings.EqualFold(name, s) || strings.EqualFold(email, s) {
			return true
		}
	}
	return false
}
