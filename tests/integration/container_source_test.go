//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestContainerSource: a job names only the PostgreSQL container; unth reads
// its address, database, user and password from Docker at run time, backs up
// and proves the backup by a restore -> VERIFIED. The password never appears
// in output, logs or stored records.
func TestContainerSource(t *testing.T) {
	e := newEnv(t)
	job := func(container string) string {
		p := filepath.Join(t.TempDir(), "job.yaml")
		y := fmt.Sprintf("version: 1\nsource:\n  name: it-container\n  container: %s\n  sslmode: disable\n"+
			"backup:\n  timeout: 10m\n  min_free_bytes: 1\nverification:\n  restore_image: %s\n  image_pull: if-missing\n  timeout: 10m\n  memory_mb: 512\n",
			container, restoreImage)
		if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r := e.run("run", "--config", job(src.Name), "--format", "json")
	var sum struct {
		Outcome            string `json:"outcome"`
		BackupID           string `json:"backup_id"`
		VerificationStatus string `json:"verification_status"`
	}
	if json.Unmarshal([]byte(r.stdout), &sum) != nil || r.code != 0 || sum.Outcome != "success" || sum.VerificationStatus != "VERIFIED" {
		t.Fatalf("run: %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	show := e.run("backup", "show", sum.BackupID, "--format", "json")
	if show.code != 0 || !strings.Contains(show.stdout, "it-container") || !strings.Contains(show.stdout, "VERIFIED") {
		t.Fatalf("backup record: %s", show.stdout)
	}
	// The password read from the container is never written anywhere.
	if strings.Contains(e.logs.String(), src.Password) {
		t.Fatal("password in unth output")
	}
	_ = filepath.Walk(e.dataDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && fi.Size() < 64<<20 {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), src.Password) { // #nosec G304 -- test data dir
				t.Errorf("password stored in %s", p)
			}
		}
		return nil
	})
	// A container that does not exist: clear category, nothing created.
	r = e.run("run", "--config", job("no-such-db-"+randHex(3)), "--format", "json")
	if r.code == 0 || !strings.Contains(r.stdout, "SOURCE_NOT_FOUND") {
		t.Fatalf("missing container: %d %s %s", r.code, r.stdout, r.stderr)
	}
}
