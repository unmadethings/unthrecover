package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unmadethings/unthrecover/internal/runner"
)

// newScheduleCmd runs every job in a directory on a fixed schedule. It is the
// scheduler for containers (Docker Compose, Coolify, Dokploy), where systemd
// timers and cron are not available. Each job runs exactly like `unth run`.
func newScheduleCmd(app *App) *cobra.Command {
	var jobsDir, at string
	var every time.Duration
	var runAtStart bool
	var rounds int
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Run all jobs in a directory on a schedule (for containers without systemd/cron)",
		Long: `Schedule runs every *.yaml job in --jobs-dir, one after another, each exactly like
"unth run" (lock, backup, verify, retention, notify). A failing job is logged and does not
stop the others or the scheduler. Stop with SIGINT/SIGTERM; a running job is cancelled and
recorded as CANCELLED.

  --at 03:00     every day at 03:00 (time zone from TZ, default UTC in the container image)
  --every 6h     every 6 hours (minimum 1m)

On a host with systemd prefer the unth@<job>.timer units (docs/SCHEDULING.md).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (at == "") == (every == 0) {
				return &ExitError{Code: ExitUsage, Err: errors.New("set exactly one of --at HH:MM or --every DURATION")}
			}
			if every != 0 && every < time.Minute {
				return &ExitError{Code: ExitUsage, Err: errors.New("--every must be at least 1m")}
			}
			var atH, atM int
			if at != "" {
				var err error
				if atH, atM, err = parseClock(at); err != nil {
					return &ExitError{Code: ExitUsage, Err: err}
				}
			}
			jobs, err := listJobs(jobsDir)
			if err != nil {
				return &ExitError{Code: ExitUsage, Err: err}
			}
			if len(jobs) == 0 {
				return &ExitError{Code: ExitUsage, Err: fmt.Errorf("no *.yaml jobs in %s", jobsDir)}
			}
			// Validate every job up front so a typo is reported at start, not at 03:00.
			for _, j := range jobs {
				if _, err := loadConfig(j); err != nil {
					return fmt.Errorf("%s: %w", filepath.Base(j), err)
				}
			}
			ctx := cmd.Context()
			next := time.Now()
			if !runAtStart {
				next = nextRun(time.Now(), every, atH, atM, at != "")
			}
			for round := 1; ; round++ {
				fmt.Fprintf(app.Out, "Next run: %s (%d job(s) in %s)\n", next.Format(time.RFC3339), len(jobs), jobsDir)
				if err := sleepUntil(ctx, next); err != nil {
					fmt.Fprintln(app.Out, "Scheduler stopped.")
					return nil
				}
				runRound(ctx, app, jobsDir)
				if ctx.Err() != nil {
					fmt.Fprintln(app.Out, "Scheduler stopped.")
					return nil
				}
				if rounds > 0 && round >= rounds {
					return nil
				}
				next = nextRun(time.Now(), every, atH, atM, at != "")
			}
		},
	}
	cmd.Flags().StringVar(&jobsDir, "jobs-dir", "/etc/unth/jobs", "directory with job configurations (*.yaml)")
	cmd.Flags().StringVar(&at, "at", "", "run daily at this time, HH:MM")
	cmd.Flags().DurationVar(&every, "every", 0, "run at this interval, e.g. 6h")
	cmd.Flags().BoolVar(&runAtStart, "run-at-start", false, "also run all jobs immediately at start")
	cmd.Flags().IntVar(&rounds, "rounds", 0, "stop after this many rounds (0 = run forever; for tests)")
	_ = cmd.Flags().MarkHidden("rounds")
	return cmd
}

// runRound runs each job once. The job list is re-read every round, so jobs
// added or removed while the scheduler runs are picked up.
func runRound(ctx context.Context, app *App, dir string) {
	jobs, err := listJobs(dir)
	if err != nil {
		app.Log.Error("cannot list jobs", "dir", dir, "error", err)
		return
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		name := filepath.Base(j)
		cfg, err := loadConfig(j)
		if err != nil {
			fmt.Fprintf(app.Out, "[%s] %s: configuration error: %v\n", time.Now().UTC().Format(time.RFC3339), name, err)
			continue
		}
		store, r, err := app.openStore(ctx)
		if err != nil {
			fmt.Fprintf(app.Out, "[%s] %s: cannot open data directory: %v\n", time.Now().UTC().Format(time.RFC3339), name, err)
			return
		}
		rn := &runner.Runner{Store: store, Repo: r, Docker: dockerClient, Redactor: app.Redactor, Log: app.Log}
		sum := rn.Run(ctx, cfg)
		_ = store.Close()
		line := fmt.Sprintf("[%s] %s: %s", sum.FinishedAt.UTC().Format(time.RFC3339), cfg.Source.Name, strings.ToUpper(sum.Outcome))
		if sum.VerificationStatus != "" {
			line += " · verification " + sum.VerificationStatus
		}
		if sum.ErrorCategory != "" {
			line += " · " + sum.ErrorCategory
		}
		if sum.NotifyFailed() {
			line += " · notification failed"
		}
		fmt.Fprintln(app.Out, line)
	}
}

func listJobs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("jobs directory: %w", err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") {
			continue
		}
		if strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml") {
			out = append(out, filepath.Join(dir, n))
		}
	}
	sort.Strings(out)
	return out, nil
}

func parseClock(s string) (int, int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, fmt.Errorf("--at must be HH:MM (24h), got %q", s)
	}
	return t.Hour(), t.Minute(), nil
}

// nextRun returns the next time after now: now+every, or the next HH:MM in
// now's location (DST-safe because the date is rebuilt each time).
func nextRun(now time.Time, every time.Duration, h, m int, daily bool) time.Time {
	if !daily {
		return now.Add(every)
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !t.After(now) {
		t = time.Date(now.Year(), now.Month(), now.Day()+1, h, m, 0, 0, now.Location())
	}
	return t
}

func sleepUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
