//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDiscoverJobsCoolify: a database created the way Coolify creates it
// (labels, own network) is found by `unth discover jobs`; the generated job
// file, used unchanged by the unth container on that network, backs up and
// verifies the database. The password is never read or printed by discovery.
func TestDiscoverJobsCoolify(t *testing.T) {
	image := os.Getenv("UNTH_TEST_IMAGE")
	if image == "" {
		t.Skip("NOT RUN: set UNTH_TEST_IMAGE (make dist && make image)")
	}
	netName := "unth-it-coolify-" + randHex(3)
	if out, err := exec.Command("docker", "network", "create", "--label", "io.unthrecover.test="+sessionLabel, netName).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", netName).Run() })
	dbName := "unth-it-cooldb-" + randHex(3)
	password := "coolify-" + randHex(12)
	out, err := exec.Command("docker", "run", "-d", "--name", dbName, "--network", netName,
		"--label", "io.unthrecover.test="+sessionLabel, "--label", "coolify.managed=true",
		"--label", "coolify.type=database", "--label", "coolify.name=it-coolify-"+randHex(2),
		"-e", "POSTGRES_DB=shop", "-e", "POSTGRES_USER=shopuser", "-e", "POSTGRES_PASSWORD="+password,
		"--memory", "512m", sourceImage).CombinedOutput()
	if err != nil {
		t.Fatalf("start db: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", dbName).Run() })
	for i := 0; i < 60; i++ {
		if exec.Command("docker", "exec", dbName, "pg_isready", "-U", "shopuser", "-d", "shop").Run() == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if out, err := exec.Command("docker", "exec", dbName, "psql", "-U", "shopuser", "-d", "shop", "-c",
		"CREATE TABLE items(id int primary key, name text); INSERT INTO items SELECT g, 'item '||g FROM generate_series(1,500) g;").CombinedOutput(); err != nil {
		t.Fatalf("seed: %v %s", err, out)
	}

	e := newEnv(t)
	r := e.run("discover", "jobs", "--format", "json")
	if r.code != 0 {
		t.Fatalf("discover jobs: %d %s", r.code, r.stderr)
	}
	var js []struct {
		Container, Platform, Workload, Host, Database, User, Network string
		PasswordEnv                                                  string `json:"password_env"`
		RestoreImage                                                 string `json:"restore_image"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &js); err != nil {
		t.Fatal(err)
	}
	var w string
	var pwEnv string
	found := false
	for _, j := range js {
		if j.Container == dbName {
			found = true
			if j.Platform != "coolify" || j.Host != dbName || j.Database != "shop" || j.User != "shopuser" ||
				j.Network != netName || j.RestoreImage != sourceImage {
				t.Fatalf("suggestion %+v", j)
			}
			w, pwEnv = j.Workload, j.PasswordEnv
		}
	}
	if !found {
		t.Fatalf("container not discovered: %s", r.stdout)
	}
	jobsDir := filepath.Join(t.TempDir(), "jobs")
	if r := e.run("discover", "jobs", "--write-dir", jobsDir); r.code != 0 || !strings.Contains(r.stdout, "wrote") {
		t.Fatalf("write: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if strings.Contains(e.logs.String(), password) {
		t.Fatal("discovery printed the database password")
	}
	_ = os.Chmod(filepath.Dir(jobsDir), 0o755)
	_ = os.Chmod(jobsDir, 0o755)
	_ = filepath.Walk(jobsDir, func(p string, _ os.FileInfo, _ error) error { return os.Chmod(p, 0o755) })

	gid, _ := exec.Command("stat", "-c", "%g", "/var/run/docker.sock").Output()
	dataVol := "unth-it-data-" + randHex(3)
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", dataVol).Run() })
	run := exec.Command("docker", "run", "--rm", "--label", "io.unthrecover.test="+sessionLabel, "--network", netName,
		"-v", "/var/run/docker.sock:/var/run/docker.sock", "--group-add", strings.TrimSpace(string(gid)),
		"-v", dataVol+":/var/lib/unthrecover", "-v", jobsDir+":/etc/unth/jobs:ro", "-e", pwEnv+"="+password,
		image, "run", "--config", "/etc/unth/jobs/"+w+".yaml")
	res, err := run.CombinedOutput()
	if err != nil || !strings.Contains(string(res), "Result: SUCCESS") || !strings.Contains(string(res), "VERIFIED") {
		t.Fatalf("generated job did not verify: %v\n%s", err, res)
	}
	if strings.Contains(string(res), password) {
		t.Fatal("password in unth output")
	}
	if left := managedResources(""); len(left) > 0 {
		t.Fatalf("leftover sandbox resources: %v", left)
	}
}
