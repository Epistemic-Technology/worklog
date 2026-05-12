// Package render produces human-readable views over the event log.
// "show" is fast and deterministic; "review" composes LLM summaries
// and is intended to be persisted to disk.
package render

import (
	"context"
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
	"gopkg.in/yaml.v3"
)

// Range is an inclusive time range with helpers for label rendering.
type Range struct {
	From  time.Time
	To    time.Time
	Label string
}

// DayRange returns the range covering the calendar day containing t.
func DayRange(t time.Time) Range {
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	return Range{From: start, To: start.AddDate(0, 0, 1).Add(-time.Nanosecond), Label: start.Format("2006-01-02")}
}

// WeekRange returns the ISO week (Mon-Sun) containing t.
func WeekRange(t time.Time) Range {
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	start := time.Date(t.Year(), t.Month(), t.Day()-(wd-1), 0, 0, 0, 0, t.Location())
	end := start.AddDate(0, 0, 7).Add(-time.Nanosecond)
	yr, wk := start.ISOWeek()
	return Range{From: start, To: end, Label: fmt.Sprintf("%d-W%02d", yr, wk)}
}

// MonthRange returns the calendar month containing t.
func MonthRange(t time.Time) Range {
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
	end := start.AddDate(0, 1, 0).Add(-time.Nanosecond)
	return Range{From: start, To: end, Label: start.Format("2006-01")}
}

// YearRange returns the calendar year containing t.
func YearRange(t time.Time) Range {
	start := time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location())
	end := start.AddDate(1, 0, 0).Add(-time.Nanosecond)
	return Range{From: start, To: end, Label: start.Format("2006")}
}

// ShowOptions controls the Show renderer.
type ShowOptions struct {
	Kind string // optional filter by event kind
}

// Show writes a deterministic markdown view to w. Events are grouped
// by day with one-liner summaries. No LLM calls.
func Show(w io.Writer, root string, r Range, opts ShowOptions) error {
	events, err := event.List(config.WorklogDir(root), r.From, r.To)
	if err != nil {
		return err
	}
	if opts.Kind != "" {
		events = filterKind(events, opts.Kind)
	}
	fmt.Fprintf(w, "# %s\n\n", r.Label)
	if len(events) == 0 {
		fmt.Fprintln(w, "_no events_")
		return nil
	}
	day := ""
	for _, ev := range events {
		d := ev.Time.Format("2006-01-02")
		if d != day {
			fmt.Fprintf(w, "\n## %s\n\n", d)
			day = d
		}
		fmt.Fprintf(w, "- %s `%s` %s%s\n", ev.Time.Format("15:04"), ev.Kind, oneLine(ev.Summary), authorSuffix(ev.Author))
	}
	return nil
}

// ReviewOptions controls cache and persistence behavior for the
// Review* functions.
type ReviewOptions struct {
	// Regenerate forces a fresh LLM pass even if a cached review
	// exists. The fresh result still respects Persist for writing.
	Regenerate bool
	// Persist controls whether generated reviews are written to
	// .worklog/reviews/ and whether cached files are read back.
	Persist bool
}

// ReviewWeekly composes a weekly review markdown body. By default, a
// cached file at .worklog/reviews/<YYYY-Www>.md is served when
// present; set opts.Regenerate to bypass it. Output is written to w
// (with a stderr hint on cache hits).
func ReviewWeekly(ctx context.Context, w io.Writer, root string, cfg config.Config, sum *summarize.Client, period time.Time, opts ReviewOptions) error {
	r := WeekRange(period)
	if cached, ok := serveCachedReview(w, root, r.Label, opts); ok {
		return cached
	}
	events, err := event.List(config.WorklogDir(root), r.From, r.To)
	if err != nil {
		return err
	}
	lines := make([]string, 0, len(events))
	for _, ev := range events {
		lines = append(lines, fmt.Sprintf("%s [%s]%s %s", ev.Time.Format("2006-01-02"), ev.Kind, authorTag(ev.Author), oneLine(ev.Summary)))
	}
	body := sum.WeeklyReview(ctx, r.Label, lines)
	return emitReview(w, root, r.Label, body, len(events), opts)
}

// ReviewMonthly composes a monthly review markdown body. By default,
// a cached file at .worklog/reviews/<YYYY-MM>.md is served when
// present; set opts.Regenerate to bypass it.
func ReviewMonthly(ctx context.Context, w io.Writer, root string, cfg config.Config, sum *summarize.Client, period time.Time, opts ReviewOptions) error {
	r := MonthRange(period)
	if cached, ok := serveCachedReview(w, root, r.Label, opts); ok {
		return cached
	}
	events, err := event.List(config.WorklogDir(root), r.From, r.To)
	if err != nil {
		return err
	}
	lines := make([]string, 0, len(events))
	for _, ev := range events {
		lines = append(lines, fmt.Sprintf("%s [%s]%s %s", ev.Time.Format("2006-01-02"), ev.Kind, authorTag(ev.Author), oneLine(ev.Summary)))
	}
	body := sum.MonthlyReview(ctx, r.Label, lines)
	return emitReview(w, root, r.Label, body, len(events), opts)
}

// ReviewYearly composes a yearly review by reading the twelve monthly
// review files (or generating them on the fly if missing). By
// default, a cached yearly file is served when present.
func ReviewYearly(ctx context.Context, w io.Writer, root string, cfg config.Config, sum *summarize.Client, period time.Time, opts ReviewOptions) error {
	r := YearRange(period)
	if cached, ok := serveCachedReview(w, root, r.Label, opts); ok {
		return cached
	}
	monthlies := map[string]string{}
	for m := 0; m < 12; m++ {
		t := time.Date(period.Year(), time.Month(m+1), 1, 0, 0, 0, 0, period.Location())
		label := t.Format("2006-01")
		monthly, err := loadOrBuildMonthly(ctx, root, cfg, sum, t)
		if err != nil {
			return err
		}
		if strings.TrimSpace(monthly) != "" {
			monthlies[label] = monthly
		}
	}
	body := sum.YearlyReview(ctx, r.Label, monthlies)
	return emitReview(w, root, r.Label, body, 0, opts)
}

// serveCachedReview writes the cached review file for label to w if
// one exists and the options allow it. Returns (writeError, true)
// when a cache hit was handled, or (_, false) to indicate the caller
// should generate the review.
func serveCachedReview(w io.Writer, root, label string, opts ReviewOptions) (error, bool) {
	if opts.Regenerate || !opts.Persist {
		return nil, false
	}
	path := filepath.Join(config.WorklogDir(root), "reviews", label+".md")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	if ts := frontmatterField(string(b), "generated_at"); ts != "" {
		fmt.Fprintf(os.Stderr, "worklog: serving cached review (generated %s) — pass --regenerate to refresh\n", ts)
	} else {
		fmt.Fprintln(os.Stderr, "worklog: serving cached review — pass --regenerate to refresh")
	}
	_, werr := w.Write(b)
	return werr, true
}

// emitReview writes the (frontmatter + body) to w and, if opts.Persist
// is true, also persists it to .worklog/reviews/<label>.md.
func emitReview(w io.Writer, root, label, body string, eventCount int, opts ReviewOptions) error {
	fm := map[string]any{
		"period":       label,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}
	if eventCount > 0 {
		fm["event_count"] = eventCount
	}
	out := frontmatterBlock(fm) + body
	if _, err := io.WriteString(w, out); err != nil {
		return err
	}
	if opts.Persist {
		return persistReview(root, label, out)
	}
	return nil
}

func loadOrBuildMonthly(ctx context.Context, root string, cfg config.Config, sum *summarize.Client, period time.Time) (string, error) {
	label := period.Format("2006-01")
	path := filepath.Join(config.WorklogDir(root), "reviews", label+".md")
	if b, err := os.ReadFile(path); err == nil {
		// Strip frontmatter for cleaner composition.
		return stripFrontmatter(string(b)), nil
	}
	r := MonthRange(period)
	events, err := event.List(config.WorklogDir(root), r.From, r.To)
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return "", nil
	}
	lines := make([]string, 0, len(events))
	for _, ev := range events {
		lines = append(lines, fmt.Sprintf("%s [%s]%s %s", ev.Time.Format("2006-01-02"), ev.Kind, authorTag(ev.Author), oneLine(ev.Summary)))
	}
	return sum.MonthlyReview(ctx, label, lines), nil
}

func persistReview(root, label, body string) error {
	dir := filepath.Join(config.WorklogDir(root), "reviews")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, label+".md"), []byte(body), 0o644)
}

// Resummarize walks all events with summary == pending and re-runs
// the summarizer. Returns the count of events filled in.
func Resummarize(ctx context.Context, root string, sum *summarize.Client) (int, error) {
	if !sum.Configured() {
		return 0, fmt.Errorf("render: summarizer not configured (set ANTHROPIC_API_KEY)")
	}
	events, err := event.List(config.WorklogDir(root), time.Time{}, time.Time{})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, ev := range events {
		if ev.Summary != summarize.PendingMarker {
			continue
		}
		var newSummary, newBody string
		switch ev.Kind {
		case event.KindCommit:
			newSummary, newBody = sum.Commit(ctx, ev.Body, "")
		case event.KindClaudeSession, event.KindAgentSession:
			newSummary, newBody = sum.Session(ctx, "", ev.Body, nil)
		default:
			continue
		}
		if newSummary == "" || newSummary == summarize.PendingMarker {
			continue
		}
		ev.Summary = newSummary
		if newBody != "" {
			ev.Body = newBody
		}
		if err := event.Write(ev.Path+".tmp", ev.Frontmatter, ev.Body); err != nil {
			return count, err
		}
		if err := os.Rename(ev.Path+".tmp", ev.Path); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// ListEvents prints the raw event filenames matching kind (or all).
func ListEvents(w io.Writer, root, kind string) error {
	events, err := event.List(config.WorklogDir(root), time.Time{}, time.Time{})
	if err != nil {
		return err
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Time.Before(events[j].Time) })
	for _, ev := range events {
		if kind != "" && ev.Kind != kind {
			continue
		}
		author := ev.Author
		if author == "" {
			author = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", ev.Time.Format(time.RFC3339), ev.Kind, author, oneLine(ev.Summary))
	}
	return nil
}

func filterKind(events []*event.Event, kind string) []*event.Event {
	out := events[:0]
	for _, ev := range events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func authorSuffix(a string) string {
	if a == "" {
		return ""
	}
	return " — " + a
}

func authorTag(a string) string {
	if a == "" {
		return ""
	}
	return " (" + a + ")"
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func frontmatterBlock(fields map[string]any) string {
	b, err := yaml.Marshal(fields)
	if err != nil {
		return ""
	}
	return "---\n" + string(b) + "---\n\n"
}

func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	rest := s[4:]
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		return s
	}
	return strings.TrimLeft(rest[idx+5:], "\n")
}

// frontmatterField extracts a top-level scalar field from the YAML
// frontmatter of s. Returns "" if absent or malformed. Tolerates
// values with or without surrounding quotes.
func frontmatterField(s, key string) string {
	if !strings.HasPrefix(s, "---\n") {
		return ""
	}
	rest := s[4:]
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		return ""
	}
	for _, line := range strings.Split(rest[:idx], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		return v
	}
	return ""
}
