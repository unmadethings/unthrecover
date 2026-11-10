//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/unmadethings/unthrecover/internal/manifest"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/postgres"
)

// M0-01..04: healthy backup, isolated restore, records match, schema/constraints match.
func TestHealthyBackupIsVerified(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{queries: true}))

	rep, r := e.verify(id)
	if r.code != 0 || rep == nil || rep.FinalStatus != "VERIFIED" {
		t.Fatalf("expected VERIFIED: code=%d stdout=%s stderr=%s", r.code, r.stdout, r.stderr)
	}
	for _, s := range rep.Steps {
		if s.Status != "PASS" {
			t.Errorf("step %s = %s", s.Name, s.Status)
		}
	}
	// Row totals must equal the source computed independently with psql.
	want, _ := strconv.ParseInt(psqlValue(t, src, "shop", `SELECT (SELECT count(*) FROM customers)+(SELECT count(*) FROM products)+
		(SELECT count(*) FROM orders)+(SELECT count(*) FROM order_items)+(SELECT count(*) FROM billing.invoices)+
		(SELECT count(*) FROM room_bookings)+(SELECT count(*) FROM audit_log)`), 10, 64)
	if rep.Comparison == nil || rep.Comparison.RowsActual != want || rep.Comparison.RowsExpected != want {
		t.Fatalf("row totals: want %d, got %+v", want, rep.Comparison)
	}
	if rep.Comparison.TablesChecked != 7 || rep.Comparison.TablesHashed != 7 || rep.Comparison.QueriesChecked != 2 {
		t.Fatalf("coverage: %+v", rep.Comparison)
	}
	if rep.Cleanup.Status != "PASS" || len(rep.Cleanup.Removed) != 3 {
		t.Fatalf("cleanup: %+v", rep.Cleanup)
	}
	e.assertNoResources(rep.RunID)

	// report command returns the same run, in both formats.
	rr := e.run("report", id, "--format", "json")
	var stored report
	if rr.code != 0 || json.Unmarshal([]byte(rr.stdout), &stored) != nil || stored.RunID != rep.RunID || stored.FinalStatus != "VERIFIED" {
		t.Fatalf("report json: %d %s", rr.code, rr.stdout)
	}
	if rt := e.run("report", id); rt.code != 0 || !strings.Contains(rt.stdout, "Final status:      VERIFIED") {
		t.Fatalf("report text: %s", rt.stdout)
	}
	if rl := e.run("backup", "list"); !strings.Contains(rl.stdout, "VERIFIED") {
		t.Fatalf("backup list: %s", rl.stdout)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "reports", rep.RunID+".json")); err != nil {
		t.Fatalf("report file: %v", err)
	}
}

// Idempotency: every verify is a new, real run.
func TestRepeatedVerifyCreatesNewRuns(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{}))
	r1, res1 := e.verify(id)
	r2, res2 := e.verify(id)
	if res1.code != 0 || res2.code != 0 || r1.RunID == r2.RunID {
		t.Fatalf("runs: %v %v", r1, r2)
	}
	show := e.run("backup", "show", id, "--format", "json")
	var v struct {
		Runs []map[string]any `json:"verification_runs"`
	}
	_ = json.Unmarshal([]byte(show.stdout), &v)
	if len(v.Runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(v.Runs))
	}
	// Each run had its own sandbox; both removed.
	e.assertNoResources(r1.RunID)
	e.assertNoResources(r2.RunID)
}

// M0-05: checksum corruption → FAIL, no restore attempted.
func TestChecksumCorruptionFails(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{}))
	p := e.artifactPath(id)
	b, _ := os.ReadFile(p)
	b[len(b)/2] ^= 0xFF
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, r := e.verify(id)
	if r.code != 3 || rep.FinalStatus != "FAILED" || rep.ErrorCategory != "CHECKSUM_MISMATCH" {
		t.Fatalf("code=%d rep=%+v", r.code, rep)
	}
	if rep.step("restore") != "NOT_RUN" || rep.step("isolated_environment") != "NOT_RUN" {
		t.Fatal("restore must not run after a checksum failure")
	}
	e.assertNoResources(rep.RunID)
}

// Critical rule: a corrupted artifact whose checksum was ALSO updated
// (bit-rot before hashing, or tampering of artifact + manifest + metadata)
// must still never be VERIFIED, because the restore itself is tested.
func TestConsistentlyCorruptedArtifactIsNeverVerified(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{}))
	p := e.artifactPath(id)
	b, _ := os.ReadFile(p)
	b = b[:len(b)*2/3] // truncated archive
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteChecksums(t, e, id)

	rep, r := e.verify(id)
	if r.code != 3 || rep.FinalStatus == "VERIFIED" {
		t.Fatalf("corrupted backup verified! code=%d rep=%+v", r.code, rep)
	}
	if rep.step("checksum_sha256") != "PASS" {
		t.Fatalf("precondition: checksum should match after rewrite, got %s", rep.step("checksum_sha256"))
	}
	if rep.ErrorCategory != "ARTIFACT_INVALID" && rep.ErrorCategory != "RESTORE_FAILED" {
		t.Fatalf("unexpected category %s: %s", rep.ErrorCategory, rep.ErrorMessage)
	}
	e.assertNoResources(rep.RunID)
}

// rewriteChecksums makes metadata + manifest agree with the bytes on disk.
func rewriteChecksums(t *testing.T, e *env, id string) {
	t.Helper()
	sum, n, err := manifest.HashFile(e.artifactPath(id))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := metadata.Open(ctx, e.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bk, err := s.GetBackup(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	bk.SHA256, bk.SizeBytes = sum, n
	if err := s.UpdateBackup(ctx, bk); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(e.dataDir, "backups", id, manifest.FileName)
	m, err := manifest.Read(mp)
	if err != nil {
		t.Fatal(err)
	}
	m.Artifact.SHA256, m.Artifact.SizeBytes = sum, n
	_ = os.Remove(mp)
	if err := manifest.Write(mp, m); err != nil {
		t.Fatal(err)
	}
}

// The data comparison is real: if the expected fingerprint differs from what
// is restored, verification fails with DATA_MISMATCH.
func TestDataMismatchIsDetected(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{}))
	ctx := context.Background()
	s, err := metadata.Open(ctx, e.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	bk, _ := s.GetBackup(ctx, id)
	var fp postgres.Fingerprint
	_ = json.Unmarshal([]byte(bk.FingerprintJSON), &fp)
	fp.Tables[0].Rows++ // expect one more row than the dump contains
	raw, _ := json.Marshal(&fp)
	bk.FingerprintJSON, bk.SourceFingerprint = string(raw), fp.Digest()
	if err := s.UpdateBackup(ctx, bk); err != nil {
		t.Fatal(err)
	}
	s.Close()
	mp := filepath.Join(e.dataDir, "backups", id, manifest.FileName)
	m, _ := manifest.Read(mp)
	m.Fingerprint, m.FingerprintDigest = &fp, fp.Digest()
	_ = os.Remove(mp)
	_ = manifest.Write(mp, m)

	rep, r := e.verify(id)
	if r.code != 3 || rep.FinalStatus != "FAILED" || rep.ErrorCategory != "DATA_MISMATCH" {
		t.Fatalf("code=%d rep=%+v", r.code, rep)
	}
	if rep.step("restore") != "PASS" {
		t.Fatal("restore should have run and passed")
	}
	e.assertNoResources(rep.RunID)
}

// Manifest tampering without metadata update is detected.
func TestManifestTamperingFails(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{}))
	mp := filepath.Join(e.dataDir, "backups", id, manifest.FileName)
	m, _ := manifest.Read(mp)
	m.Artifact.SHA256 = strings.Repeat("0", 64)
	_ = os.Remove(mp)
	_ = manifest.Write(mp, m)
	rep, r := e.verify(id)
	if r.code != 3 || rep.ErrorCategory != "CHECKSUM_MISMATCH" {
		t.Fatalf("code=%d rep=%+v", r.code, rep)
	}
}

// Symlinked artifact is rejected (path safety).
func TestSymlinkedArtifactRejected(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{}))
	p := e.artifactPath(id)
	moved := filepath.Join(t.TempDir(), "elsewhere.dump")
	if err := os.Rename(p, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, p); err != nil {
		t.Fatal(err)
	}
	rep, r := e.verify(id)
	if r.code != 3 || rep.ErrorCategory != "ARTIFACT_INVALID" {
		t.Fatalf("code=%d rep=%+v", r.code, rep)
	}
}

// M0-06: missing artifact → FAIL.
func TestMissingArtifactFails(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{}))
	if err := os.Remove(e.artifactPath(id)); err != nil {
		t.Fatal(err)
	}
	rep, r := e.verify(id)
	if r.code != 3 || rep.FinalStatus != "FAILED" || rep.ErrorCategory != "ARTIFACT_MISSING" {
		t.Fatalf("code=%d rep=%+v", r.code, rep)
	}
	e.assertNoResources(rep.RunID)
}

// M0-07 + M0-12: wrong credentials → FAIL; secrets never leak.
func TestWrongCredentialsFailWithoutLeakingSecrets(t *testing.T) {
	e := newEnv(t)
	wrong := "WrongPassw0rd-" + randHex(8)
	pf := filepath.Join(t.TempDir(), "wrong.pass")
	_ = os.WriteFile(pf, []byte(wrong), 0o600)
	v, r := e.backup(e.config(cfgOpts{passFile: pf}))
	if r.code != 1 || !strings.Contains(r.stderr, "AUTHENTICATION_FAILED") {
		t.Fatalf("code=%d stderr=%s", r.code, r.stderr)
	}
	if v["status"] != "FAILED" || v["error_category"] != "AUTHENTICATION_FAILED" {
		t.Fatalf("record: %v", v)
	}
	assertNoSecret(t, e, wrong)
	if _, err := os.Stat(filepath.Join(e.dataDir, "backups", v["id"].(string))); !os.IsNotExist(err) {
		t.Fatal("failed backup left a directory behind")
	}
}

// M0-12: run a full successful flow at debug log level and scan every output
// and every persisted file for the source password.
func TestSecretsDoNotLeak(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{queries: true}))
	if _, r := e.verify(id); r.code != 0 {
		t.Fatalf("verify: %s", r.stderr)
	}
	e.run("report", id)
	e.run("audit", "list")
	e.run("backup", "show", id, "--format", "json")
	assertNoSecret(t, e, src.Password)
}

func assertNoSecret(t *testing.T, e *env, secret string) {
	t.Helper()
	if strings.Contains(e.logs.String(), secret) {
		t.Fatal("secret found in CLI output/logs")
	}
	_ = filepath.Walk(e.dataDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), secret) {
			t.Errorf("secret found in %s", p)
		}
		return nil
	})
}

// M0-08: interruption (SIGINT) during restore → CANCELLED + safe cleanup.
func TestInterruptDuringRestoreCleansUp(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{db: bigDB(t)}))
	cmd := e.command(context.Background(), "verify", id, "--format", "json")
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if !waitChild(cmd.Process.Pid, "pg_restore", 3*time.Minute) {
		_ = cmd.Process.Kill()
		t.Fatal("pg_restore never started")
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	err := cmd.Wait()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	var rep report
	_ = json.Unmarshal([]byte(out.String()), &rep)
	if code != 130 || rep.FinalStatus != "CANCELLED" {
		t.Fatalf("code=%d final=%s out=%s", code, rep.FinalStatus, out.String())
	}
	if rep.step("restore") != "FAIL" {
		t.Fatalf("restore step should be interrupted, got %s", rep.step("restore"))
	}
	if rep.Cleanup.Status != "PASS" {
		t.Fatalf("cleanup: %+v", rep.Cleanup)
	}
	e.assertNoResources(rep.RunID)
}

// Interrupted-operation recovery: SIGKILL leaves state behind; `unth cleanup`
// reconciles metadata and removes only that run's resources.
func TestKillDuringRestoreThenCleanup(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{db: bigDB(t)}))
	cmd := e.command(context.Background(), "verify", id)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if !waitChild(cmd.Process.Pid, "pg_restore", 3*time.Minute) {
		_ = cmd.Process.Kill()
		t.Fatal("pg_restore never started")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	ctx := context.Background()
	s, _ := metadata.Open(ctx, e.dataDir)
	bk, _ := s.GetBackup(ctx, id)
	runID := bk.LastVerificationID
	run, _ := s.GetRun(ctx, runID)
	s.Close()
	if run.Status != "RESTORE_TESTING" {
		t.Fatalf("expected RESTORE_TESTING after kill, got %s", run.Status)
	}
	if len(managedResources(runID)) == 0 {
		t.Fatal("expected leftover resources after SIGKILL (test precondition)")
	}
	dry := e.run("cleanup", "--dry-run")
	if dry.code != 0 || !strings.Contains(dry.stdout, runID) || len(managedResources(runID)) == 0 {
		t.Fatalf("dry-run must not change anything: %s", dry.stdout)
	}
	cl := e.run("cleanup")
	if cl.code != 0 {
		t.Fatalf("cleanup: %s %s", cl.stdout, cl.stderr)
	}
	e.assertNoResources(runID)
	s, _ = metadata.Open(ctx, e.dataDir)
	defer s.Close()
	run, _ = s.GetRun(ctx, runID)
	bk, _ = s.GetBackup(ctx, id)
	if run.Status != "FAILED" || run.ErrorCategory != "INTERRUPTED" || bk.VerificationStatus != "FAILED" {
		t.Fatalf("after cleanup run=%s/%s backup=%s", run.Status, run.ErrorCategory, bk.VerificationStatus)
	}
	if again := e.run("cleanup"); again.code != 0 || !strings.Contains(again.stdout, "Nothing to clean up") {
		t.Fatalf("cleanup not idempotent: %s", again.stdout)
	}
}

var bigOnce string

// bigDB creates (once) a database large enough that pg_restore runs for a
// few seconds, so it can be interrupted deterministically.
func bigDB(t *testing.T) string {
	t.Helper()
	if bigOnce != "" {
		return bigOnce
	}
	if err := psql(src, "postgres", "CREATE DATABASE big"); err != nil {
		t.Fatal(err)
	}
	if err := psql(src, "big", `CREATE TABLE events AS SELECT g AS id, md5(g::text) AS a, md5((g*7)::text) AS b,
		repeat('x', 200) AS pad FROM generate_series(1, 1500000) g;
		ALTER TABLE events ADD PRIMARY KEY (id); CREATE INDEX ON events (a); CREATE INDEX ON events (b);`); err != nil {
		t.Fatal(err)
	}
	bigOnce = "big"
	return bigOnce
}

// waitChild waits until the real restore child (pg_restore --single-transaction,
// not the earlier `pg_restore --list` integrity step) is running.
func waitChild(pid int, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir("/proc")
		for _, de := range entries {
			b, err := os.ReadFile(filepath.Join("/proc", de.Name(), "stat"))
			if err != nil {
				continue
			}
			s := string(b)
			open, close := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
			if open < 0 || close < 0 || s[open+1:close] != name {
				continue
			}
			fields := strings.Fields(s[close+1:])
			cmdline, _ := os.ReadFile(filepath.Join("/proc", de.Name(), "cmdline"))
			if len(fields) > 1 && fields[1] == strconv.Itoa(pid) && strings.Contains(string(cmdline), "--single-transaction") {
				time.Sleep(500 * time.Millisecond) // let it get into the data load
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// M0-09a: insufficient disk (pre-flight) → controlled FAIL.
func TestInsufficientDiskPreflight(t *testing.T) {
	e := newEnv(t)
	v, r := e.backup(e.config(cfgOpts{minFree: 1 << 60}))
	if r.code != 1 || v["error_category"] != "INSUFFICIENT_STORAGE" {
		t.Fatalf("code=%d v=%v stderr=%s", r.code, v, r.stderr)
	}
}

// M0-09b: disk actually fills up during the dump → controlled FAIL, partial
// file removed. Uses a tiny tmpfs: either a pre-mounted, writable one given in
// UNTH_TEST_TINY_FS (CI mounts it with sudo), or one mounted here (needs root).
func TestDiskFullDuringBackup(t *testing.T) {
	dir := os.Getenv("UNTH_TEST_TINY_FS")
	if dir == "" {
		dir = filepath.Join(t.TempDir(), "tiny")
		_ = os.MkdirAll(dir, 0o700)
		if out, err := exec.Command("mount", "-t", "tmpfs", "-o", "size=448k,mode=0700", "tmpfs", dir).CombinedOutput(); err != nil {
			t.Skipf("NOT RUN: cannot mount tmpfs (needs root/CAP_SYS_ADMIN or UNTH_TEST_TINY_FS): %v %s", err, out)
		}
		t.Cleanup(func() { _ = exec.Command("umount", dir).Run() })
	}
	e := newEnv(t)
	e.dataDir = filepath.Join(dir, "data-"+randHex(4))
	t.Cleanup(func() { _ = os.RemoveAll(e.dataDir) })
	v, r := e.backup(e.config(cfgOpts{}))
	if r.code != 1 || v["error_category"] != "INSUFFICIENT_STORAGE" {
		t.Fatalf("code=%d v=%v stderr=%s", r.code, v, r.stderr)
	}
	entries, _ := os.ReadDir(filepath.Join(e.dataDir, "tmp"))
	if len(entries) != 0 {
		t.Fatalf("partial files left behind: %v", entries)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dataDir, "backups")); len(entries) != 0 {
		t.Fatalf("backup dir left behind: %v", entries)
	}
}

// M0-10: isolated environment cannot be created → FAIL.
func TestEnvironmentCreationFailure(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{}))
	rep, r := e.verify(id, "--image", "unth-recover-test/does-not-exist:0", "--pull", "never")
	if r.code != 3 || rep.FinalStatus != "FAILED" || rep.ErrorCategory != "ENVIRONMENT_FAILED" {
		t.Fatalf("code=%d rep=%+v", r.code, rep)
	}
	if rep.step("restore") != "NOT_RUN" {
		t.Fatal("restore must not run")
	}
	e.assertNoResources(rep.RunID)
}

// M0-11: Docker not accessible → descriptive FAIL.
func TestDockerUnavailable(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{}))
	e.extra = []string{"DOCKER_HOST=unix:///nonexistent/docker.sock"}
	rep, r := e.verify(id)
	if r.code != 3 || rep.ErrorCategory != "DOCKER_UNAVAILABLE" || !strings.Contains(rep.ErrorMessage, "does not exist") {
		t.Fatalf("code=%d rep=%+v", r.code, rep)
	}
	d := e.run("discover", "docker")
	if d.code == 0 || !strings.Contains(d.stderr, "cannot connect to Docker Engine") {
		t.Fatalf("discover: %d %s", d.code, d.stderr)
	}
	doc := e.run("doctor")
	if doc.code != 1 || !strings.Contains(doc.stdout, "FAIL  docker") {
		t.Fatalf("doctor: %d %s", doc.code, doc.stdout)
	}
}

// Timeout → FAILED with TIMEOUT and cleanup.
func TestVerifyTimeout(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{}))
	rep, r := e.verify(id, "--timeout", "1s")
	if r.code != 3 || rep.FinalStatus != "FAILED" || rep.ErrorCategory != "TIMEOUT" {
		t.Fatalf("code=%d rep=%+v", r.code, rep)
	}
	e.assertNoResources(rep.RunID)
}

// M0-14a: pg_dump older than the server → controlled UNSUPPORTED_VERSION.
// Simulated with a shim that reports an older pg_dump version.
func TestUnsupportedVersionViaOlderTool(t *testing.T) {
	shimDir := t.TempDir()
	real, _ := exec.LookPath("pg_dump")
	shim := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'pg_dump (PostgreSQL) 11.22'; exit 0; fi\nexec %s \"$@\"\n", real)
	if err := os.WriteFile(filepath.Join(shimDir, "pg_dump"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	realRestore, _ := exec.LookPath("pg_restore")
	_ = os.Symlink(realRestore, filepath.Join(shimDir, "pg_restore"))
	e := newEnv(t)
	e.extra = []string{"UNTH_PG_BINDIR=" + shimDir}
	v, r := e.backup(e.config(cfgOpts{}))
	if r.code != 1 || v["error_category"] != "UNSUPPORTED_VERSION" {
		t.Fatalf("code=%d v=%v stderr=%s", r.code, v, r.stderr)
	}
	d := e.run("discover", "postgres", "--config", e.config(cfgOpts{}))
	if d.code != 1 || !strings.Contains(d.stdout, "cannot dump a newer server") {
		t.Fatalf("discover: %d %s %s", d.code, d.stdout, d.stderr)
	}
}

// M0-14b: a real newer PostgreSQL server (17) with pg_dump 16 → controlled
// failure. Skipped (NOT RUN) if the image cannot be pulled.
func TestUnsupportedVersionRealServer(t *testing.T) {
	tool, err := postgres.LookupTool(context.Background(), "pg_dump")
	if err != nil {
		t.Fatal(err)
	}
	img := envOr("UNTH_TEST_NEWER_IMAGE", fmt.Sprintf("postgres:%d-alpine", tool.Major+1))
	if !strings.Contains(img, strconv.Itoa(tool.Major+1)) {
		t.Skipf("NOT RUN: %s is not newer than local pg_dump %d", img, tool.Major)
	}
	if err := ensureImage(img); err != nil {
		t.Skipf("NOT RUN: cannot pull %s: %v", img, err)
	}
	newer, err := startPostgres(img, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	v, r := e.backup(e.config(cfgOpts{port: newer.Port, passFile: newer.PassFile}))
	if r.code != 1 || v["error_category"] != "UNSUPPORTED_VERSION" {
		t.Fatalf("code=%d v=%v stderr=%s", r.code, v, r.stderr)
	}
}

// Snapshot consistency: concurrent writes during backup do not break
// verification, because fingerprint and dump share one snapshot.
func TestConcurrentWritesDuringBackupStillVerify(t *testing.T) {
	if err := psql(src, "postgres", "CREATE DATABASE shop_live TEMPLATE shop"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			_ = psql(src, "shop_live", `INSERT INTO orders (customer_id, total_cents, placed_at) SELECT 1+g%2000, g, now() FROM generate_series(1,200) g;
				UPDATE customers SET full_name = full_name || '.' WHERE id % 13 = 0;`)
		}
	}()
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{db: "shop_live"}))
	cancel()
	<-done
	rep, r := e.verify(id)
	if r.code != 0 || rep.FinalStatus != "VERIFIED" {
		t.Fatalf("code=%d rep=%+v stderr=%s", r.code, rep, r.stderr)
	}
}

// Invalid configuration and CLI smoke tests against the real binary.
func TestCLISmoke(t *testing.T) {
	e := newEnv(t)
	if r := e.run("version"); r.code != 0 || !strings.HasPrefix(r.stdout, "unth ") {
		t.Fatalf("version: %+v", r)
	}
	if r := e.run("doctor"); r.code != 0 {
		t.Fatalf("doctor: %s", r.stdout)
	}
	if r := e.run("discover", "docker", "--format", "json"); r.code != 0 || !strings.Contains(r.stdout, src.Name) {
		t.Fatalf("discover docker: %d %s", r.code, r.stderr)
	}
	if r := e.run("discover", "postgres", "--config", e.config(cfgOpts{})); r.code != 0 || !strings.Contains(r.stdout, "supported") {
		t.Fatalf("discover postgres: %d %s %s", r.code, r.stdout, r.stderr)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	_ = os.WriteFile(bad, []byte("version: 1\nsource: {name: x}\nunknown: 1\n"), 0o600)
	if r := e.run("backup", "postgres", "--config", bad); r.code != 2 {
		t.Fatalf("invalid config exit %d", r.code)
	}
	if r := e.run("report", "bk-20260101t000000-00000000"); r.code != 2 {
		t.Fatalf("report unknown exit %d", r.code)
	}
	if r := e.run("backup", "list"); r.code != 0 {
		t.Fatal("list")
	}
}
