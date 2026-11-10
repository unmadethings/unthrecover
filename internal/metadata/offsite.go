package metadata

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Offsite copy states.
const (
	CopyUploading = "UPLOADING"
	CopyUploaded  = "UPLOADED"        // upload finished, not yet checked
	CopyVerified  = "REMOTE_VERIFIED" // downloaded again; hashes (and decryption, signature) checked
	CopyFailed    = "FAILED"
	CopyDeleted   = "DELETED" // removed by offsite retention
)

// OffsiteCopy is one copy of a backup on one offsite target.
type OffsiteCopy struct {
	BackupID      string     `json:"backup_id"`
	Target        string     `json:"target"`   // e.g. s3://bucket, rclone:gdrive:
	Location      string     `json:"location"` // key prefix of the copy
	Status        string     `json:"status"`
	Encrypted     bool       `json:"encrypted"`
	Signed        bool       `json:"signed"`
	SizeBytes     int64      `json:"size_bytes"`
	ObjectSHA256  string     `json:"object_sha256,omitempty"` // of the uploaded (encrypted) artifact object
	StartedAt     time.Time  `json:"started_at"`
	UploadedAt    *time.Time `json:"uploaded_at,omitempty"`
	CheckedAt     *time.Time `json:"checked_at,omitempty"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
	ErrorCategory string     `json:"error_category,omitempty"`
	ErrorMessage  string     `json:"error_message,omitempty"`
}

// UpsertCopy inserts or replaces the copy record of (backup, target).
func (s *Store) UpsertCopy(ctx context.Context, c *OffsiteCopy) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO offsite_copies (backup_id, target, location, status, encrypted, signed,
		size_bytes, object_sha256, started_at, uploaded_at, checked_at, deleted_at, error_category, error_message)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(backup_id, target) DO UPDATE SET location=excluded.location, status=excluded.status,
		encrypted=excluded.encrypted, signed=excluded.signed, size_bytes=excluded.size_bytes,
		object_sha256=excluded.object_sha256, started_at=excluded.started_at, uploaded_at=excluded.uploaded_at,
		checked_at=excluded.checked_at, deleted_at=excluded.deleted_at, error_category=excluded.error_category,
		error_message=excluded.error_message`,
		c.BackupID, c.Target, c.Location, c.Status, c.Encrypted, c.Signed, c.SizeBytes, c.ObjectSHA256,
		fmtTime(c.StartedAt), fmtTimePtr(c.UploadedAt), fmtTimePtr(c.CheckedAt), fmtTimePtr(c.DeletedAt),
		c.ErrorCategory, c.ErrorMessage)
	return err
}

const copyCols = `backup_id, target, location, status, encrypted, signed, COALESCE(size_bytes,0),
	COALESCE(object_sha256,''), started_at, uploaded_at, checked_at, deleted_at,
	COALESCE(error_category,''), COALESCE(error_message,'')`

func scanCopy(row interface{ Scan(...any) error }) (*OffsiteCopy, error) {
	var c OffsiteCopy
	var started string
	var up, ck, del sql.NullString
	if err := row.Scan(&c.BackupID, &c.Target, &c.Location, &c.Status, &c.Encrypted, &c.Signed, &c.SizeBytes,
		&c.ObjectSHA256, &started, &up, &ck, &del, &c.ErrorCategory, &c.ErrorMessage); err != nil {
		return nil, err
	}
	c.StartedAt = parseTime(started)
	c.UploadedAt, c.CheckedAt, c.DeletedAt = parseTimePtr(up), parseTimePtr(ck), parseTimePtr(del)
	return &c, nil
}

// GetCopy returns the copy of a backup on a target, or ErrNotFound.
func (s *Store) GetCopy(ctx context.Context, backupID, target string) (*OffsiteCopy, error) {
	c, err := scanCopy(s.db.QueryRowContext(ctx, `SELECT `+copyCols+` FROM offsite_copies WHERE backup_id=? AND target=?`, backupID, target))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ListCopies returns the copies of one backup (all targets).
func (s *Store) ListCopies(ctx context.Context, backupID string) ([]*OffsiteCopy, error) {
	return s.queryCopies(ctx, `SELECT `+copyCols+` FROM offsite_copies WHERE backup_id=? ORDER BY target`, backupID)
}

// ListCopiesByWorkload returns all copies of a workload's backups on a target, newest first.
func (s *Store) ListCopiesByWorkload(ctx context.Context, workload, target string) ([]*OffsiteCopy, error) {
	return s.queryCopies(ctx, `SELECT c.backup_id, c.target, c.location, c.status, c.encrypted, c.signed,
		COALESCE(c.size_bytes,0), COALESCE(c.object_sha256,''), c.started_at, c.uploaded_at, c.checked_at,
		c.deleted_at, COALESCE(c.error_category,''), COALESCE(c.error_message,'')
		FROM offsite_copies c JOIN backups b ON b.id = c.backup_id
		WHERE b.workload_id=? AND c.target=? ORDER BY b.created_at DESC`, workload, target)
}

// CountVerifiedCopies returns the number of REMOTE_VERIFIED copies and the newest check time.
func (s *Store) CountVerifiedCopies(ctx context.Context) (int, *time.Time, error) {
	var n int
	var last sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(checked_at) FROM offsite_copies WHERE status=?`, CopyVerified).Scan(&n, &last)
	return n, parseTimePtr(last), err
}

func (s *Store) queryCopies(ctx context.Context, q string, args ...any) ([]*OffsiteCopy, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OffsiteCopy
	for rows.Next() {
		c, err := scanCopy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
