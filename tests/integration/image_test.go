//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestContainerImageRunsJob runs the published-style container image
// (UNTH_TEST_IMAGE) next to a database on a user-defined network, the way it
// is deployed with Compose/Coolify/Dokploy: `unth run` inside the container
// backs up, verifies (joining its own sandbox network) and cleans up.
func TestContainerImageRunsJob(t *testing.T) {
	image := os.Getenv("UNTH_TEST_IMAGE")
	if image == "" {
		t.Skip("NOT RUN: set UNTH_TEST_IMAGE (make dist && make image)")
	}
	netName := "unth-it-app-" + randHex(3)
	if out, err := exec.Command("docker", "network", "create", "--label", "io.unthrecover.test="+sessionLabel, netName).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", netName).Run() })
	// attach the shared source DB to the app network under an alias
	if out, err := exec.Command("docker", "network", "connect", "--alias", "shopdb", netName, src.Name).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "disconnect", "-f", netName, src.Name).Run() })

	dir := t.TempDir()
	_ = os.Chmod(dir, 0o755)
	pass := filepath.Join(dir, "db.pass")
	_ = os.WriteFile(pass, []byte(src.Password), 0o600)
	// Hand the 0600 secret to the image's unth user (uid 10001) without
	// needing root on the host: chown it from a throw-away container.
	if out, err := exec.Command("docker", "run", "--rm", "--label", "io.unthrecover.test="+sessionLabel, "--network", "none",
		"-u", "0", "--entrypoint", "chown", "-v", dir+":/d", image, "10001:10001", "/d/db.pass").CombinedOutput(); err != nil {
		t.Fatalf("chown secret: %v %s", err, out)
	}
	vol := testVolume(t)
	job := fmt.Sprintf(`version: 1
source:
  name: img-shop
  host: shopdb
  port: 5432
  database: shop
  user: postgres
  password_file: /etc/unth/db.pass
  sslmode: disable
verification:
  restore_image: %s
retention:
  keep_last: 1
`, restoreImage)
	volJob := fmt.Sprintf(`version: 1
source:
  name: img-files
  type: docker-volume
  volume: %s
  helper_image: %s
`, vol, helperImage())
	_ = os.WriteFile(filepath.Join(dir, "job.yaml"), []byte(job), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "vol.yaml"), []byte(volJob), 0o644)
	dataVol := "unth-it-data-" + randHex(3)
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", dataVol).Run() })

	gid, _ := exec.Command("stat", "-c", "%g", "/var/run/docker.sock").Output()
	run := func(args ...string) (string, error) {
		base := []string{"run", "--rm", "--label", "io.unthrecover.test=" + sessionLabel, "--network", netName,
			"-v", "/var/run/docker.sock:/var/run/docker.sock", "--group-add", strings.TrimSpace(string(gid)),
			"-v", dataVol + ":/var/lib/unthrecover", "-v", dir + ":/etc/unth:ro", image}
		out, err := exec.Command("docker", append(base, args...)...).CombinedOutput()
		return string(out), err
	}
	if out, err := run("doctor"); err != nil || !strings.Contains(out, "PASS  docker") || !strings.Contains(out, "PASS  pg_dump") {
		t.Fatalf("doctor in container: %v\n%s", err, out)
	}
	for i := 0; i < 2; i++ {
		out, err := run("run", "--config", "/etc/unth/job.yaml")
		if err != nil || !strings.Contains(out, "Result: SUCCESS") || !strings.Contains(out, "VERIFIED") {
			t.Fatalf("unth run in container (%d): %v\n%s", i, err, out)
		}
	}
	out, err := run("backup", "list")
	if err != nil || strings.Count(out, "BACKUP_CREATED") != 1 || strings.Count(out, "EXPIRED") != 1 {
		t.Fatalf("retention in container: %v\n%s", err, out)
	}
	if out, err := run("run", "--config", "/etc/unth/vol.yaml"); err != nil || !strings.Contains(out, "Result: SUCCESS") {
		t.Fatalf("volume job in container: %v\n%s", err, out)
	}
	if left := managedResources(""); len(left) > 0 {
		t.Fatalf("leftover sandbox resources: %v", left)
	}
	if strings.Contains(out, src.Password) {
		t.Fatal("password leaked")
	}
}
