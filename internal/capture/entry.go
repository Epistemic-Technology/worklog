package capture

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mikethicke/worklog/internal/config"
	"github.com/mikethicke/worklog/internal/event"
)

// EntryInput is the structured form of an event creation request,
// suitable for unmarshalling from JSON. Fields mirror event.Frontmatter
// plus a Body field for the markdown body. Time is a pointer so
// "absent" is distinguishable from the zero value.
type EntryInput struct {
	Time      *time.Time `json:"time,omitempty"`
	EndTime   *time.Time `json:"end_time,omitempty"`
	Kind      string     `json:"kind"`
	Author    string     `json:"author,omitempty"`
	Refs      []string   `json:"refs,omitempty"`
	Summary   string     `json:"summary,omitempty"`
	Tags      []string   `json:"tags,omitempty"`
	SessionID string     `json:"session_id,omitempty"`
	Thread    string     `json:"thread,omitempty"`
	Body      string     `json:"body,omitempty"`
}

var kindPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateKind reports whether s is a usable event kind. Kinds appear
// in filenames, so we restrict them to a lowercase slug.
func ValidateKind(s string) error {
	if !kindPattern.MatchString(s) {
		return fmt.Errorf("invalid kind %q: must match [a-z0-9][a-z0-9-]*", s)
	}
	return nil
}

// Entry writes an arbitrary event under .worklog/. The kind is
// validated as a slug. Time defaults to now if not set. Summary falls
// back to the first line of the body. Author falls back to the
// resolved attribution from config.
func Entry(root string, in EntryInput, defaultAuthor string) (string, error) {
	if err := ValidateKind(in.Kind); err != nil {
		return "", err
	}
	t := time.Now()
	if in.Time != nil && !in.Time.IsZero() {
		t = *in.Time
	}
	short := strings.ReplaceAll(uuid.NewString(), "-", "")[:6]
	path := event.Path(config.WorklogDir(root), t, in.Kind, short)

	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		summary = firstLine(in.Body)
	}
	author := in.Author
	if author == "" {
		author = defaultAuthor
	}

	fm := event.Frontmatter{
		Time:      t,
		Kind:      in.Kind,
		Author:    author,
		Refs:      in.Refs,
		Summary:   summary,
		Tags:      in.Tags,
		SessionID: in.SessionID,
		Thread:    in.Thread,
	}
	if in.EndTime != nil && !in.EndTime.IsZero() {
		fm.EndTime = *in.EndTime
	}
	if err := event.Write(path, fm, in.Body); err != nil {
		return "", err
	}
	return path, nil
}
