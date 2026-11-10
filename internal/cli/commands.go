package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/unmadethings/unthrecover/internal/backup"
	"github.com/unmadethings/unthrecover/internal/buildinfo"
	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/discovery"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/postgres"
	"github.com/unmadethings/unthrecover/internal/recovery"
	"github.com/unmadethings/unthrecover/internal/repo"
	"github.com/unmadethings/unthrecover/internal/report"
	"github.com/unmadethings/unthrecover/internal/verify"
)

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func checkFormat(f string) error {
	if f != "text" && f != "json" {
		return &ExitError{Code: ExitUsage, Err: fmt.Errorf("invalid --format %q (text|json)", f)}
	}
	return nil
}

// ---------------------------------------------------------------- version

func newVersionCmd(app *App) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			info := map[string]string{"version": buildinfo.Version, "commit": buildinfo.Commit,
				"date": buildinfo.Date, "go": buildinfo.GoVersion(), "platform": runtime.GOOS + "/" + runtime.GOARCH}
			if format == "json" {
				return writeJSON(app.Out, info)
			}
			fmt.Fprintf(app.Out, "unth %s (commit %s, built %s, %s, %s)\n", info["version"], info["commit"], info["date"], info["go"], info["platform"])
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	return cmd
}

// ---------------------------------------------------------------- doctor

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func newDoctorCmd(app *App) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that the environment can run backups and verifications",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			ctx := cmd.Context()
			var checks []check
			add := func(name, status, detail string) { checks = append(checks, check{name, status, detail}) }

			add("platform", "PASS", runtime.GOOS+"/"+runtime.GOARCH)
			if runtime.GOOS != "linux" {
				checks[len(checks)-1].Status = "WARN"
				checks[len(checks)-1].Detail += " (only Linux is supported in this release: isolated restore requires host access to Docker bridge networks)"
			}
			for _, t := range []string{"pg_dump", "pg_restore"} {
				ti, err := postgres.LookupTool(ctx, t)
				if err != nil {
					if ms := postgres.InstalledMajors(ctx, t); len(ms) > 0 {
						ti, err = postgres.FindTool(ctx, t, ms[len(ms)-1])
					}
				}
				switch {
				case err != nil:
					add(t, "FAIL", app.Redactor.RedactError(err))
				case t == "pg_dump":
					extra := ""
					if ms := postgres.InstalledMajors(ctx, t); len(ms) > 0 {
						extra = fmt.Sprintf("; version-matched clients installed for majors %v", ms)
					}
					add(t, "PASS", fmt.Sprintf("%s (can back up PostgreSQL %d–%d)%s", ti.Version, postgres.MinServerMajor, ti.Major, extra))
				default:
					add(t, "PASS", fmt.Sprintf("%s (reads archives from pg_dump <= %d)", ti.Version, ti.Major))
				}
			}
			dc, err := dockerClient()
			if err == nil {
				info, nerr := dc.Negotiate(ctx)
				if nerr != nil {
					add("docker", "FAIL", nerr.Error())
				} else {
					add("docker", "PASS", fmt.Sprintf("Engine %s, API %s (%s) via %s", info.Version, info.APIVersion, info.Os, dc.Host()))
				}
			} else {
				add("docker", "FAIL", err.Error())
			}
			store, r, err := app.openStore(ctx)
			if err != nil {
				add("data_dir", "FAIL", err.Error())
			} else {
				defer store.Close()
				ver, _ := store.SchemaVersion(ctx)
				add("data_dir", "PASS", fmt.Sprintf("%s (metadata schema v%d)", r.Root, ver))
				if free, err := repo.FreeBytes(r.Root); err == nil {
					st := "PASS"
					if free < 1<<30 {
						st = "WARN"
					}
					add("free_space", st, fmt.Sprintf("%.1f GiB available", float64(free)/(1<<30)))
				}
				runs, _ := store.UnfinishedRuns(ctx)
				bks, _ := store.InterruptedBackups(ctx)
				if n := len(runs) + len(bks); n > 0 {
					add("unfinished_operations", "WARN", fmt.Sprintf("%d unfinished operations; run `unth cleanup --dry-run` if no unth process is running", n))
				} else {
					add("unfinished_operations", "PASS", "none")
				}
			}
			failed := false
			for _, c := range checks {
				if c.Status == "FAIL" {
					failed = true
				}
			}
			if format == "json" {
				if err := writeJSON(app.Out, map[string]any{"ok": !failed, "checks": checks}); err != nil {
					return err
				}
			} else {
				tw := tabwriter.NewWriter(app.Out, 2, 4, 2, ' ', 0)
				for _, c := range checks {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Status, c.Name, c.Detail)
				}
				_ = tw.Flush()
			}
			if failed {
				return &ExitError{Code: ExitFailure}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	return cmd
}

// ---------------------------------------------------------------- discover

func newDiscoverCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "discover", Short: "Read-only discovery of backup sources"}
	var format, cfgPath string
	dockerCmd := &cobra.Command{
		Use:   "docker",
		Short: "List Docker containers and volumes and assess backup support (read-only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			dc, err := dockerClient()
			if err != nil {
				return err
			}
			inv, err := discovery.Docker(cmd.Context(), dc)
			if err != nil {
				return failure.New(failure.DockerUnavailable, "Docker discovery failed", err)
			}
			if format == "json" {
				return writeJSON(app.Out, inv)
			}
			fmt.Fprintf(app.Out, "Docker Engine %s (API %s)\n\nCONTAINERS\n", inv.DockerVersion, inv.APIVersion)
			tw := tabwriter.NewWriter(app.Out, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tIMAGE\tSTATE\tKIND\tASSESSMENT")
			for _, c := range inv.Containers {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.Name, c.Image, c.State, c.Kind, c.Assessment)
			}
			_ = tw.Flush()
			fmt.Fprintln(app.Out, "\nVOLUMES")
			tw = tabwriter.NewWriter(app.Out, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tDRIVER\tUSED BY\tASSESSMENT")
			for _, v := range inv.Volumes {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", trunc(v.Name, 40), v.Driver, strings.Join(v.UsedBy, ","), v.Assessment)
			}
			return tw.Flush()
		},
	}
	dockerCmd.Flags().StringVar(&format, "format", "text", "output format: text|json")

	pgCmd := &cobra.Command{
		Use:   "postgres",
		Short: "Test a PostgreSQL connection and evaluate backup compatibility (read-only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return err
			}
			pw, err := cfg.Source.ResolvePassword()
			if err != nil {
				return &ExitError{Code: ExitUsage, Err: err}
			}
			app.Redactor.Add(pw)
			rep, err := discovery.Postgres(cmd.Context(), cfg.Source, pw)
			if err != nil {
				rep.Error = app.Redactor.RedactError(err)
			}
			if format == "json" {
				if werr := writeJSON(app.Out, rep); werr != nil {
					return werr
				}
			} else {
				fmt.Fprintf(app.Out, "Reachable:      %v\n", rep.Reachable)
				if rep.Reachable {
					fmt.Fprintf(app.Out, "Server version: %s\nDatabase size:  %d bytes\nUser tables:    %d\nIn recovery:    %v\npg_dump:        %s\n",
						rep.ServerVersion, rep.DatabaseSize, rep.UserTables, rep.InRecovery, rep.PgDumpVersion)
					fmt.Fprintf(app.Out, "Compatibility:  %s\n", rep.Compatibility)
				}
			}
			if err != nil {
				return err
			}
			if !rep.Compatible {
				return &ExitError{Code: ExitFailure, Err: failure.New(failure.UnsupportedVersion, rep.Compatibility, nil)}
			}
			return nil
		},
	}
	pgCmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	pgCmd.Flags().StringVarP(&cfgPath, "config", "c", "", "job configuration file (required)")
	_ = pgCmd.MarkFlagRequired("config")

	var writeDir string
	jobsCmd := &cobra.Command{
		Use:   "jobs",
		Short: "Suggest job configurations for PostgreSQL containers (Coolify, Dokploy, Compose, Docker) (read-only)",
		Long: `Jobs finds PostgreSQL containers, recognizes Coolify, Dokploy and Docker Compose from their
labels and networks, and prints a ready-to-review job configuration for each: host name on the
shared Docker network, database, user, the same image for the restore test (same extensions),
and which network unth must join. Database passwords are never read: each job uses password_env
and says where to find the password. --write-dir writes <workload>.yaml files (never overwrites).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			dc, err := dockerClient()
			if err != nil {
				return err
			}
			js, err := discovery.SuggestJobs(cmd.Context(), dc)
			if err != nil {
				return failure.New(failure.DockerUnavailable, "Docker discovery failed", err)
			}
			if format == "json" {
				return writeJSON(app.Out, js)
			}
			if len(js) == 0 {
				fmt.Fprintln(app.Out, "No PostgreSQL containers found.")
				return nil
			}
			for _, j := range js {
				fmt.Fprintf(app.Out, "── %s  (%s, container %s)\n%s\n", j.Workload, j.Platform, j.Container, j.YAML)
			}
			if writeDir != "" {
				written, skipped, err := discovery.WriteJobs(writeDir, js)
				for _, p := range written {
					fmt.Fprintf(app.Out, "wrote   %s\n", p)
				}
				for _, p := range skipped {
					fmt.Fprintf(app.Out, "skipped %s (exists)\n", p)
				}
				return err
			}
			return nil
		},
	}
	jobsCmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	jobsCmd.Flags().StringVar(&writeDir, "write-dir", "", "write <workload>.yaml files into this directory (never overwrites)")
	cmd.AddCommand(dockerCmd, pgCmd, jobsCmd)
	return cmd
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n-3] + "..."
	}
	return s
}

func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, &ExitError{Code: ExitUsage, Err: failure.New(failure.ConfigInvalid, "configuration", err)}
	}
	return cfg, nil
}

// ---------------------------------------------------------------- backup

func newBackupCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "backup", Short: "Create and inspect backups"}
	var cfgPath, format string
	pg := &cobra.Command{
		Use:   "postgres",
		Short: "Create a snapshot-consistent logical PostgreSQL backup",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return err
			}
			if cfg.Source.Type != config.TypePostgres {
				return &ExitError{Code: ExitUsage, Err: fmt.Errorf("source.type is %q; use `unth backup volume` for docker-volume sources", cfg.Source.Type)}
			}
			store, r, err := app.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			eng := &backup.Engine{Store: store, Repo: r, Redactor: app.Redactor, Log: app.Log, Docker: dockerClient}
			if format == "text" {
				fmt.Fprintf(app.Out, "Backing up %s (%s@%s:%d/%s)...\n", cfg.Source.Name, cfg.Source.User, cfg.Source.Host, cfg.Source.Port, cfg.Source.Database)
			}
			b, err := eng.RunPostgres(cmd.Context(), cfg)
			if b != nil && format == "json" {
				_ = writeJSON(app.Out, backupView(b))
			}
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return &ExitError{Code: ExitCancelled, Err: err}
				}
				return err
			}
			if format == "text" {
				fmt.Fprintf(app.Out, "Backup created: %s\n  size:    %d bytes\n  sha256:  %s\n  server:  PostgreSQL %s\n  status:  %s (not yet verified)\n\nNext: unth verify %s\n",
					b.ID, b.SizeBytes, b.SHA256, b.ServerVersion, b.Status, b.ID)
			}
			return nil
		},
	}
	pg.Flags().StringVarP(&cfgPath, "config", "c", "", "job configuration file (required)")
	pg.Flags().StringVar(&format, "format", "text", "output format: text|json")
	_ = pg.MarkFlagRequired("config")

	var listFormat string
	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List backups (newest first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(listFormat); err != nil {
				return err
			}
			store, _, err := app.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			bs, err := store.ListBackups(cmd.Context(), limit)
			if err != nil {
				return err
			}
			if listFormat == "json" {
				views := make([]map[string]any, 0, len(bs))
				for _, b := range bs {
					views = append(views, backupView(b))
				}
				return writeJSON(app.Out, views)
			}
			tw := tabwriter.NewWriter(app.Out, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "BACKUP ID\tWORKLOAD\tCREATED\tSTATUS\tVERIFICATION\tSIZE")
			for _, b := range bs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", b.ID, b.WorkloadID, b.CreatedAt.Format(time.RFC3339), b.Status, b.VerificationStatus, b.SizeBytes)
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&listFormat, "format", "text", "output format: text|json")
	list.Flags().IntVar(&limit, "limit", 100, "maximum number of backups")

	var showFormat string
	show := &cobra.Command{
		Use:   "show <backup-id>",
		Short: "Show a backup and its verification history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkFormat(showFormat); err != nil {
				return err
			}
			store, _, err := app.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			b, err := store.GetBackup(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("backup %s: %w", args[0], err)
			}
			runs, err := store.ListRuns(cmd.Context(), b.ID)
			if err != nil {
				return err
			}
			v := backupView(b)
			rv := []map[string]any{}
			for _, r := range runs {
				rv = append(rv, map[string]any{"run_id": r.ID, "status": r.Status, "started_at": r.StartedAt,
					"restore_duration_ms": r.RestoreDurationMS, "error_category": r.ErrorCategory})
			}
			v["verification_runs"] = rv
			if showFormat == "json" {
				return writeJSON(app.Out, v)
			}
			keys := []string{"id", "workload_id", "type", "status", "verification_status", "created_at", "snapshot_at",
				"size_bytes", "sha256", "server_version", "artifact_path", "error_category", "error_message"}
			for _, k := range keys {
				if val, ok := v[k]; ok && fmt.Sprint(val) != "" {
					fmt.Fprintf(app.Out, "%-20s %v\n", k+":", val)
				}
			}
			fmt.Fprintf(app.Out, "verification runs:\n")
			for _, r := range runs {
				fmt.Fprintf(app.Out, "  %s  %-10s %s  restore=%dms %s\n", r.ID, r.Status, r.StartedAt.Format(time.RFC3339), r.RestoreDurationMS, r.ErrorCategory)
			}
			return nil
		},
	}
	show.Flags().StringVar(&showFormat, "format", "text", "output format: text|json")
	var vCfg, vFormat string
	vol := &cobra.Command{
		Use:   "volume",
		Short: "Create a file-level backup of a Docker volume (consistency: offline or stop)",
		Long: `Backs up a Docker volume through the Docker archive API using a never-started helper
container. consistency: offline (default) refuses if a running container mounts the volume;
consistency: stop stops exactly those containers for the duration of the copy and starts them
again afterwards (also on failure). A per-file manifest (type, mode, owner, size, SHA-256) is
recorded and used by ` + "`unth verify`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(vFormat); err != nil {
				return err
			}
			cfg, err := loadConfig(vCfg)
			if err != nil {
				return err
			}
			if cfg.Source.Type != config.TypeDockerVolume {
				return &ExitError{Code: ExitUsage, Err: fmt.Errorf("source.type is %q; use `unth backup postgres` for PostgreSQL sources", cfg.Source.Type)}
			}
			store, r, err := app.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			eng := &backup.Engine{Store: store, Repo: r, Redactor: app.Redactor, Log: app.Log, Docker: dockerClient}
			if vFormat == "text" {
				fmt.Fprintf(app.Out, "Backing up volume %s (workload %s, consistency %s)...\n", cfg.Source.Volume, cfg.Source.Name, cfg.Source.Consistency)
			}
			b, err := eng.RunVolume(cmd.Context(), cfg)
			if b != nil && vFormat == "json" {
				_ = writeJSON(app.Out, backupView(b))
			}
			if b != nil && vFormat == "text" && b.Status == metadata.BackupCreated {
				fmt.Fprintf(app.Out, "Backup created: %s\n  size:    %d bytes\n  sha256:  %s\n  status:  %s (not yet verified)\n", b.ID, b.SizeBytes, b.SHA256, b.Status)
				if b.ErrorMessage != "" {
					fmt.Fprintf(app.Out, "  %s\n", b.ErrorMessage)
				}
				fmt.Fprintf(app.Out, "\nNext: unth verify %s\n", b.ID)
			}
			if err != nil && errors.Is(err, context.Canceled) {
				return &ExitError{Code: ExitCancelled, Err: err}
			}
			return err
		},
	}
	vol.Flags().StringVarP(&vCfg, "config", "c", "", "job configuration file (required)")
	vol.Flags().StringVar(&vFormat, "format", "text", "output format: text|json")
	_ = vol.MarkFlagRequired("config")
	cmd.AddCommand(pg, vol, list, show)
	return cmd
}

func backupView(b *metadata.Backup) map[string]any {
	v := map[string]any{
		"id": b.ID, "workload_id": b.WorkloadID, "type": b.BackupType, "status": b.Status,
		"verification_status": b.VerificationStatus, "created_at": b.CreatedAt, "size_bytes": b.SizeBytes,
		"sha256": b.SHA256, "server_version": b.ServerVersion, "artifact_path": b.ArtifactPath,
		"error_category": b.ErrorCategory, "error_message": b.ErrorMessage, "last_verification_id": b.LastVerificationID,
	}
	if b.SnapshotAt != nil {
		v["snapshot_at"] = *b.SnapshotAt
	}
	return v
}

// ---------------------------------------------------------------- verify

func newVerifyCmd(app *App) *cobra.Command {
	var format, image, pull string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "verify <backup-id>",
		Short: "Restore a backup into a fresh isolated container and verify it",
		Long: `Verify performs a REAL recovery test on every invocation:
  1. checks the artifact, manifest and SHA-256 checksum,
  2. creates a new isolated PostgreSQL container (internal network, no published ports, resource limits),
  3. restores the backup with pg_restore,
  4. compares row counts, table contents, schema objects, constraints and assertion queries
     with the fingerprint captured from the source inside the backup snapshot,
  5. stores a report and removes every resource it created.

A backup is VERIFIED only if all mandatory steps pass. Exit code 3 means not verified.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			store, r, err := app.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			eng := &verify.Engine{Store: store, Repo: r, Docker: dockerClient, Redactor: app.Redactor, Log: app.Log}
			if format == "text" {
				fmt.Fprintf(app.Out, "Verifying %s...\n", args[0])
				eng.Progress = func(s report.Step) {
					fmt.Fprintf(app.Out, "  [%s] %s (%dms)\n", s.Status, s.Name, s.DurationMS)
				}
			}
			rep, err := eng.Run(cmd.Context(), args[0], verify.Options{Timeout: timeout, RestoreImage: image, ImagePull: pull})
			if err != nil {
				if errors.Is(err, verify.ErrNotVerifiable) || errors.Is(err, metadata.ErrNotFound) {
					return &ExitError{Code: ExitUsage, Err: err}
				}
				if rep == nil {
					return err
				}
			}
			if format == "json" {
				if werr := rep.JSON(app.Out); werr != nil {
					return werr
				}
			} else {
				fmt.Fprintln(app.Out)
				if werr := rep.Text(app.Out); werr != nil {
					return werr
				}
			}
			if err != nil {
				return err
			}
			switch rep.FinalStatus {
			case metadata.VerifyVerified:
				return nil
			case metadata.VerifyCanceled:
				return &ExitError{Code: ExitCancelled}
			default:
				return &ExitError{Code: ExitNotVerified}
			}
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "override verification timeout (e.g. 10m)")
	cmd.Flags().StringVar(&image, "image", "", "override restore image (default postgres:<source major>)")
	cmd.Flags().StringVar(&pull, "pull", "", "override image pull policy: if-missing|always|never")
	return cmd
}

// ---------------------------------------------------------------- report

func newReportCmd(app *App) *cobra.Command {
	var format, runID string
	cmd := &cobra.Command{
		Use:   "report <backup-id>",
		Short: "Show the latest (or a specific) verification report of a backup",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			store, _, err := app.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			b, err := store.GetBackup(cmd.Context(), args[0])
			if err != nil {
				return &ExitError{Code: ExitUsage, Err: fmt.Errorf("backup %s: %w", args[0], err)}
			}
			id := runID
			if id == "" {
				id = b.LastVerificationID
			}
			if id == "" {
				return &ExitError{Code: ExitFailure, Err: fmt.Errorf("backup %s has never been verified; run `unth verify %s`", b.ID, b.ID)}
			}
			run, err := store.GetRun(cmd.Context(), id)
			if err != nil || run.BackupID != b.ID {
				return &ExitError{Code: ExitUsage, Err: fmt.Errorf("verification run %s not found for backup %s", id, b.ID)}
			}
			if run.ReportJSON == "" {
				return &ExitError{Code: ExitFailure, Err: fmt.Errorf("run %s has no report (status %s); it may still be running or was interrupted", run.ID, run.Status)}
			}
			if format == "json" {
				_, err := fmt.Fprintln(app.Out, run.ReportJSON)
				return err
			}
			var rep report.Report
			if err := json.Unmarshal([]byte(run.ReportJSON), &rep); err != nil {
				return err
			}
			return rep.Text(app.Out)
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	cmd.Flags().StringVar(&runID, "run", "", "verification run ID (default: latest)")
	return cmd
}

// ---------------------------------------------------------------- cleanup

func newCleanupCmd(app *App) *cobra.Command {
	var dryRun bool
	var format string
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Recover from interrupted operations and remove orphaned verification resources",
		Long: `Marks backups and verification runs whose process is no longer running as
FAILED (INTERRUPTED), removes their partial files, and removes Docker
containers, volumes and networks labeled io.unthrecover.managed=true that
belong to runs which are not active. Nothing without these labels is touched.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			store, r, err := app.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			actions, err := recovery.Cleanup(cmd.Context(), store, r, dockerClient, recovery.Options{DryRun: dryRun})
			_ = store.Audit(context.WithoutCancel(cmd.Context()), metadata.AuditEvent{Actor: repo.Actor(), Action: "cleanup",
				Target: "*", Outcome: map[bool]string{true: "dry-run", false: "done"}[dryRun], Details: fmt.Sprintf("%d actions", len(actions))})
			if format == "json" {
				if actions == nil {
					actions = []recovery.Action{}
				}
				_ = writeJSON(app.Out, map[string]any{"dry_run": dryRun, "actions": actions})
			} else {
				if len(actions) == 0 {
					fmt.Fprintln(app.Out, "Nothing to clean up.")
				}
				for _, a := range actions {
					state := "planned"
					if a.Done {
						state = "done"
					}
					if a.Error != "" {
						state = "error: " + a.Error
					}
					fmt.Fprintf(app.Out, "[%s] %s %s — %s\n", state, a.Kind, a.Target, a.Detail)
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "only show what would be done")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	return cmd
}

// ---------------------------------------------------------------- audit

func newAuditCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "audit", Short: "Inspect the audit trail"}
	var format string
	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List audit events (newest first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			store, _, err := app.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			evs, err := store.ListAudit(cmd.Context(), limit)
			if err != nil {
				return err
			}
			if format == "json" {
				return writeJSON(app.Out, evs)
			}
			tw := tabwriter.NewWriter(app.Out, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "TIME\tACTOR\tACTION\tTARGET\tOUTCOME\tDETAILS")
			for _, e := range evs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.At.Format(time.RFC3339), e.Actor, e.Action, e.Target, e.Outcome, e.Details)
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&format, "format", "text", "output format: text|json")
	list.Flags().IntVar(&limit, "limit", 100, "maximum number of events")
	cmd.AddCommand(list)
	return cmd
}
