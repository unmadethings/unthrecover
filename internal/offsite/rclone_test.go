package offsite

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/metadata"
)

// TestRcloneBackend runs the full copy/check/fetch/prune cycle through the
// real rclone binary against a "local" type remote (same code path as Google
// Drive, OneDrive, Dropbox; only the remote's storage differs).
func TestRcloneBackend(t *testing.T) {
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("NOT RUN: rclone not installed")
	}
	ctx := context.Background()
	store := t.TempDir()
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(conf, []byte("[teststore]\ntype = local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	be, err := NewRclone(config.Offsite{Type: "rclone", Remote: "teststore:" + filepath.Join(store, "Unth Backups"), RcloneConfig: conf})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Get(ctx, "missing/object"); err != ErrNotExist {
		t.Fatalf("missing object: %v", err)
	}
	e := newEnv(t, true, true)
	e.c.Backend = be
	var bs []*metadata.Backup
	for i := 0; i < 3; i++ {
		b := e.addBackup(t, "shop", time.Now().UTC().Add(time.Duration(i)*time.Minute), secretContent+string(rune('a'+i)))
		rec, err := e.c.Copy(ctx, b)
		if err != nil || rec.Status != metadata.CopyVerified || rec.Target != be.Target() {
			t.Fatalf("copy: %v %+v", err, rec)
		}
		bs = append(bs, b)
	}
	list, err := e.c.List(ctx, "shop")
	if err != nil || len(list) != 3 || list[0].BackupID != bs[2].ID {
		t.Fatalf("list %v %+v", err, list)
	}
	// Tamper with a stored object directly on the remote's storage.
	art := filepath.Join(store, "Unth Backups", "unth", "shop", bs[0].ID, "artifact.dump.age")
	raw, err := os.ReadFile(art)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-3] ^= 1
	_ = os.WriteFile(art, raw, 0o600)
	if _, err := e.c.Check(ctx, bs[0]); failure.CategoryOf(err) != failure.DecryptionFailed {
		t.Fatalf("tampered: %v", err)
	}
	e2 := newEnv(t, true, true)
	c2 := &Copier{Backend: be, Keys: e.keys, Prefix: "unth", Store: e2.store, Repo: e2.repo}
	if fb, _, err := c2.Fetch(ctx, "shop", bs[1].ID); err != nil || fb.SHA256 != bs[1].SHA256 {
		t.Fatalf("fetch: %v", err)
	}
	res, err := e.c.Prune(ctx, "shop", config.Retention{KeepLast: 1}, false)
	if err != nil || len(res.Deleted) != 1 { // bs[0] is FAILED after the tamper check, so only bs[1] is pruned
		t.Fatalf("prune %v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(store, "Unth Backups", "unth", "shop", bs[1].ID, DescriptorName)); !os.IsNotExist(err) {
		t.Fatal("pruned copy still on the remote")
	}
}
