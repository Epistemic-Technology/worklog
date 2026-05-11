package capture

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mikethicke/worklog/internal/config"
	"github.com/mikethicke/worklog/internal/event"
)

// Note writes a manual entry under .worklog/. The body may be empty;
// the caller usually has the user paste a multi-line markdown body.
func Note(root, text, author string) (string, error) {
	now := time.Now()
	short := strings.ReplaceAll(uuid.NewString(), "-", "")[:6]
	path := event.Path(config.WorklogDir(root), now, "note", short)
	summary := firstLine(text)
	if summary == "" {
		summary = "(note)"
	}
	fm := event.Frontmatter{
		Time:    now,
		Kind:    event.KindNote,
		Author:  author,
		Summary: summary,
	}
	if err := event.Write(path, fm, text); err != nil {
		return "", err
	}
	return path, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	if len(s) > 200 {
		return s[:199] + "…"
	}
	return s
}
