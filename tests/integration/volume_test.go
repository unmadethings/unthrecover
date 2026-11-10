//go:build integration

package integration

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unmadethings/unthrecover/internal/manifest"
	"github.com/unmadethings/unthrecover/internal/metadata"
)

func helperImage() string { return envOr("UNTH_TEST_HELPER_IMAGE", sourceImage) }

// testVolume creates a labeled volume populated with a variety of objects.
func testVolume(t *testing.T) string {
	t.Helper()
	name := "unth-it-vol-" + randHex(4)
	if out, err := exec.Command("docker", "volume", "create", "--label", "io.unthrecover.test="+sessionLabel, name).CombinedOutput(); err != nil {
		t.Fatalf("create volume: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", name).Run() })
	script := `set -e; cd /data
mkdir -p uploads/2026/11 empty private
printf 'hello world\n' > uploads/a.txt
head -c 300000 /dev/urandom > uploads/2026/11/blob.bin
printf 'secret' > private/key && chmod 600 private/key && chown 1234:5678 private/key
chmod 700 private && chmod 1777 empty
ln -s a.txt uploads/link-to-a
ln -s ../../nonexistent dangling`
	dockerRun(t, name, script)
	return name
}

func dockerRun(t *testing.T, vol, script string) {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm", "--label", "io.unthrecover.test="+sessionLabel, "--network", "none",
		"--entrypoint", "sh", "-v", vol+":/data", helperImage(), "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v %s", err, out)
	}
}

// runningWriter starts a labeled container that mounts the volume.
func runningWriter(t *testing.T, vol string) string {
	t.Helper()
	out, err := exec.Command("docker", "run", "-d", "--label", "io.unthrecover.test="+sessionLabel, "--network", "none",
		"--entrypoint", "sh", "-v", vol+":/data", helperImage(), "-c", "sleep 600").CombinedOutput()
	if err != nil {
		t.Fatalf("start writer: %v %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	return id
}

func containerState(t *testing.T, id string) string {
	out, _ := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", id).Output()
	return strings.TrimSpace(string(out))
}

func (e *env) volumeConfig(vol, consistency string) string {
	e.t.Helper()
	y := fmt.Sprintf(`version: 1
source:
  name: it-files
  type: docker-volume
  volume: %s
  consistency: %s
  helper_image: %s
  stop_timeout: 5s
backup:
  min_free_bytes: 1
verification:
  image_pull: if-missing
  timeout: 10m
`, vol, consistency, helperImage())
	p := filepath.Join(e.t.TempDir(), "vol.yaml")
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) volumeBackup(cfg string) (map[string]any, result) {
	r := e.run("backup", "volume", "--config", cfg, "--format", "json")
	var v map[string]any
	_ = json.Unmarshal([]byte(r.stdout), &v)
	return v, r
}

type volReport struct {
	report
	FileComparison *struct {
		EntriesExpected int   `json:"entries_expected"`
		EntriesActual   int   `json:"entries_actual"`
		BytesActual     int64 `json:"bytes_actual"`
		MismatchCount   int   `json:"mismatch_count"`
	} `json:"file_comparison"`
}

func (e *env) verifyVol(id string) (*volReport, result) {
	r := e.run("verify", id, "--format", "json")
	var rep volReport
	if json.Unmarshal([]byte(r.stdout), &rep) != nil {
		return nil, r
	}
	return &rep, r
}

func TestVolumeBackupIsVerified(t *testing.T) {
	vol := testVolume(t)
	e := newEnv(t)
	v, r := e.volumeBackup(e.volumeConfig(vol, "offline"))
	if r.code != 0 || v["status"] != "BACKUP_CREATED" {
		t.Fatalf("backup: %d %v %s", r.code, v, r.stderr)
	}
	id := v["id"].(string)
	rep, r := e.verifyVol(id)
	if r.code != 0 || rep.FinalStatus != "VERIFIED" {
		t.Fatalf("verify: %d %+v %s", r.code, rep, r.stderr)
	}
	for _, s := range []string{"artifact_accessible", "manifest_valid", "checksum_sha256", "archive_readable", "isolated_environment", "restore", "file_integrity"} {
		if rep.step(s) != "PASS" {
			t.Errorf("step %s = %s", s, rep.step(s))
		}
	}
	// root + uploads, 2026, 11, empty, private + 4 files... count independently with find.
	out, _ := exec.Command("docker", "run", "--rm", "--network", "none", "--entrypoint", "sh", "-v", vol+":/data:ro", helperImage(),
		"-c", "cd /data && find . | wc -l").Output()
	want := strings.TrimSpace(string(out))
	if fmt.Sprint(rep.FileComparison.EntriesActual) != want || rep.FileComparison.MismatchCount != 0 || rep.FileComparison.BytesActual < 300000 {
		t.Fatalf("file comparison %+v, find counted %s", rep.FileComparison, want)
	}
	e.assertNoResources(rep.RunID)
	e.assertNoResources(id) // snapshot helper removed
	if t := e.run("report", id); !strings.Contains(t.stdout, "File checks:") {
		e.t.Fatalf("text report: %s", t.stdout)
	}
}

func TestVolumeOfflineRefusesRunningWriter(t *testing.T) {
	vol := testVolume(t)
	w := runningWriter(t, vol)
	e := newEnv(t)
	v, r := e.volumeBackup(e.volumeConfig(vol, "offline"))
	if r.code != 1 || v["error_category"] != "CONSISTENCY_UNSAFE" || !strings.Contains(r.stderr, "consistency: stop") {
		t.Fatalf("code=%d v=%v stderr=%s", r.code, v, r.stderr)
	}
	if containerState(t, w) != "running" {
		t.Fatal("offline policy must not touch the running container")
	}
}

func TestVolumeStopPolicyStopsAndRestartsWriters(t *testing.T) {
	vol := testVolume(t)
	w := runningWriter(t, vol)
	// A stopped container that also mounts the volume must stay stopped.
	out, err := exec.Command("docker", "create", "--label", "io.unthrecover.test="+sessionLabel, "--network", "none",
		"--entrypoint", "sh", "-v", vol+":/data", helperImage(), "-c", "sleep 600").CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	idle := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", idle).Run() })

	e := newEnv(t)
	v, r := e.volumeBackup(e.volumeConfig(vol, "stop"))
	if r.code != 0 || v["status"] != "BACKUP_CREATED" {
		t.Fatalf("backup: %d %v %s", r.code, v, r.stderr)
	}
	if containerState(t, w) != "running" {
		t.Fatalf("writer not restarted: %s", containerState(t, w))
	}
	if containerState(t, idle) != "created" {
		t.Fatalf("idle container was started: %s", containerState(t, idle))
	}
	audit := e.run("audit", "list").stdout
	if !strings.Contains(audit, "backup.stop_container") || !strings.Contains(audit, "backup.restart_container") {
		t.Fatalf("stop/restart not audited:\n%s", audit)
	}
	if rep, r := e.verifyVol(v["id"].(string)); r.code != 0 || rep.FinalStatus != "VERIFIED" {
		t.Fatalf("verify: %d %+v", r.code, rep)
	}
}

// rewriteVolumeChecksums makes metadata + manifest checksums match a tampered artifact.
func rewriteVolumeChecksums(t *testing.T, e *env, id string) {
	t.Helper()
	art := filepath.Join(e.dataDir, "backups", id, manifest.VolumeArtifactName)
	sum, n, err := manifest.HashFile(art)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := metadata.Open(ctx, e.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bk, _ := s.GetBackup(ctx, id)
	bk.SHA256, bk.SizeBytes = sum, n
	if err := s.UpdateBackup(ctx, bk); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(e.dataDir, "backups", id, manifest.FileName)
	m, _ := manifest.Read(mp)
	m.Artifact.SHA256, m.Artifact.SizeBytes = sum, n
	_ = os.Remove(mp)
	if err := manifest.Write(mp, m); err != nil {
		t.Fatal(err)
	}
}

func TestVolumeTamperedArchiveIsNeverVerified(t *testing.T) {
	vol := testVolume(t)
	e := newEnv(t)
	v, r := e.volumeBackup(e.volumeConfig(vol, "offline"))
	if r.code != 0 {
		t.Fatal(r.stderr)
	}
	id := v["id"].(string)
	art := filepath.Join(e.dataDir, "backups", id, manifest.VolumeArtifactName)

	// 1. Content changed inside a valid tar, checksums rewritten to match.
	orig, _ := os.ReadFile(art)
	var buf bytes.Buffer
	tr, tw := tar.NewReader(bytes.NewReader(orig)), tar.NewWriter(&buf)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(tr)
		if h.Name == "data/uploads/a.txt" {
			body = bytes.NewBufferString("HELLO WORLD\n")
			h.Size = int64(body.Len())
		}
		_ = tw.WriteHeader(h)
		_, _ = tw.Write(body.Bytes())
	}
	_ = tw.Close()
	_ = os.WriteFile(art, buf.Bytes(), 0o600)
	rewriteVolumeChecksums(t, e, id)
	rep, r := e.verifyVol(id)
	if r.code != 3 || rep.FinalStatus == "VERIFIED" || rep.ErrorCategory != "ARTIFACT_INVALID" || rep.step("checksum_sha256") != "PASS" {
		t.Fatalf("modified content: code=%d %+v", r.code, rep)
	}

	// 2. Malicious path traversal entry, checksums rewritten: rejected before Docker sees it.
	buf.Reset()
	tw = tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "data/../../etc/evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
	_, _ = tw.Write([]byte("evil"))
	_ = tw.Close()
	_ = os.WriteFile(art, buf.Bytes(), 0o600)
	rewriteVolumeChecksums(t, e, id)
	rep, r = e.verifyVol(id)
	if r.code != 3 || rep.ErrorCategory != "ARTIFACT_INVALID" || rep.step("restore") != "NOT_RUN" {
		t.Fatalf("traversal: code=%d %+v", r.code, rep)
	}
	e.assertNoResources(rep.RunID)
}

func TestVolumeRunJobAndMissingVolume(t *testing.T) {
	vol := testVolume(t)
	e := newEnv(t)
	cfg := e.volumeConfig(vol, "offline")
	if s, r := e.runJob(cfg); r.code != 0 || s.VerificationStatus != "VERIFIED" {
		t.Fatalf("run: %d %+v %s", r.code, s, r.stderr)
	}
	missing := e.volumeConfig("unth-it-no-such-volume", "offline")
	if v, r := e.volumeBackup(missing); r.code != 1 || v["error_category"] != "SOURCE_NOT_FOUND" {
		t.Fatalf("missing volume: %d %v", r.code, v)
	}
	if r := e.run("backup", "postgres", "--config", cfg); r.code != 2 {
		t.Fatalf("type mismatch must be a usage error: %d", r.code)
	}
}
