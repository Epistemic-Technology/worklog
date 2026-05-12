package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mikethicke/worklog/internal/capture"
	"github.com/mikethicke/worklog/internal/config"
	"github.com/mikethicke/worklog/internal/event"
	"github.com/mikethicke/worklog/internal/render"
	"github.com/mikethicke/worklog/internal/summarize"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "worklog",
		Short:         "Per-project event log for software work",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(
		newInitCmd(),
		newCaptureCommitCmd(),
		newCaptureClaudeCmd(),
		newNoteCmd(),
		newEntryCmd(),
		newSyncCmd(),
		newResummarizeCmd(),
		newShowCmd(),
		newReviewCmd(),
		newLsCmd(),
		newResetCmd(),
	)
	return root
}

// repoRoot finds the git toplevel for cwd. If the current directory
// isn't inside a git repo, falls back to cwd.
func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	return os.Getwd()
}

func loadCtx() (context.Context, string, config.Config, *summarize.Client, error) {
	ctx := context.Background()
	root, err := repoRoot()
	if err != nil {
		return ctx, "", config.Config{}, nil, err
	}
	cfg, err := config.Load(root)
	if err != nil {
		return ctx, root, cfg, nil, err
	}
	sum := summarize.New(cfg.Summarizer.Provider, cfg.Summarizer.ResolveAPIKey(), cfg.Summarizer.Model)
	return ctx, root, cfg, sum, nil
}

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Set up .worklog/, install git hook, write capture-session shim",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := repoRoot()
			if err != nil {
				return err
			}
			return runInit(root)
		},
	}
}

func newCaptureCommitCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "capture-commit <sha>",
		Short:  "Capture a single git commit (invoked by post-commit hook)",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, root, cfg, sum, err := loadCtx()
			if err != nil {
				return err
			}
			path, err := capture.CaptureCommit(ctx, root, cfg, sum, args[0])
			if err != nil {
				return err
			}
			if path != "" {
				fmt.Fprintln(os.Stderr, "worklog:", filepath.Base(path))
			}
			return nil
		},
	}
}

func newCaptureClaudeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "capture-claude",
		Short:  "Capture a Claude Code session (reads JSON from stdin)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, root, cfg, sum, err := loadCtx()
			if err != nil {
				return err
			}
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			var p capture.ClaudeSessionPayload
			if err := json.Unmarshal(raw, &p); err != nil {
				return fmt.Errorf("parsing hook payload: %w", err)
			}
			path, err := capture.CaptureClaudeFromPayload(ctx, root, cfg, sum, p)
			if err != nil {
				return err
			}
			if path != "" {
				fmt.Fprintln(os.Stderr, "worklog:", filepath.Base(path))
			}
			return nil
		},
	}
}

func newNoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "note [text]",
		Short: "Add a manual entry",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := repoRoot()
			if err != nil {
				return err
			}
			cfg, err := config.Load(root)
			if err != nil {
				return err
			}
			var text string
			if len(args) > 0 {
				text = strings.Join(args, " ")
			} else {
				text, err = openEditor(root)
				if err != nil {
					return err
				}
				if strings.TrimSpace(text) == "" {
					return errors.New("note is empty; aborting")
				}
			}
			path, err := capture.Note(root, text, cfg.ResolveAuthor())
			if err != nil {
				return err
			}
			fmt.Println(path)
			return nil
		},
	}
}

func newEntryCmd() *cobra.Command {
	var (
		jsonMode  bool
		summary   string
		tags      []string
		refs      []string
		thread    string
		sessionID string
		author    string
		when      string
	)
	cmd := &cobra.Command{
		Use:   "entry <kind> [text...]",
		Short: "Add an event of any kind",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := repoRoot()
			if err != nil {
				return err
			}
			cfg, err := config.Load(root)
			if err != nil {
				return err
			}

			var in capture.EntryInput
			if jsonMode {
				raw, err := io.ReadAll(os.Stdin)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(raw, &in); err != nil {
					return fmt.Errorf("parsing json: %w", err)
				}
				// Positional kind, if given, overrides any kind in JSON.
				if len(args) > 0 {
					in.Kind = args[0]
				}
			} else {
				if len(args) == 0 {
					return errors.New("entry: kind is required (e.g. `worklog entry decision \"...\"`)")
				}
				in.Kind = args[0]
				if len(args) > 1 {
					in.Body = strings.Join(args[1:], " ")
				} else {
					text, err := openEditor(root)
					if err != nil {
						return err
					}
					if strings.TrimSpace(text) == "" {
						return errors.New("entry body is empty; aborting")
					}
					in.Body = text
				}
			}

			if summary != "" {
				in.Summary = summary
			}
			if len(tags) > 0 {
				in.Tags = tags
			}
			if len(refs) > 0 {
				in.Refs = refs
			}
			if thread != "" {
				in.Thread = thread
			}
			if sessionID != "" {
				in.SessionID = sessionID
			}
			if author != "" {
				in.Author = author
			}
			if when != "" {
				t, err := time.Parse(time.RFC3339, when)
				if err != nil {
					return fmt.Errorf("--time must be RFC3339: %w", err)
				}
				in.Time = &t
			}

			path, err := capture.Entry(root, in, cfg.ResolveAuthor())
			if err != nil {
				return err
			}
			fmt.Println(path)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonMode, "json", false, "read JSON {kind, summary, body, ...} from stdin")
	cmd.Flags().StringVar(&summary, "summary", "", "override summary")
	cmd.Flags().StringSliceVar(&tags, "tags", nil, "comma-separated tags")
	cmd.Flags().StringSliceVar(&refs, "refs", nil, "comma-separated refs")
	cmd.Flags().StringVar(&thread, "thread", "", "thread name")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "session id")
	cmd.Flags().StringVar(&author, "author", "", "override author")
	cmd.Flags().StringVar(&when, "time", "", "override event time (RFC3339)")
	return cmd
}

func newSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Reconcile: write event files for any commits/sessions not yet captured",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, root, cfg, sum, err := loadCtx()
			if err != nil {
				return err
			}
			gitCount, err := capture.SyncGit(ctx, root, cfg, sum)
			if err != nil {
				return fmt.Errorf("git sync: %w", err)
			}
			ccCount, err := capture.SyncClaude(ctx, root, cfg, sum)
			if err != nil {
				return fmt.Errorf("claude sync: %w", err)
			}
			fmt.Fprintf(os.Stderr, "git: %d new, claude: %d new\n", gitCount, ccCount)
			return nil
		},
	}
}

func newResummarizeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resummarize",
		Short: "Fill in pending summaries via the LLM",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, root, _, sum, err := loadCtx()
			if err != nil {
				return err
			}
			n, err := render.Resummarize(ctx, root, sum)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "resummarized %d events\n", n)
			return nil
		},
	}
}

func newShowCmd() *cobra.Command {
	var (
		day, week, month, year bool
		since, until           string
		kind                   string
	)
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Render a review to stdout",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := repoRoot()
			if err != nil {
				return err
			}
			now := time.Now()
			var r render.Range
			switch {
			case day:
				r = render.DayRange(now)
			case week:
				r = render.WeekRange(now)
			case month:
				r = render.MonthRange(now)
			case year:
				r = render.YearRange(now)
			default:
				r = render.WeekRange(now)
			}
			if since != "" {
				t, err := parseDate(since)
				if err != nil {
					return err
				}
				r.From = t
			}
			if until != "" {
				t, err := parseDate(until)
				if err != nil {
					return err
				}
				r.To = t.Add(24*time.Hour - time.Nanosecond)
			}
			return render.Show(os.Stdout, root, r, render.ShowOptions{Kind: kind})
		},
	}
	cmd.Flags().BoolVar(&day, "day", false, "show today")
	cmd.Flags().BoolVar(&week, "week", false, "show this week (default)")
	cmd.Flags().BoolVar(&month, "month", false, "show this month")
	cmd.Flags().BoolVar(&year, "year", false, "show this year")
	cmd.Flags().StringVar(&since, "since", "", "start date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&until, "until", "", "end date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&kind, "kind", "", "filter by event kind")
	return cmd
}

func newReviewCmd() *cobra.Command {
	var (
		week       string
		month      string
		year       string
		regenerate bool
	)
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Generate (and cache) a weekly, monthly, or yearly review",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, root, cfg, sum, err := loadCtx()
			if err != nil {
				return err
			}
			opts := render.ReviewOptions{
				Regenerate: regenerate,
				Persist:    cfg.Reviews.Persist,
			}
			switch {
			case week != "":
				t, err := parseISOWeek(week)
				if err != nil {
					return fmt.Errorf("--week must be YYYY-Www: %w", err)
				}
				return render.ReviewWeekly(ctx, os.Stdout, root, cfg, sum, t, opts)
			case month != "":
				t, err := time.Parse("2006-01", month)
				if err != nil {
					return fmt.Errorf("--month must be YYYY-MM: %w", err)
				}
				return render.ReviewMonthly(ctx, os.Stdout, root, cfg, sum, t, opts)
			case year != "":
				t, err := time.Parse("2006", year)
				if err != nil {
					return fmt.Errorf("--year must be YYYY: %w", err)
				}
				return render.ReviewYearly(ctx, os.Stdout, root, cfg, sum, t, opts)
			default:
				return errors.New("specify --week YYYY-Www, --month YYYY-MM, or --year YYYY")
			}
		},
	}
	cmd.Flags().StringVar(&week, "week", "", "weekly review (YYYY-Www, ISO week)")
	cmd.Flags().StringVar(&month, "month", "", "monthly review (YYYY-MM)")
	cmd.Flags().StringVar(&year, "year", "", "yearly review (YYYY)")
	cmd.Flags().BoolVar(&regenerate, "regenerate", false, "bypass cached review and re-run the summarizer")
	return cmd
}

// parseISOWeek parses a YYYY-Www label (e.g. "2026-W19") into a time
// pointing at the Monday of that ISO week, in the local timezone.
func parseISOWeek(s string) (time.Time, error) {
	parts := strings.SplitN(s, "-W", 2)
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("expected YYYY-Www, got %q", s)
	}
	year, err := strconv.Atoi(parts[0])
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid year: %w", err)
	}
	week, err := strconv.Atoi(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid week: %w", err)
	}
	if week < 1 || week > 53 {
		return time.Time{}, fmt.Errorf("week must be 1-53, got %d", week)
	}
	// Jan 4 is always in ISO week 1. Walk back to its Monday, then
	// step forward by (week-1) weeks.
	jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.Local)
	wd := int(jan4.Weekday())
	if wd == 0 {
		wd = 7
	}
	weekOneMonday := jan4.AddDate(0, 0, -(wd - 1))
	return weekOneMonday.AddDate(0, 0, (week-1)*7), nil
}

func newLsCmd() *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List raw event files",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := repoRoot()
			if err != nil {
				return err
			}
			return render.ListEvents(os.Stdout, root, kind)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "filter by event kind")
	return cmd
}

func newResetCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Delete all captured events and reviews (back to post-init state)",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := repoRoot()
			if err != nil {
				return err
			}
			wd := config.WorklogDir(root)
			shards, reviews, err := resetTargets(wd)
			if err != nil {
				return err
			}
			if len(shards) == 0 && len(reviews) == 0 {
				fmt.Fprintln(os.Stderr, "nothing to reset")
				return nil
			}
			if !force {
				fmt.Fprintf(os.Stderr, "Will delete %d event shard(s) and %d review file(s) under %s.\n", len(shards), len(reviews), wd)
				fmt.Fprint(os.Stderr, "Continue? [y/N] ")
				reader := bufio.NewReader(os.Stdin)
				answer, _ := reader.ReadString('\n')
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y") {
					return errors.New("aborted")
				}
			}
			for _, p := range shards {
				if err := os.RemoveAll(p); err != nil {
					return err
				}
			}
			for _, p := range reviews {
				if err := os.Remove(p); err != nil {
					return err
				}
			}
			fmt.Fprintf(os.Stderr, "removed %d shard(s), %d review(s)\n", len(shards), len(reviews))
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "skip confirmation prompt")
	return cmd
}

func resetTargets(worklogDir string) (shards, reviews []string, err error) {
	entries, err := os.ReadDir(worklogDir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.IsDir() && event.IsMonthShard(e.Name()) {
			shards = append(shards, filepath.Join(worklogDir, e.Name()))
		}
	}
	reviewsDir := filepath.Join(worklogDir, "reviews")
	rentries, rerr := os.ReadDir(reviewsDir)
	if rerr == nil {
		for _, e := range rentries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				reviews = append(reviews, filepath.Join(reviewsDir, e.Name()))
			}
		}
	}
	return shards, reviews, nil
}

func parseDate(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", s, time.Local)
}

func openEditor(root string) (string, error) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	f, err := os.CreateTemp("", "worklog-note-*.md")
	if err != nil {
		return "", err
	}
	tmpName := f.Name()
	f.Close()
	defer os.Remove(tmpName)

	cmd := exec.Command("sh", "-c", fmt.Sprintf("%s %s", editor, shellQuote(tmpName)))
	cmd.Dir = root
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	b, err := os.ReadFile(tmpName)
	return string(b), err
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
