package retention

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/repo"
)

var now = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// mk creates n backups, one every `step` back from now, newest first.
func mk(n int, step time.Duration, verified map[int]string) []*metadata.Backup {
	var out []*metadata.Backup
	for i := 0; i < n; i++ {
		vs := metadata.VerifyNone
		if v, ok := verified[i]; ok {
			vs = v
		}
		out = append(out, &metadata.Backup{ID: idFor(i), Status: metadata.BackupCreated,
			VerificationStatus: vs, CreatedAt: now.Add(-time.Duration(i) * step)})
	}
	return out
}

func idFor(i int) string { return "bk-" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + "-x" }

func kept(ds []Decision) map[string]bool {
	m := map[string]bool{}
	for _, d := range ds {
		if d.Keep {
			m[d.BackupID] = true
		}
	}
	return m
}

func TestKeepLast(t *testing.T) {
	ds := Plan(mk(5, time.Hour, nil), config.Retention{KeepLast: 2}, now)
	k := kept(ds)
	if len(k) != 2 || !k[idFor(0)] || !k[idFor(1)] {
		t.Fatalf("kept %v", k)
	}
}

func TestNewestVerifiedAlwaysKept(t *testing.T) {
	// Only the 4th backup was verified; keep_last 1 must still keep it.
	bs := mk(5, time.Hour, map[int]string{3: metadata.VerifyVerified, 4: metadata.VerifyVerified, 0: metadata.VerifyFailed})
	k := kept(Plan(bs, config.Retention{KeepLast: 1}, now))
	if !k[idFor(0)] || !k[idFor(3)] || k[idFor(4)] || len(k) != 2 {
		t.Fatalf("kept %v", k)
	}
	// Even max_age cannot expire the newest verified backup.
	k = kept(Plan(bs, config.Retention{MaxAge: config.Duration(time.Hour)}, now))
	if !k[idFor(3)] {
		t.Fatalf("newest verified expired by max_age: %v", k)
	}
}

func TestInProgressVerificationKept(t *testing.T) {
	bs := mk(3, time.Hour, map[int]string{2: metadata.VerifyRestoreTesting})
	if k := kept(Plan(bs, config.Retention{KeepLast: 1}, now)); !k[idFor(2)] {
		t.Fatalf("in-progress verification expired: %v", k)
	}
}

func TestKeepDailyWeekly(t *testing.T) {
	bs := mk(48, 6*time.Hour, nil) // 4 per day over 12 days
	k := kept(Plan(bs, config.Retention{KeepDaily: 3}, now))
	if len(k) != 3 {
		t.Fatalf("keep_daily 3 kept %d: %v", len(k), k)
	}
	for _, i := range []int{0, 3, 7} { // 12:00 today, 18:00 on each of the two previous days
		if !k[idFor(i)] {
			t.Fatalf("expected newest-per-day %d kept: %v", i, k)
		}
	}
	k = kept(Plan(bs, config.Retention{KeepWeekly: 2}, now))
	if len(k) != 2 {
		t.Fatalf("keep_weekly 2 kept %d", len(k))
	}
}

func TestMaxAge(t *testing.T) {
	bs := mk(10, 24*time.Hour, nil)
	k := kept(Plan(bs, config.Retention{MaxAge: config.Duration(72 * time.Hour)}, now))
	if len(k) != 4 { // ages 0,24,48,72h
		t.Fatalf("max_age alone kept %d: %v", len(k), k)
	}
	// combined: keep_last 8 but max_age 72h expires older ones
	k = kept(Plan(bs, config.Retention{KeepLast: 8, MaxAge: config.Duration(72 * time.Hour)}, now))
	if len(k) != 4 {
		t.Fatalf("keep_last+max_age kept %d", len(k))
	}
}

func TestOnlyCreatedBackupsConsidered(t *testing.T) {
	bs := mk(3, time.Hour, nil)
	bs[1].Status = metadata.BackupFailed
	bs[2].Status = metadata.BackupExpired
	ds := Plan(bs, config.Retention{KeepLast: 1}, now)
	if len(ds) != 1 || ds[0].BackupID != idFor(0) {
		t.Fatalf("%+v", ds)
	}
}

func TestApplyExpiresAndKeepsHistory(t *testing.T) {
	ctx := context.Background()
	r, err := repo.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := metadata.Open(ctx, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, b := range mk(3, time.Hour, map[int]string{2: metadata.VerifyVerified}) {
		b.WorkloadID, b.SourceJSON, b.BackupType = "w", "{}", "postgres-logical"
		b.ID = []string{"bk-new-aaaa", "bk-mid-aaaa", "bk-old-aaaa"}[i]
		if err := s.InsertBackup(ctx, b); err != nil {
			t.Fatal(err)
		}
		b.Status = metadata.BackupCreated
		_ = s.UpdateBackup(ctx, b)
		dir, _ := r.BackupDir(b.ID)
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, "artifact.dump"), []byte("x"), 0o600)
	}
	if _, err := Apply(ctx, s, r, "w", config.Retention{}, false, "t", now); err == nil {
		t.Fatal("empty policy must be refused")
	}
	res, err := Apply(ctx, s, r, "w", config.Retention{KeepLast: 1}, true, "t", now)
	if err != nil || len(res.Expired) != 1 || res.Expired[0] != "bk-mid-aaaa" {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	if b, _ := s.GetBackup(ctx, "bk-mid-aaaa"); b.Status != metadata.BackupCreated {
		t.Fatal("dry run changed state")
	}
	res, err = Apply(ctx, s, r, "w", config.Retention{KeepLast: 1}, false, "t", now)
	if err != nil || len(res.Expired) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	b, _ := s.GetBackup(ctx, "bk-mid-aaaa")
	dir, _ := r.BackupDir("bk-mid-aaaa")
	if _, statErr := os.Stat(dir); b.Status != metadata.BackupExpired || b.ArtifactPath != "" || !os.IsNotExist(statErr) {
		t.Fatalf("not expired: %+v %v", b, statErr)
	}
	if b, _ := s.GetBackup(ctx, "bk-old-aaaa"); b.Status != metadata.BackupCreated {
		t.Fatal("newest verified backup was expired")
	}
	evs, _ := s.ListAudit(ctx, 10)
	if len(evs) == 0 || evs[0].Action != "prune.expire" {
		t.Fatal("no audit event")
	}
}
