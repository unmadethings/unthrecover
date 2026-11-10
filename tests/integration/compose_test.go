//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestComposeDeployment deploys deployments/compose/compose.yaml exactly as a
// user would (jobs folder + .env), against the shared source database on an
// external network: the scheduler must back up and verify, the panel must
// require login and show the verified workload, and `down -v` must leave no
// sandbox resources behind.
func TestComposeDeployment(t *testing.T) {
	image := os.Getenv("UNTH_TEST_IMAGE")
	if image == "" {
		t.Skip("NOT RUN: set UNTH_TEST_IMAGE (make dist && make image)")
	}
	if exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("NOT RUN: docker compose plugin not installed")
	}
	netName := "unth-it-app-" + randHex(3)
	if out, err := exec.Command("docker", "network", "create", "--label", "io.unthrecover.test="+sessionLabel, netName).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", netName).Run() })
	if out, err := exec.Command("docker", "network", "connect", "--alias", "shopdb", netName, src.Name).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "disconnect", "-f", netName, src.Name).Run() })

	dir := t.TempDir()
	_ = os.Chmod(dir, 0o755)
	compose, err := os.ReadFile(filepath.Join(repoRoot, "deployments", "compose", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Same file, with the documented "add that network" step applied.
	text := strings.Replace(string(compose), "    # networks: [default, myapp_default]", "    networks: [default, appnet]", 1)
	text = strings.Replace(text, "# networks:\n#   myapp_default:\n#     external: true", "networks:\n  appnet:\n    name: "+netName+"\n    external: true", 1)
	if text == string(compose) {
		t.Fatal("compose.yaml no longer has the documented network placeholders")
	}
	write := func(name, content string, mode os.FileMode) {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("compose.yaml", text, 0o644)
	write("jobs/shop.yaml", fmt.Sprintf(`version: 1
source:
  name: compose-shop
  host: shopdb
  port: 5432
  database: shop
  user: postgres
  password_env: SHOP_DB_PASSWORD
  sslmode: disable
verification:
  restore_image: %s
retention:
  keep_last: 3
`, restoreImage), 0o644)
	token := randHex(24)
	gid, _ := exec.Command("stat", "-c", "%g", "/var/run/docker.sock").Output()
	port := fmt.Sprintf("%d", 18500+time.Now().Nanosecond()%400)
	// .env is 0644 here only because the compose CLI and the container user
	// differ in this test; users are told to chmod 600.
	write(".env", "UNTH_UI_TOKEN="+token+"\nDOCKER_GID="+strings.TrimSpace(string(gid))+"\nSHOP_DB_PASSWORD="+src.Password+"\n", 0o644)

	project := "unth-it-compose-" + randHex(3)
	dc := func(args ...string) (string, error) {
		cmd := exec.Command("docker", append([]string{"compose", "-p", project}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "UNTH_IMAGE="+image, "UNTH_PANEL_PORT="+port,
			"UNTH_SCHEDULE=--every 1h --run-at-start")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := dc("config", "--quiet"); err != nil {
		t.Fatalf("compose config: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = dc("down", "-v", "--remove-orphans") })
	if out, err := dc("up", "-d"); err != nil {
		t.Fatalf("compose up: %v\n%s", err, out)
	}

	// The scheduler runs the job at start; wait for its result line.
	var logs string
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		logs, _ = dc("logs", "--no-color", "scheduler")
		if strings.Contains(logs, "compose-shop: ") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !strings.Contains(logs, "compose-shop: SUCCESS · verification VERIFIED") {
		t.Fatalf("scheduler did not report a verified run:\n%s", logs)
	}
	if !strings.Contains(logs, "Next run: ") {
		t.Fatalf("scheduler did not schedule the next run:\n%s", logs)
	}

	base := "http://127.0.0.1:" + port
	var resp *http.Response
	for i := 0; i < 60; i++ {
		if resp, err = http.Get(base + "/api/overview"); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("panel not reachable: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("overview without login: %d", resp.StatusCode)
	}
	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar}
	resp, err = cl.Post(base+"/api/login", "application/json", strings.NewReader(`{"token":"`+token+`"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("login: %v %v", err, resp)
	}
	resp.Body.Close()
	resp, err = cl.Get(base + "/api/overview")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var ov struct {
		Protected int     `json:"protected_workloads"`
		Last      *string `json:"last_verified_backup_at"`
	}
	if json.Unmarshal(body, &ov) != nil || ov.Protected < 1 || ov.Last == nil {
		t.Fatalf("overview: %s", body)
	}

	all, _ := dc("logs", "--no-color")
	for _, secret := range []string{token, src.Password} {
		if strings.Contains(all, secret) {
			t.Fatal("secret in container logs")
		}
	}
	if out, err := dc("down", "-v", "--remove-orphans"); err != nil {
		t.Fatalf("compose down: %v\n%s", err, out)
	}
	if left := managedResources(""); len(left) > 0 {
		t.Fatalf("leftover sandbox resources: %v", left)
	}
}
