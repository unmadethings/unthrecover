// Package metadata persists backups, verification runs and audit events in a
// local SQLite database with versioned, forward-only migrations.
package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)

	"github.com/unmadethings/unthrecover/internal/security"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

// Backup statuses.
const (
	BackupPending  = "PENDING"
	BackupRunning  = "RUNNING"
	BackupCreated  = "BACKUP_CREATED"
	BackupFailed   = "FAILED"
	BackupCanceled = "CANCELLED"
	BackupExpired  = "EXPIRED" // artifact removed by retention (`unth prune`)
)

// Verification statuses. VERIFIED is only ever written by the verification
// engine after a real isolated restore and all mandatory checks passed.
const (
	VerifyNone             = "NOT_VERIFIED"
	VerifyPending          = "PENDING"
	VerifyRunning          = "RUNNING"
	VerifyIntegrityChecked = "INTEGRITY_CHECKED"
	VerifyRestoreTesting   = "RESTORE_TESTING"
	VerifyVerified         = "VERIFIED"
	VerifyFailed           = "FAILED"
	VerifyCanceled         = "CANCELLED"
)

// Store wraps the SQLite metadata database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (creating if needed) the metadata database in dataDir and runs
// pending migrations.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	if err := security.EnsurePrivateDir(dataDir); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	path := filepath.Join(dataDir, "metadata.db")
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: metadata database is a symbolic link", security.ErrUnsafePath)
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open metadata db: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	s := &Store{db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SchemaVersion returns the applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&v)
	return v, err
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("migrations table: %w", err)
	}
	current, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if current > len(migrations) {
		return fmt.Errorf("metadata schema version %d is newer than this binary supports (%d); upgrade unth", current, len(migrations))
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
			i+1, now()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// migrations are append-only. Never edit an existing entry.
var migrations = []string{
	// 1: initial schema
	`CREATE TABLE backups (
		id TEXT PRIMARY KEY,
		workload_id TEXT NOT NULL,
		backup_type TEXT NOT NULL,
		status TEXT NOT NULL,
		verification_status TEXT NOT NULL DEFAULT 'NOT_VERIFIED',
		created_at TEXT NOT NULL,
		completed_at TEXT,
		snapshot_at TEXT,
		source_json TEXT NOT NULL,
		source_fingerprint TEXT,
		fingerprint_json TEXT,
		verification_json TEXT,
		artifact_path TEXT,
		manifest_path TEXT,
		size_bytes INTEGER,
		sha256 TEXT,
		server_version TEXT,
		server_major INTEGER,
		tool_version TEXT,
		error_category TEXT,
		error_message TEXT,
		pid INTEGER,
		last_verification_id TEXT
	);
	CREATE INDEX idx_backups_workload ON backups(workload_id, created_at);
	CREATE TABLE verification_runs (
		id TEXT PRIMARY KEY,
		backup_id TEXT NOT NULL REFERENCES backups(id),
		status TEXT NOT NULL,
		started_at TEXT NOT NULL,
		finished_at TEXT,
		restore_duration_ms INTEGER,
		total_duration_ms INTEGER,
		error_category TEXT,
		error_message TEXT,
		report_json TEXT,
		pid INTEGER
	);
	CREATE INDEX idx_runs_backup ON verification_runs(backup_id, started_at);
	CREATE TABLE audit_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at TEXT NOT NULL,
		actor TEXT NOT NULL,
		action TEXT NOT NULL,
		target TEXT NOT NULL,
		outcome TEXT NOT NULL,
		details TEXT
	);`,
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Backup is a backup record.
type Backup struct {
	ID                 string
	WorkloadID         string
	BackupType         string
	Status             string
	VerificationStatus string
	CreatedAt          time.Time
	CompletedAt        *time.Time
	SnapshotAt         *time.Time
	SourceJSON         string
	SourceFingerprint  string
	FingerprintJSON    string
	VerificationJSON   string
	ArtifactPath       string
	ManifestPath       string
	SizeBytes          int64
	SHA256             string
	ServerVersion      string
	ServerMajor        int
	ToolVersion        string
	ErrorCategory      string
	ErrorMessage       string
	PID                int
	LastVerificationID string
}

// InsertBackup creates a backup record.
func (s *Store) InsertBackup(ctx context.Context, b *Backup) error {
	if b.VerificationStatus == "" {
		b.VerificationStatus = VerifyNone
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO backups
		(id, workload_id, backup_type, status, verification_status, created_at, source_json, pid)
		VALUES (?,?,?,?,?,?,?,?)`,
		b.ID, b.WorkloadID, b.BackupType, b.Status, b.VerificationStatus, fmtTime(b.CreatedAt), b.SourceJSON, b.PID)
	return err
}

// UpdateBackup writes all mutable fields of a backup record.
func (s *Store) UpdateBackup(ctx context.Context, b *Backup) error {
	if b.VerificationStatus == "" {
		return errors.New("metadata: verification_status must not be empty")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE backups SET
		status=?, verification_status=?, completed_at=?, snapshot_at=?, source_fingerprint=?,
		fingerprint_json=?, verification_json=?, artifact_path=?, manifest_path=?, size_bytes=?, sha256=?,
		server_version=?, server_major=?, tool_version=?, error_category=?, error_message=?, pid=?,
		last_verification_id=?
		WHERE id=?`,
		b.Status, b.VerificationStatus, fmtTimePtr(b.CompletedAt), fmtTimePtr(b.SnapshotAt), b.SourceFingerprint,
		b.FingerprintJSON, b.VerificationJSON, b.ArtifactPath, b.ManifestPath, b.SizeBytes, b.SHA256,
		b.ServerVersion, b.ServerMajor, b.ToolVersion, b.ErrorCategory, b.ErrorMessage, b.PID,
		b.LastVerificationID, b.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const backupCols = `id, workload_id, backup_type, status, verification_status, created_at, completed_at,
	snapshot_at, source_json, COALESCE(source_fingerprint,''), COALESCE(fingerprint_json,''),
	COALESCE(verification_json,''), COALESCE(artifact_path,''), COALESCE(manifest_path,''),
	COALESCE(size_bytes,0), COALESCE(sha256,''), COALESCE(server_version,''), COALESCE(server_major,0),
	COALESCE(tool_version,''), COALESCE(error_category,''), COALESCE(error_message,''), COALESCE(pid,0),
	COALESCE(last_verification_id,'')`

func scanBackup(row interface{ Scan(...any) error }) (*Backup, error) {
	var b Backup
	var created string
	var completed, snapshot sql.NullString
	err := row.Scan(&b.ID, &b.WorkloadID, &b.BackupType, &b.Status, &b.VerificationStatus, &created,
		&completed, &snapshot, &b.SourceJSON, &b.SourceFingerprint, &b.FingerprintJSON, &b.VerificationJSON,
		&b.ArtifactPath, &b.ManifestPath, &b.SizeBytes, &b.SHA256, &b.ServerVersion, &b.ServerMajor,
		&b.ToolVersion, &b.ErrorCategory, &b.ErrorMessage, &b.PID, &b.LastVerificationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.CreatedAt = parseTime(created)
	b.CompletedAt = parseTimePtr(completed)
	b.SnapshotAt = parseTimePtr(snapshot)
	return &b, nil
}

// GetBackup loads a backup by ID.
func (s *Store) GetBackup(ctx context.Context, id string) (*Backup, error) {
	return scanBackup(s.db.QueryRowContext(ctx, `SELECT `+backupCols+` FROM backups WHERE id=?`, id))
}

// ListBackups returns backups, newest first.
func (s *Store) ListBackups(ctx context.Context, limit int) ([]*Backup, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupCols+` FROM backups ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListBackupsByWorkload returns all backups of a workload, newest first.
func (s *Store) ListBackupsByWorkload(ctx context.Context, workload string) ([]*Backup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupCols+` FROM backups WHERE workload_id=? ORDER BY created_at DESC`, workload)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Run is a verification run record. Every `unth verify` invocation creates a
// new run with its own ID; previous results are never reused.
type Run struct {
	ID                string
	BackupID          string
	Status            string
	StartedAt         time.Time
	FinishedAt        *time.Time
	RestoreDurationMS int64
	TotalDurationMS   int64
	ErrorCategory     string
	ErrorMessage      string
	ReportJSON        string
	PID               int
}

// InsertRun creates a verification run record.
func (s *Store) InsertRun(ctx context.Context, r *Run) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO verification_runs (id, backup_id, status, started_at, pid)
		VALUES (?,?,?,?,?)`, r.ID, r.BackupID, r.Status, fmtTime(r.StartedAt), r.PID)
	return err
}

// UpdateRun writes all mutable fields of a run.
func (s *Store) UpdateRun(ctx context.Context, r *Run) error {
	_, err := s.db.ExecContext(ctx, `UPDATE verification_runs SET status=?, finished_at=?, restore_duration_ms=?,
		total_duration_ms=?, error_category=?, error_message=?, report_json=? WHERE id=?`,
		r.Status, fmtTimePtr(r.FinishedAt), r.RestoreDurationMS, r.TotalDurationMS, r.ErrorCategory,
		r.ErrorMessage, r.ReportJSON, r.ID)
	return err
}

const runCols = `id, backup_id, status, started_at, finished_at, COALESCE(restore_duration_ms,0),
	COALESCE(total_duration_ms,0), COALESCE(error_category,''), COALESCE(error_message,''),
	COALESCE(report_json,''), COALESCE(pid,0)`

func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var started string
	var finished sql.NullString
	err := row.Scan(&r.ID, &r.BackupID, &r.Status, &started, &finished, &r.RestoreDurationMS,
		&r.TotalDurationMS, &r.ErrorCategory, &r.ErrorMessage, &r.ReportJSON, &r.PID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.StartedAt = parseTime(started)
	r.FinishedAt = parseTimePtr(finished)
	return &r, nil
}

// GetRun loads a verification run by ID.
func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM verification_runs WHERE id=?`, id))
}

// ListRuns returns runs for a backup, newest first.
func (s *Store) ListRuns(ctx context.Context, backupID string) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM verification_runs WHERE backup_id=?
		ORDER BY started_at DESC`, backupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRunsSince returns verification runs started at or after t.
func (s *Store) ListRunsSince(ctx context.Context, t time.Time) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM verification_runs WHERE started_at >= ? ORDER BY started_at DESC`, fmtTime(t))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InterruptedBackups returns backups left in PENDING/RUNNING state.
func (s *Store) InterruptedBackups(ctx context.Context) ([]*Backup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupCols+` FROM backups WHERE status IN ('PENDING','RUNNING')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UnfinishedRuns returns verification runs that never reached a final state.
func (s *Store) UnfinishedRuns(ctx context.Context) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM verification_runs
		WHERE status IN ('PENDING','RUNNING','INTEGRITY_CHECKED','RESTORE_TESTING')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AuditEvent is an append-only audit trail entry. Details must never contain
// secret values.
type AuditEvent struct {
	ID      int64
	At      time.Time
	Actor   string
	Action  string
	Target  string
	Outcome string
	Details string
}

// Audit appends an audit event.
func (s *Store) Audit(ctx context.Context, e AuditEvent) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_events (at, actor, action, target, outcome, details)
		VALUES (?,?,?,?,?,?)`, fmtTime(e.At), e.Actor, e.Action, e.Target, e.Outcome, e.Details)
	return err
}

// ListAudit returns audit events, newest first.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, actor, action, target, outcome, COALESCE(details,'')
		FROM audit_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var at string
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Target, &e.Outcome, &e.Details); err != nil {
			return nil, err
		}
		e.At = parseTime(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func parseTimePtr(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := parseTime(s.String)
	return &t
}
