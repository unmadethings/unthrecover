package metadata

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.SchemaVersion(ctx)
	if v != len(migrations) {
		t.Fatalf("schema version %d", v)
	}
	fi, _ := os.Stat(filepath.Join(dir, "metadata.db"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("metadata.db mode %o", fi.Mode().Perm())
	}

	b := &Backup{ID: "bk-1-aaaa", WorkloadID: "w", BackupType: "postgres-logical", Status: BackupPending,
		CreatedAt: time.Now(), SourceJSON: "{}", PID: 42}
	if err := s.InsertBackup(ctx, b); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	b.Status, b.SHA256, b.SizeBytes, b.SnapshotAt = BackupCreated, "abc", 10, &now
	if err := s.UpdateBackup(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBackup(ctx, b.ID)
	if err != nil || got.Status != BackupCreated || got.SHA256 != "abc" || got.VerificationStatus != VerifyNone || got.SnapshotAt == nil {
		t.Fatalf("got %+v %v", got, err)
	}
	if _, err := s.GetBackup(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal("expected ErrNotFound")
	}
	if err := s.UpdateBackup(ctx, &Backup{ID: "nope", VerificationStatus: VerifyNone}); !errors.Is(err, ErrNotFound) {
		t.Fatal("update missing should be ErrNotFound")
	}

	r := &Run{ID: "vr-1-aaaa", BackupID: b.ID, Status: VerifyRunning, StartedAt: time.Now(), PID: 42}
	if err := s.InsertRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	un, _ := s.UnfinishedRuns(ctx)
	if len(un) != 1 {
		t.Fatalf("unfinished runs %d", len(un))
	}
	r.Status = VerifyVerified
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	un, _ = s.UnfinishedRuns(ctx)
	runs, _ := s.ListRuns(ctx, b.ID)
	if len(un) != 0 || len(runs) != 1 {
		t.Fatal("run lifecycle")
	}
	if err := s.InsertRun(ctx, &Run{ID: "vr-2-bbbb", BackupID: "missing", Status: VerifyRunning, StartedAt: time.Now()}); err == nil {
		t.Fatal("foreign key not enforced")
	}

	if err := s.Audit(ctx, AuditEvent{Actor: "t", Action: "a", Target: "x", Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.ListAudit(ctx, 10)
	if len(evs) != 1 {
		t.Fatal("audit")
	}
	s.Close()

	// Re-open: migrations must be idempotent.
	s2, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	bs, _ := s2.ListBackups(ctx, 10)
	if len(bs) != 1 {
		t.Fatal("data lost across reopen")
	}
}

func TestRejectsNewerSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (999, 'x')`)
	s.Close()
	if _, err := Open(ctx, dir); err == nil {
		t.Fatal("newer schema accepted")
	}
}
