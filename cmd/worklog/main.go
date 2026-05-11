package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mikethicke/worklog/internal/capture"
	"github.com/mikethicke/worklog/internal/config"
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
		newSyncCmd(),
		newResummarizeCmd(),
		newShowCmd(),
		newReviewCmd(),
		newLsCmd(),
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
	sum := summarize.New(cfg.Summarizer.ResolveAPIKey(), cfg.Summarizer.Model)
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
			author := os.Getenv("USER")
			if name, err := exec.Command("git", "config", "user.name").Output(); err == nil {
				if s := strings.TrimSpace(string(name)); s != "" {
					author = s
				}
			}
			path, err := capture.Note(root, text, author)
			if err != nil {
				return err
			}
			fmt.Println(path)
			return nil
		},
	}
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
		month string
		year  string
		write bool
	)
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Generate (and optionally persist) a monthly or yearly review",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, root, cfg, sum, err := loadCtx()
			if err != nil {
				return err
			}
			switch {
			case month != "":
				t, err := time.Parse("2006-01", month)
				if err != nil {
					return fmt.Errorf("--month must be YYYY-MM: %w", err)
				}
				return render.ReviewMonthly(ctx, os.Stdout, root, cfg, sum, t, write)
			case year != "":
				t, err := time.Parse("2006", year)
				if err != nil {
					return fmt.Errorf("--year must be YYYY: %w", err)
				}
				return render.ReviewYearly(ctx, os.Stdout, root, cfg, sum, t, write)
			default:
				return errors.New("specify --month YYYY-MM or --year YYYY")
			}
		},
	}
	cmd.Flags().StringVar(&month, "month", "", "monthly review (YYYY-MM)")
	cmd.Flags().StringVar(&year, "year", "", "yearly review (YYYY)")
	cmd.Flags().BoolVar(&write, "write", false, "persist to .worklog/reviews/")
	return cmd
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
