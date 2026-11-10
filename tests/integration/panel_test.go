//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWebPanelEndToEnd starts the real `unth serve`, logs in through the API,
// starts a verification with "Verify now" (real isolated restore) and waits
// for VERIFIED. With UNTH_TEST_BROWSER=1 it also drives the UI in Chromium.
func TestWebPanelEndToEnd(t *testing.T) {
	e := newEnv(t)
	id := e.mustBackup(e.config(cfgOpts{}))

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	token := "it-ui-token-" + randHex(12)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := e.command(ctx, "serve", "--listen", addr)
	cmd.Env = append(cmd.Env, "UNTH_UI_TOKEN="+token)
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
		if strings.Contains(stderr.String(), token) {
			t.Error("UI token leaked into server logs")
		}
	}()
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if !strings.Contains(line, "http://"+addr) {
		t.Fatalf("serve did not start: %q %s", line, stderr.String())
	}
	base := "http://" + addr

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	post := func(path, body string) *http.Response {
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Unth-CSRF", "1")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if r, _ := c.Get(base + "/api/overview"); r.StatusCode != 401 {
		t.Fatalf("unauthenticated overview: %d", r.StatusCode)
	}
	if r := post("/api/login", `{"token":"`+token+`"}`); r.StatusCode != 200 {
		t.Fatalf("login: %d", r.StatusCode)
	}
	if r := post("/api/backups/"+id+"/verify", "{}"); r.StatusCode != 202 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("verify: %d %s", r.StatusCode, b)
	}
	// A second click while running is refused.
	if r := post("/api/backups/"+id+"/verify", "{}"); r.StatusCode != 409 {
		t.Errorf("concurrent verify: %d", r.StatusCode)
	}
	deadline := time.Now().Add(3 * time.Minute)
	var det struct {
		Backup struct {
			VerificationStatus string `json:"verification_status"`
			Verifying          bool   `json:"verifying"`
		} `json:"backup"`
		Runs []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"runs"`
	}
	for time.Now().Before(deadline) {
		r, err := c.Get(base + "/api/backups/" + id)
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewDecoder(r.Body).Decode(&det)
		r.Body.Close()
		if !det.Backup.Verifying && det.Backup.VerificationStatus != "RUNNING" && len(det.Runs) > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if det.Backup.VerificationStatus != "VERIFIED" || len(det.Runs) != 1 {
		t.Fatalf("UI verification result: %+v", det)
	}
	e.assertNoResources(det.Runs[0].ID)
	var ov map[string]any
	r, _ := c.Get(base + "/api/overview")
	_ = json.NewDecoder(r.Body).Decode(&ov)
	if ov["last_verified_backup_at"] == nil {
		t.Fatalf("overview: %v", ov)
	}
	if !strings.Contains(e.run("audit", "list").stdout, "ui.verify") {
		t.Error("UI verification not audited")
	}

	if os.Getenv("UNTH_TEST_BROWSER") != "1" {
		t.Log("browser part NOT RUN (set UNTH_TEST_BROWSER=1)")
		return
	}
	shots := os.Getenv("UNTH_TEST_SCREENSHOTS")
	out, err := exec.Command("node", filepath.Join(repoRoot, "tests", "e2e", "panel.mjs"), base, token, id, shots).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "PANEL E2E: PASS") {
		t.Fatalf("browser e2e: %v\n%s", err, out)
	}
}

func TestServeRefusesUnsafeConfig(t *testing.T) {
	e := newEnv(t)
	if r := e.run("serve"); r.code != 2 || !strings.Contains(r.stderr, "no UI token") {
		t.Fatalf("no token: %d %s", r.code, r.stderr)
	}
	e.extra = []string{"UNTH_UI_TOKEN=short"}
	if r := e.run("serve"); r.code != 2 {
		t.Fatalf("short token: %d", r.code)
	}
	e.extra = []string{"UNTH_UI_TOKEN=" + strings.Repeat("a", 32)}
	if r := e.run("serve", "--listen", "0.0.0.0:0"); r.code != 2 || !strings.Contains(r.stderr, "refusing to serve plain HTTP") {
		t.Fatalf("plain http on all interfaces: %d %s", r.code, r.stderr)
	}
}
