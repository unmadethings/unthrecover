// Package backup implements the Backup Engine. M0 supports logical
// PostgreSQL backups (pg_dump custom format) to a local repository.
//
// Consistency: the data fingerprint used later for verification and the dump
// itself are taken from the SAME exported snapshot (REPEATABLE READ
// transaction + pg_export_snapshot + pg_dump --snapshot), so verification
// compares the restored database with exactly the state that was dumped.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unmadethings/unthrecover/internal/buildinfo"
	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/manifest"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/postgres"
	"github.com/unmadethings/unthrecover/internal/repo"
	"github.com/unmadethings/unthrecover/internal/security"
)

// TypePostgresLogical is the backup type identifier.
const TypePostgresLogical = "postgres-logical"

// Engine runs backups.
type Engine struct {
	Store    *metadata.Store
	Repo     *repo.Repo
	Redactor *security.Redactor
	Log      *slog.Logger
}

// Result summarizes a finished backup.
type Result struct {
	Backup *metadata.Backup
}

// countingHashWriter writes to a file while hashing and counting bytes.
type countingHashWriter struct {
	w    io.Writer
	h    io.Writer
	n    int64
	werr error
}

func (c *countingHashWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	_, _ = c.h.Write(p[:n])
	if err != nil {
		c.werr = err
	}
	return n, err
}

// RunPostgres executes a PostgreSQL logical backup described by cfg.
func (e *Engine) RunPostgres(ctx context.Context, cfg *config.Config) (*metadata.Backup, error) {
	src := cfg.Source
	srcJSON, _ := json.Marshal(src.Redacted())
	b := &metadata.Backup{
		ID:         security.NewID("bk"),
		WorkloadID: src.Name,
		BackupType: TypePostgresLogical,
		Status:     metadata.BackupPending,
		CreatedAt:  time.Now().UTC(),
		SourceJSON: string(srcJSON),
		PID:        os.Getpid(),
	}
	if err := e.Store.InsertBackup(ctx, b); err != nil {
		return nil, fmt.Errorf("record backup: %w", err)
	}
	e.audit(ctx, "backup.start", b.ID, "started", fmt.Sprintf("workload=%s host=%s db=%s", src.Name, src.Host, src.Database))

	tmpPath, _ := e.Repo.TmpFile(b.ID)
	var finalDir string
	err := e.run(ctx, cfg, b, tmpPath, &finalDir)

	// Cleanup and final state are recorded even if ctx was cancelled.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err != nil {
		_ = os.Remove(tmpPath)
		if finalDir != "" {
			_ = os.RemoveAll(finalDir)
		}
		cat := failure.CategoryOf(err)
		b.Status = metadata.BackupFailed
		if cat == failure.Cancelled {
			b.Status = metadata.BackupCanceled
		}
		b.ErrorCategory = string(cat)
		b.ErrorMessage = e.Redactor.RedactError(err)
		b.ArtifactPath, b.ManifestPath = "", ""
		now := time.Now().UTC()
		b.CompletedAt = &now
		b.PID = 0
		if uerr := e.Store.UpdateBackup(cctx, b); uerr != nil {
			e.Log.Error("record backup failure", "backup_id", b.ID, "error", uerr)
		}
		e.audit(cctx, "backup.finish", b.ID, "failed", fmt.Sprintf("category=%s", cat))
		return b, err
	}
	e.audit(cctx, "backup.finish", b.ID, "success", fmt.Sprintf("size=%d sha256=%s", b.SizeBytes, b.SHA256))
	return b, nil
}

func (e *Engine) run(ctx context.Context, cfg *config.Config, b *metadata.Backup, tmpPath string, finalDir *string) error {
	src := cfg.Source
	password, err := src.ResolvePassword()
	if err != nil {
		return failure.New(failure.ConfigInvalid, "resolve password", err)
	}
	e.Redactor.Add(password)

	tool, err := postgres.LookupTool(ctx, "pg_dump")
	if err != nil {
		return err
	}
	restoreTool, err := postgres.LookupTool(ctx, "pg_restore")
	if err != nil {
		return err
	}

	free, err := repo.FreeBytes(e.Repo.Root)
	if err != nil {
		return failure.New(failure.InsufficientStorage, "cannot determine free space", err)
	}
	if free < cfg.Backup.MinFreeBytes {
		return failure.New(failure.InsufficientStorage,
			fmt.Sprintf("only %d bytes free in %s; backup.min_free_bytes requires %d", free, e.Repo.Root, cfg.Backup.MinFreeBytes), nil).
			WithHint("free disk space or point --data-dir at a larger volume")
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Backup.Timeout.D())
	defer cancel()

	b.Status = metadata.BackupRunning
	if err := e.Store.UpdateBackup(ctx, b); err != nil {
		return err
	}

	params := postgres.ConnParams{
		Host: src.Host, Port: src.Port, Database: src.Database, User: src.User,
		Password: password, SSLMode: src.SSLMode, ConnectTimeout: int(src.ConnectTimeout.D().Seconds()),
	}
	conn, err := postgres.Connect(ctx, params)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return postgres.ClassifyConnError(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	ver, major, err := postgres.ServerVersion(ctx, tx)
	if err != nil {
		return failure.New(failure.ConnectionFailed, "query server version", err)
	}
	b.ServerVersion, b.ServerMajor, b.ToolVersion = ver, major, tool.Version
	if err := postgres.CheckDumpCompatibility(tool, major); err != nil {
		return err
	}

	var snapshot string
	if err := tx.QueryRow(ctx, `SELECT pg_export_snapshot()`).Scan(&snapshot); err != nil {
		return failure.New(failure.BackupFailed, "export snapshot", err)
	}
	snapAt := time.Now().UTC()
	b.SnapshotAt = &snapAt
	e.Log.Info("snapshot exported", "backup_id", b.ID, "server_version", ver)

	queries := make([]postgres.UserQuery, 0, len(cfg.Verification.Queries))
	for _, q := range cfg.Verification.Queries {
		queries = append(queries, postgres.UserQuery{Name: q.Name, SQL: q.SQL})
	}
	fp, err := postgres.Capture(ctx, tx, cfg.Verification.TableHashMaxRows, queries)
	if err != nil {
		if failure.CategoryOf(err) == failure.Internal {
			return failure.New(failure.BackupFailed, "capture source fingerprint", err)
		}
		return err
	}
	e.Log.Info("source fingerprint captured", "backup_id", b.ID, "tables", len(fp.Tables))

	// Stream pg_dump into a private temp file while hashing.
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- internal path
	if err != nil {
		return failure.New(failure.BackupFailed, "create temp artifact", err)
	}
	hasher := sha256.New()
	cw := &countingHashWriter{w: f, h: hasher}
	dumpErr := postgres.Dump(ctx, tool, params, snapshot, cw, e.Redactor)
	syncErr := f.Sync()
	closeErr := f.Close()
	if cw.werr != nil {
		if errors.Is(cw.werr, os.ErrClosed) || failure.CategoryOf(cw.werr) == failure.InsufficientStorage {
			return failure.New(failure.InsufficientStorage, "writing backup artifact failed: disk full", cw.werr).
				WithHint("free disk space or point --data-dir at a larger volume")
		}
		return failure.New(failure.BackupFailed, "write artifact", cw.werr)
	}
	if dumpErr != nil {
		return dumpErr
	}
	if syncErr != nil || closeErr != nil {
		return failure.New(failure.InsufficientStorage, "flush artifact", errors.Join(syncErr, closeErr))
	}
	if err := tx.Commit(ctx); err != nil {
		return failure.New(failure.BackupFailed, "release snapshot", err)
	}
	if cw.n == 0 {
		return failure.New(failure.BackupFailed, "pg_dump produced an empty artifact", nil)
	}

	toc, err := postgres.ListArchive(ctx, restoreTool, tmpPath)
	if err != nil {
		return err
	}

	dir, err := e.Repo.BackupDir(b.ID)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return failure.New(failure.BackupFailed, "create backup directory", err)
	}
	*finalDir = dir
	artifactAbs := filepath.Join(dir, manifest.ArtifactName)
	if err := os.Rename(tmpPath, artifactAbs); err != nil {
		return failure.New(failure.BackupFailed, "move artifact into repository", err)
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	fpJSON, _ := json.Marshal(fp)
	verJSON, _ := json.Marshal(cfg.Verification)

	m := &manifest.Manifest{
		FormatVersion: manifest.FormatVersion, Tool: "unth-recover", ToolVersion: buildinfo.Version,
		BackupID: b.ID, WorkloadID: b.WorkloadID, BackupType: b.BackupType,
		CreatedAt: b.CreatedAt, SnapshotAt: snapAt, Source: cfg.Source.Redacted(),
		ServerVersion: ver, ServerMajor: major, DumpToolVersion: tool.Version,
		Artifact: manifest.Artifact{Path: manifest.ArtifactName, Format: "pg_dump-custom",
			SizeBytes: cw.n, SHA256: sum, TOCItems: toc},
		FingerprintDigest: fp.Digest(), Fingerprint: fp, Verification: cfg.Verification,
		Integrity: manifest.DefaultIntegrity(),
	}
	manifestAbs := filepath.Join(dir, manifest.FileName)
	if err := manifest.Write(manifestAbs, m); err != nil {
		return failure.New(failure.BackupFailed, "write manifest", err)
	}
	syncDir(dir)

	relArtifact, _ := e.Repo.Rel(artifactAbs)
	relManifest, _ := e.Repo.Rel(manifestAbs)
	now := time.Now().UTC()
	b.Status = metadata.BackupCreated
	b.CompletedAt = &now
	b.ArtifactPath, b.ManifestPath = relArtifact, relManifest
	b.SizeBytes, b.SHA256 = cw.n, sum
	b.SourceFingerprint = fp.Digest()
	b.FingerprintJSON = string(fpJSON)
	b.VerificationJSON = string(verJSON)
	b.PID = 0
	if err := e.Store.UpdateBackup(context.WithoutCancel(ctx), b); err != nil {
		return fmt.Errorf("record backup: %w", err)
	}
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil { // #nosec G304 -- internal path
		_ = d.Sync()
		_ = d.Close()
	}
}

func (e *Engine) audit(ctx context.Context, action, target, outcome, details string) {
	if err := e.Store.Audit(ctx, metadata.AuditEvent{Actor: repo.Actor(), Action: action, Target: target,
		Outcome: outcome, Details: e.Redactor.Redact(details)}); err != nil {
		e.Log.Warn("audit write failed", "error", err)
	}
}
