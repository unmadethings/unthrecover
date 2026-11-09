// Package verify implements the Recovery Verification Engine.
//
// Every invocation creates a new verification run with its own ID, performs
// a REAL restore into a freshly created isolated PostgreSQL container, and
// compares the restored database against the fingerprint captured from the
// source inside the backup snapshot. Previous results are never reused.
package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unmadethings/unthrecover/internal/buildinfo"
	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/docker"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/manifest"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/postgres"
	"github.com/unmadethings/unthrecover/internal/repo"
	"github.com/unmadethings/unthrecover/internal/report"
	"github.com/unmadethings/unthrecover/internal/restore"
	"github.com/unmadethings/unthrecover/internal/security"
)

// MaxArtifactBytes bounds artifacts accepted for verification (1 TiB).
const MaxArtifactBytes int64 = 1 << 40

// Engine runs verifications.
type Engine struct {
	Store    *metadata.Store
	Repo     *repo.Repo
	Docker   func() (*docker.Client, error)
	Redactor *security.Redactor
	Log      *slog.Logger
	// Progress, if set, receives step updates for interactive output.
	Progress func(step report.Step)
}

// Options override stored verification settings.
type Options struct {
	Timeout      time.Duration
	RestoreImage string
	ImagePull    string
}

type runState struct {
	rep        *report.Report
	run        *metadata.Run
	backup     *metadata.Backup
	settings   config.Verification
	expected   *postgres.Fingerprint
	artifact   string
	manifest   *manifest.Manifest
	restoreDur time.Duration
	failErr    error
}

// ErrNotVerifiable is returned when the backup cannot enter verification.
var ErrNotVerifiable = errors.New("backup cannot be verified")

// Run verifies a backup and returns the report. The returned error is
// non-nil only for problems that prevented a report from being produced;
// a FAILED verification is reported through Report.FinalStatus.
func (e *Engine) Run(ctx context.Context, backupID string, opts Options) (*report.Report, error) {
	if err := security.ValidateID(backupID); err != nil {
		return nil, err
	}
	b, err := e.Store.GetBackup(ctx, backupID)
	if err != nil {
		return nil, fmt.Errorf("backup %s: %w", backupID, err)
	}
	if b.Status != metadata.BackupCreated {
		return nil, fmt.Errorf("%w: backup %s has status %s (only %s backups can be verified)",
			ErrNotVerifiable, b.ID, b.Status, metadata.BackupCreated)
	}

	var settings config.Verification
	if b.VerificationJSON != "" {
		if err := json.Unmarshal([]byte(b.VerificationJSON), &settings); err != nil {
			return nil, fmt.Errorf("stored verification settings: %w", err)
		}
	}
	settings.ApplyDefaults()
	if opts.RestoreImage != "" {
		settings.RestoreImage = opts.RestoreImage
	}
	if opts.ImagePull != "" {
		settings.ImagePull = opts.ImagePull
	}
	if opts.Timeout > 0 {
		settings.Timeout = config.Duration(opts.Timeout)
	}
	if err := settings.Validate(); err != nil {
		return nil, fmt.Errorf("verification settings: %w", err)
	}

	run := &metadata.Run{ID: security.NewID("vr"), BackupID: b.ID, Status: metadata.VerifyRunning,
		StartedAt: time.Now().UTC(), PID: os.Getpid()}
	if err := e.Store.InsertRun(ctx, run); err != nil {
		return nil, fmt.Errorf("record verification run: %w", err)
	}
	b.VerificationStatus = metadata.VerifyRunning
	b.LastVerificationID = run.ID
	if err := e.Store.UpdateBackup(ctx, b); err != nil {
		return nil, err
	}
	e.audit(ctx, "verify.start", run.ID, "started", "backup="+b.ID)

	var src map[string]any
	_ = json.Unmarshal([]byte(b.SourceJSON), &src)
	st := &runState{run: run, backup: b, settings: settings, rep: &report.Report{
		ReportVersion: 1, ToolVersion: buildinfo.Version, RunID: run.ID, BackupID: b.ID,
		WorkloadID: b.WorkloadID, Source: src, StartedAt: run.StartedAt,
		Backup: report.BackupInfo{CreatedAt: b.CreatedAt, SizeBytes: b.SizeBytes, SHA256: b.SHA256,
			ServerVersion: b.ServerVersion, Type: b.BackupType},
		ChecksumResult: report.StepNotRun, RestoreResult: report.StepNotRun,
		PostgresHealth: report.StepNotRun, ContainerHealth: report.StepNotRun,
		Cleanup: report.Cleanup{Status: report.StepNotRun},
	}}
	if b.SnapshotAt != nil {
		st.rep.Backup.SnapshotAt = *b.SnapshotAt
	}

	tctx, cancel := context.WithTimeout(ctx, settings.Timeout.D())
	defer cancel()
	e.execute(tctx, st)
	return e.finish(ctx, st)
}

// step runs fn as a named step and records the outcome. It returns false if
// the step failed (subsequent steps are then recorded as NOT_RUN).
func (e *Engine) step(ctx context.Context, st *runState, name string, fn func(ctx context.Context) (string, error)) bool {
	if st.failErr != nil {
		st.rep.Steps = append(st.rep.Steps, report.Step{Name: name, Mandatory: isMandatory(name), Status: report.StepNotRun})
		return false
	}
	start := time.Now()
	detail, err := fn(ctx)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	s := report.Step{Name: name, Mandatory: isMandatory(name), Status: report.StepPass,
		DurationMS: time.Since(start).Milliseconds(), Detail: e.Redactor.Redact(detail)}
	if err != nil {
		s.Status = report.StepFail
		s.Detail = e.Redactor.RedactError(err)
		st.failErr = err
	}
	st.rep.Steps = append(st.rep.Steps, s)
	if e.Progress != nil {
		e.Progress(s)
	}
	e.Log.Debug("verification step", "run_id", st.run.ID, "step", name, "status", s.Status)
	return err == nil
}

func isMandatory(name string) bool {
	for _, m := range MandatorySteps {
		if m == name {
			return true
		}
	}
	return false
}

func (e *Engine) setRunStatus(ctx context.Context, st *runState, status string) {
	st.run.Status = status
	if err := e.Store.UpdateRun(ctx, st.run); err != nil {
		e.Log.Warn("update run status", "error", err)
	}
}

func (e *Engine) execute(ctx context.Context, st *runState) {
	b := st.backup

	// 1. Artifact accessible: path validation, no symlinks, regular file, size.
	e.step(ctx, st, StepArtifact, func(ctx context.Context) (string, error) {
		if b.ArtifactPath == "" {
			return "", failure.New(failure.ArtifactMissing, "backup record has no artifact path", nil)
		}
		p, err := st.backupPath(e.Repo, b.ArtifactPath)
		if err != nil {
			return "", err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", failure.New(failure.ArtifactMissing, "backup artifact not found at "+b.ArtifactPath, err).
					WithHint("the artifact was deleted or moved; take a new backup")
			}
			return "", failure.New(failure.ArtifactMissing, "cannot access artifact", err)
		}
		if !fi.Mode().IsRegular() {
			return "", failure.New(failure.ArtifactInvalid, "artifact is not a regular file", nil)
		}
		if fi.Size() > MaxArtifactBytes {
			return "", failure.New(failure.ArtifactInvalid, "artifact exceeds maximum supported size", nil)
		}
		if fi.Size() != b.SizeBytes {
			return "", failure.New(failure.ChecksumMismatch,
				fmt.Sprintf("artifact size %d differs from recorded size %d", fi.Size(), b.SizeBytes), nil)
		}
		st.artifact = p
		return fmt.Sprintf("%s (%d bytes)", b.ArtifactPath, fi.Size()), nil
	})

	// 2. Manifest valid and consistent with the metadata database.
	e.step(ctx, st, StepManifest, func(ctx context.Context) (string, error) {
		p, err := st.backupPath(e.Repo, b.ManifestPath)
		if err != nil {
			return "", err
		}
		m, err := manifest.Read(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", failure.New(failure.ArtifactMissing, "manifest not found", err)
			}
			return "", failure.New(failure.ManifestInvalid, "manifest unreadable", err)
		}
		switch {
		case m.BackupID != b.ID:
			return "", failure.New(failure.ManifestInvalid, "manifest belongs to backup "+m.BackupID, nil)
		case m.Artifact.SHA256 != b.SHA256:
			return "", failure.New(failure.ChecksumMismatch, "manifest checksum differs from metadata record", nil)
		case m.Artifact.SizeBytes != b.SizeBytes:
			return "", failure.New(failure.ManifestInvalid, "manifest size differs from metadata record", nil)
		case m.Fingerprint == nil || m.FingerprintDigest != b.SourceFingerprint || m.Fingerprint.Digest() != b.SourceFingerprint:
			return "", failure.New(failure.ManifestInvalid, "manifest fingerprint does not match metadata record", nil)
		}
		var fp postgres.Fingerprint
		if err := json.Unmarshal([]byte(b.FingerprintJSON), &fp); err != nil || fp.Digest() != b.SourceFingerprint {
			return "", failure.New(failure.ManifestInvalid, "stored source fingerprint is corrupt", err)
		}
		st.expected = &fp
		st.manifest = m
		return fmt.Sprintf("format v%d, %d tables fingerprinted", m.FormatVersion, len(fp.Tables)), nil
	})

	// 3. Checksum recomputed from the bytes on disk.
	ok := e.step(ctx, st, StepChecksum, func(ctx context.Context) (string, error) {
		sum, n, err := manifest.HashFile(st.artifact)
		if err != nil {
			return "", failure.New(failure.ArtifactMissing, "read artifact", err)
		}
		if n != b.SizeBytes || sum != b.SHA256 {
			return "", failure.New(failure.ChecksumMismatch,
				fmt.Sprintf("SHA-256 mismatch: expected %s, got %s", b.SHA256, sum), nil).
				WithHint("the artifact is corrupted or was modified; do not rely on this backup")
		}
		return "sha256 " + sum, nil
	})
	st.rep.ChecksumResult = resultOf(ok, st)

	// 4. Archive structure readable by pg_restore.
	var restoreTool *postgres.ToolInfo
	e.step(ctx, st, StepArchive, func(ctx context.Context) (string, error) {
		t, err := postgres.LookupTool(ctx, "pg_restore")
		if err != nil {
			return "", err
		}
		restoreTool = t
		n, err := postgres.ListArchive(ctx, t, st.artifact)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", failure.New(failure.ArtifactInvalid, "archive has no table-of-contents entries", nil)
		}
		return fmt.Sprintf("%d TOC entries (%s)", n, t.Version), nil
	})
	if st.failErr == nil {
		e.setRunStatus(ctx, st, metadata.VerifyIntegrityChecked)
	}

	// 5. Isolated environment.
	var sb *restore.Sandbox
	e.step(ctx, st, StepEnvironment, func(ctx context.Context) (string, error) {
		dc, err := e.Docker()
		if err != nil {
			return "", failure.New(failure.DockerUnavailable, "Docker client", err)
		}
		e.setRunStatus(ctx, st, metadata.VerifyRestoreTesting)
		s, err := restore.Create(ctx, dc, st.run.ID, b.ID, b.ServerMajor, st.settings, e.Redactor, e.Log)
		sb = s
		if s != nil {
			st.rep.Environment = report.Environment{RestoreImage: s.Image, Network: "internal bridge (no external egress), no published ports",
				MemoryMB: st.settings.MemoryMB, CPUs: st.settings.CPUs, PidsLimit: st.settings.PidsLimit}
			if s.DockerInfo != nil {
				st.rep.Environment.DockerVersion = s.DockerInfo.Version
			}
		}
		if err != nil {
			st.rep.ContainerHealth = report.StepFail
			return "", err
		}
		st.rep.ContainerHealth = report.StepPass
		return fmt.Sprintf("container %s on %s (image %s)", s.ContainerID[:12], s.NetworkName, s.Image), nil
	})

	// 6. Real restore.
	ok = e.step(ctx, st, StepRestore, func(ctx context.Context) (string, error) {
		start := time.Now()
		err := postgres.Restore(ctx, restoreTool, sb.Params, st.artifact, e.Redactor)
		st.restoreDur = time.Since(start)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("pg_restore --single-transaction --exit-on-error completed in %s", st.restoreDur.Round(time.Millisecond)), nil
	})
	st.rep.RestoreResult = resultOf(ok, st)

	// 7. PostgreSQL health on the restored instance, then fingerprint.
	var actual *postgres.Fingerprint
	ok = e.step(ctx, st, StepHealth, func(ctx context.Context) (string, error) {
		conn, err := postgres.Connect(ctx, sb.Params)
		if err != nil {
			return "", failure.New(failure.RestoreFailed, "restored database not reachable", err)
		}
		defer conn.Close(context.WithoutCancel(ctx))
		var inRecovery bool
		if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
			return "", failure.New(failure.RestoreFailed, "health query failed", err)
		}
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return "", failure.New(failure.RestoreFailed, "begin read-only transaction", err)
		}
		defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
		var queries []postgres.UserQuery
		for _, q := range st.settings.Queries {
			queries = append(queries, postgres.UserQuery{Name: q.Name, SQL: q.SQL})
		}
		fp, err := postgres.Capture(ctx, tx, st.expected.HashMaxRows, queries)
		if err != nil {
			if failure.CategoryOf(err) == failure.Internal {
				return "", failure.New(failure.RestoreFailed, "inspect restored database", err)
			}
			return "", err
		}
		actual = fp
		st.rep.Environment.TargetVersion = fp.ServerVersion
		return fmt.Sprintf("connected, server %s, in_recovery=%v", fp.ServerVersion, inRecovery), nil
	})
	st.rep.PostgresHealth = resultOf(ok, st)

	var cmp *postgres.Comparison
	if actual != nil {
		cmp = postgres.Compare(st.expected, actual)
		st.rep.Comparison = cmp
	}
	e.step(ctx, st, StepData, func(ctx context.Context) (string, error) {
		if len(cmp.DataMismatches) > 0 {
			return "", failure.New(failure.DataMismatch, fmt.Sprintf("%d data mismatches (first: %s %s expected=%s actual=%s)",
				len(cmp.DataMismatches), cmp.DataMismatches[0].Check, cmp.DataMismatches[0].Object,
				cmp.DataMismatches[0].Expected, cmp.DataMismatches[0].Actual), nil)
		}
		return fmt.Sprintf("%d tables, %d rows match; %d tables content-hash verified", cmp.TablesChecked, cmp.RowsActual, cmp.TablesHashed), nil
	})
	e.step(ctx, st, StepSchema, func(ctx context.Context) (string, error) {
		if len(cmp.SchemaMismatches) > 0 {
			m := cmp.SchemaMismatches[0]
			return "", failure.New(failure.SchemaMismatch, fmt.Sprintf("%d schema mismatches (first: %s %s %s→%s)",
				len(cmp.SchemaMismatches), m.Check, m.Object, m.Expected, m.Actual), nil)
		}
		parts := make([]string, 0, len(cmp.ObjectsChecked))
		for _, k := range []string{"tables", "columns", "constraints", "indexes", "sequences", "functions", "triggers", "types", "schemas", "extensions"} {
			parts = append(parts, fmt.Sprintf("%s=%d", k, cmp.ObjectsChecked[k]))
		}
		return strings.Join(parts, " "), nil
	})
	e.step(ctx, st, StepQueries, func(ctx context.Context) (string, error) {
		if len(cmp.QueryMismatches) > 0 {
			return "", failure.New(failure.AssertionFailed, fmt.Sprintf("%d assertion queries differ (first: %s)",
				len(cmp.QueryMismatches), cmp.QueryMismatches[0].Object), nil)
		}
		if cmp.QueriesChecked == 0 {
			return "no application assertion queries configured (not covered)", nil
		}
		return fmt.Sprintf("%d assertion queries match", cmp.QueriesChecked), nil
	})
}

func (st *runState) backupPath(r *repo.Repo, rel string) (string, error) {
	if rel == "" {
		return "", failure.New(failure.ArtifactMissing, "empty path in backup record", nil)
	}
	p, err := r.Resolve(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", failure.New(failure.ArtifactMissing, "path not found: "+rel, err)
		}
		return "", failure.New(failure.ArtifactInvalid, "unsafe artifact path", err)
	}
	return p, nil
}

func resultOf(ok bool, st *runState) string {
	if ok {
		return report.StepPass
	}
	if n := len(st.rep.Steps); n > 0 {
		return st.rep.Steps[n-1].Status
	}
	return report.StepNotRun
}

// finish performs cleanup, computes the final status and persists the report.
func (e *Engine) finish(parent context.Context, st *runState) (*report.Report, error) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Minute)
	defer cancel()
	rep := st.rep

	// Cleanup by run label regardless of outcome (even if the sandbox was
	// only partially created).
	if dc, err := e.Docker(); err == nil {
		res, cerr := restore.Cleanup(cctx, dc, st.run.ID)
		if res != nil {
			rep.Cleanup.Removed = res.Removed
			rep.Cleanup.Errors = res.Errors
		}
		switch {
		case cerr != nil && res != nil && len(res.Removed) == 0 && hasAccessErr(cerr):
			rep.Cleanup.Status = report.StepNotRun
			rep.Cleanup.Errors = []string{"Docker not reachable; nothing could have been created"}
		case cerr != nil:
			rep.Cleanup.Status = report.StepFail
		default:
			rep.Cleanup.Status = report.StepPass
		}
	} else {
		rep.Cleanup.Status = report.StepNotRun
	}

	cancelled := errors.Is(parent.Err(), context.Canceled)
	if st.failErr != nil && errors.Is(st.failErr, context.DeadlineExceeded) && !cancelled {
		st.failErr = failure.New(failure.Timeout, fmt.Sprintf("verification exceeded timeout of %s", st.settings.Timeout.D()), st.failErr).
			WithHint("increase verification.timeout or use --timeout")
	}
	rep.FinalStatus = Decide(rep.Steps, cancelled)
	if rep.FinalStatus != metadata.VerifyVerified {
		err := st.failErr
		if err == nil && cancelled {
			err = context.Canceled
		}
		if err == nil {
			err = failure.New(failure.Internal, "not all mandatory steps passed", nil)
		}
		rep.ErrorCategory = string(failure.CategoryOf(err))
		rep.ErrorMessage = e.Redactor.RedactError(err)
		rep.Hint = failure.HintOf(err)
		rep.DetectedFailures = append(rep.DetectedFailures, rep.ErrorCategory+": "+rep.ErrorMessage)
	}
	if rep.DetectedFailures == nil {
		rep.DetectedFailures = []string{}
	}
	rep.Scope = buildScope(st)
	rep.FinishedAt = time.Now().UTC()
	rep.Objectives = report.Objectives{
		ObservedRestoreDurationMS: st.restoreDur.Milliseconds(),
		ObservedTotalDurationMS:   rep.FinishedAt.Sub(rep.StartedAt).Milliseconds(),
		RTONote: "Observed duration of an isolated test restore on this host. It is NOT a guaranteed RTO: real " +
			"recovery also includes detection, provisioning, data transfer, application cut-over and human response time.",
		BackupSnapshotAt: rep.Backup.SnapshotAt,
		RPONote: "Time between the backup snapshot and this report: changes made after the snapshot would be lost if " +
			"this backup were restored now. This is an estimate of exposure, not a verified RPO.",
	}
	if !rep.Backup.SnapshotAt.IsZero() {
		rep.Objectives.DataLossWindowSeconds = int64(rep.FinishedAt.Sub(rep.Backup.SnapshotAt).Seconds())
	}

	// Persist.
	raw, _ := json.Marshal(rep)
	redacted := e.Redactor.Redact(string(raw))
	st.run.Status = rep.FinalStatus
	fin := rep.FinishedAt
	st.run.FinishedAt = &fin
	st.run.RestoreDurationMS = st.restoreDur.Milliseconds()
	st.run.TotalDurationMS = rep.Objectives.ObservedTotalDurationMS
	st.run.ErrorCategory = rep.ErrorCategory
	st.run.ErrorMessage = rep.ErrorMessage
	st.run.ReportJSON = redacted
	if err := e.Store.UpdateRun(cctx, st.run); err != nil {
		return rep, fmt.Errorf("record verification result: %w", err)
	}
	b := st.backup
	b.VerificationStatus = rep.FinalStatus
	b.LastVerificationID = st.run.ID
	if err := e.Store.UpdateBackup(cctx, b); err != nil {
		return rep, fmt.Errorf("record backup verification status: %w", err)
	}
	if p, err := e.Repo.ReportPath(st.run.ID); err == nil {
		if err := os.WriteFile(p, []byte(redacted+"\n"), 0o600); err != nil {
			e.Log.Warn("write report file", "error", err)
		}
	}
	e.audit(cctx, "verify.finish", st.run.ID, strings.ToLower(rep.FinalStatus),
		fmt.Sprintf("backup=%s category=%s cleanup=%s", b.ID, rep.ErrorCategory, rep.Cleanup.Status))
	return rep, nil
}

func hasAccessErr(err error) bool {
	var ae *docker.AccessError
	return errors.As(err, &ae) || strings.Contains(err.Error(), "cannot connect to Docker Engine")
}

func buildScope(st *runState) report.Scope {
	s := report.Scope{}
	passed := map[string]bool{}
	for _, step := range st.rep.Steps {
		passed[step.Name] = step.Status == report.StepPass
	}
	add := func(cond bool, covered, notCovered string) {
		if cond {
			s.Covered = append(s.Covered, covered)
		} else {
			s.NotCovered = append(s.NotCovered, notCovered)
		}
	}
	add(passed[StepChecksum], "artifact SHA-256 matches metadata record and manifest", "artifact checksum (step did not pass)")
	add(passed[StepRestore], "full pg_restore into a fresh isolated PostgreSQL container (single transaction, exit on error)",
		"isolated restore (step did not pass)")
	if c := st.rep.Comparison; c != nil && passed[StepData] {
		s.Covered = append(s.Covered, fmt.Sprintf("exact row counts for all %d user tables", c.TablesChecked))
		if c.TablesHashed > 0 {
			s.Covered = append(s.Covered, fmt.Sprintf("full content hash for %d tables with <= %d rows", c.TablesHashed, st.expected.HashMaxRows))
		}
		if c.TablesHashed < c.TablesChecked {
			s.NotCovered = append(s.NotCovered, fmt.Sprintf("row-level content of %d tables larger than %d rows (row counts only)",
				c.TablesChecked-c.TablesHashed, st.expected.HashMaxRows))
		}
	} else {
		s.NotCovered = append(s.NotCovered, "data integrity (step did not pass)")
	}
	add(passed[StepSchema], "schemas, tables, columns, constraints (PK/FK/UNIQUE/CHECK/EXCLUDE), indexes, sequences (values must not regress; sequences are non-transactional), functions, triggers, types, extensions",
		"schema and constraint comparison (step did not pass)")
	if c := st.rep.Comparison; c != nil && c.QueriesChecked > 0 && passed[StepQueries] {
		s.Covered = append(s.Covered, fmt.Sprintf("%d application assertion queries", c.QueriesChecked))
	} else {
		s.NotCovered = append(s.NotCovered, "application-specific assertions (none configured or not passed)")
	}
	s.NotCovered = append(s.NotCovered,
		"roles, ownership and privileges (pg_dump of a single database does not include cluster roles; restored with --no-owner --no-privileges)",
		"cluster-level settings (postgresql.conf, pg_hba.conf) and other databases in the cluster",
		"contents of materialized views and unlogged-table semantics (definitions are compared; data is rebuilt by pg_restore)",
		"application/service health (no application containers are started in this release)",
		"point-in-time recovery (logical backups restore to the snapshot only)",
		"authenticity of the artifact (checksums are not signatures)")
	return s
}

func (e *Engine) audit(ctx context.Context, action, target, outcome, details string) {
	if err := e.Store.Audit(ctx, metadata.AuditEvent{Actor: repo.Actor(), Action: action, Target: target,
		Outcome: outcome, Details: e.Redactor.Redact(details)}); err != nil {
		e.Log.Warn("audit write failed", "error", err)
	}
}
