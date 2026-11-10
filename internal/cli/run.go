package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unmadethings/unthrecover/internal/repo"
	"github.com/unmadethings/unthrecover/internal/report"
	"github.com/unmadethings/unthrecover/internal/retention"
	"github.com/unmadethings/unthrecover/internal/runner"
)

// ExitNotifyFailed: the job itself succeeded but a notification failed.
const ExitNotifyFailed = 4

// ExitOffsiteFailed: the backup is VERIFIED locally but the offsite copy
// failed or could not be proven; nothing was pruned.
const ExitOffsiteFailed = 5

func newRunCmd(app *App) *cobra.Command {
	var cfgPath, format string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a complete job: backup, verify, prune (retention) and notify",
		Long: `Run is the command to schedule (systemd timer, cron). It:
  1. takes a per-workload lock (overlapping runs are refused),
  2. creates a backup,
  3. verifies it with a real isolated restore,
  4. applies retention (only if verification succeeded; the newest VERIFIED backup is always kept),
  5. sends notifications (always attempted, even after a failure).

With offsite: configured, step 4 first copies the verified backup offsite (encrypted), downloads
it again and checks it; only then is anything pruned (locally and offsite).

Exit codes: 0 verified · 1 backup or prune failed · 3 not verified · 4 job succeeded but a
notification failed · 5 verified locally but the offsite copy failed · 130 cancelled · 2
configuration error.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return err
			}
			store, r, err := app.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			rn := &runner.Runner{Store: store, Repo: r, Docker: dockerClient, Redactor: app.Redactor, Log: app.Log}
			if format == "text" {
				fmt.Fprintf(app.Out, "Running job %q...\n", cfg.Source.Name)
				rn.Progress = func(s report.Step) { fmt.Fprintf(app.Out, "  [%s] %s (%dms)\n", s.Status, s.Name, s.DurationMS) }
			}
			sum := rn.Run(cmd.Context(), cfg)
			if format == "json" {
				if err := writeJSON(app.Out, sum); err != nil {
					return err
				}
			} else {
				printSummary(app, sum)
			}
			switch {
			case sum.Cancelled:
				return &ExitError{Code: ExitCancelled}
			case sum.Outcome == runner.OutcomeFailure && sum.Stage == "verify":
				return &ExitError{Code: ExitNotVerified}
			case sum.Outcome == runner.OutcomeFailure && sum.Stage == "offsite":
				return &ExitError{Code: ExitOffsiteFailed}
			case sum.Outcome == runner.OutcomeFailure:
				return &ExitError{Code: ExitFailure}
			case sum.NotifyFailed():
				return &ExitError{Code: ExitNotifyFailed}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "", "job configuration file (required)")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func printSummary(app *App, s *runner.Summary) {
	fmt.Fprintf(app.Out, "\nResult: %s", strings.ToUpper(s.Outcome))
	if s.Stage != "" {
		fmt.Fprintf(app.Out, " (failed at %s)", s.Stage)
	}
	fmt.Fprintln(app.Out)
	if s.BackupID != "" {
		fmt.Fprintf(app.Out, "Backup:        %s (%s)\n", s.BackupID, s.BackupStatus)
	}
	if s.RunID != "" {
		fmt.Fprintf(app.Out, "Verification:  %s (%s)\n", s.RunID, s.VerificationStatus)
	}
	if s.ErrorCategory != "" {
		fmt.Fprintf(app.Out, "Failure:       %s: %s\n", s.ErrorCategory, s.ErrorMessage)
	}
	if s.Hint != "" {
		fmt.Fprintf(app.Out, "Suggested fix: %s\n", s.Hint)
	}
	if o := s.Offsite; o != nil {
		fmt.Fprintf(app.Out, "Offsite:       %s %s (encrypted=%v signed=%v)", o.Status, strings.TrimSuffix(o.Target+"/"+o.Location, "/"), o.Encrypted, o.Signed)
		if len(o.Pruned) > 0 {
			fmt.Fprintf(app.Out, ", pruned %d copy(ies)", len(o.Pruned))
		}
		fmt.Fprintln(app.Out)
	}
	switch {
	case s.Prune != nil:
		fmt.Fprintf(app.Out, "Retention:     expired %d backup(s) %v\n", len(s.Prune.Expired), s.Prune.Expired)
	case s.PruneSkipped != "":
		fmt.Fprintf(app.Out, "Retention:     skipped (%s)\n", s.PruneSkipped)
	}
	for _, n := range s.Notifications {
		switch {
		case n.Sent:
			fmt.Fprintf(app.Out, "Notify %-8s sent\n", n.Name+":")
		case n.Skipped != "":
			fmt.Fprintf(app.Out, "Notify %-8s skipped (%s)\n", n.Name+":", n.Skipped)
		default:
			fmt.Fprintf(app.Out, "Notify %-8s FAILED (%s)\n", n.Name+":", n.Error)
		}
	}
	fmt.Fprintf(app.Out, "Duration:      %s\n", s.FinishedAt.Sub(s.StartedAt).Round(time.Millisecond))
}

func newPruneCmd(app *App) *cobra.Command {
	var cfgPath, format string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Apply the retention policy of a job (expire old backups)",
		Long: `Prune applies the retention rules of the job configuration to the workload's backups.
Expired backups have their artifact directory deleted and their status set to EXPIRED;
metadata, verification history and reports are kept. The newest VERIFIED backup and any
backup whose verification is in progress are never expired. Use --dry-run first.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return err
			}
			if !cfg.Retention.Enabled() {
				return &ExitError{Code: ExitUsage, Err: fmt.Errorf("no retention rules in %s (set retention.keep_last / keep_daily / keep_weekly / max_age)", cfgPath)}
			}
			store, r, err := app.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			res, err := retention.Apply(cmd.Context(), store, r, cfg.Source.Name, cfg.Retention, dryRun, repo.Actor(), time.Now().UTC())
			if res != nil {
				if format == "json" {
					_ = writeJSON(app.Out, res)
				} else {
					verb := "expired"
					if dryRun {
						verb = "would expire"
					}
					for _, d := range res.Decisions {
						state := "keep  "
						if !d.Keep {
							state = "EXPIRE"
						}
						fmt.Fprintf(app.Out, "%s %s  %s  %-12s %s\n", state, d.BackupID, d.CreatedAt.Format(time.RFC3339), d.Verified, strings.Join(d.Reasons, "; "))
					}
					fmt.Fprintf(app.Out, "\n%s %d backup(s)\n", verb, len(res.Expired))
				}
			}
			return err
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "", "job configuration file (required)")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "only show what would be expired")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}
