//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/unmadethings/unthrecover/internal/connections"
	"github.com/unmadethings/unthrecover/internal/fakegoogle"
)

type panel struct {
	base, token string
	c           *http.Client
	stderr      *strings.Builder
}

// startPanel runs `unth serve` on a free loopback port and logs in.
func startPanel(t *testing.T, e *env) *panel {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	p := &panel{base: "http://" + addr, token: "it-ui-token-" + randHex(12), stderr: &strings.Builder{}}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := e.command(ctx, "serve", "--listen", addr)
	cmd.Env = append(cmd.Env, "UNTH_UI_TOKEN="+p.token)
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
		cancel()
	})
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); !strings.Contains(line, p.base) {
		t.Fatalf("serve did not start: %q %s", line, p.stderr.String())
	}
	jar, _ := cookiejar.New(nil)
	p.c = &http.Client{Jar: jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if r := p.post(t, "/api/login", `{"token":"`+p.token+`"}`); r.StatusCode != 200 {
		t.Fatalf("login %d", r.StatusCode)
	}
	return p
}

func (p *panel) post(t *testing.T, path, body string) *http.Response {
	req, _ := http.NewRequest("POST", p.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Unth-CSRF", "1")
	r, err := p.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// connectDrive clicks "Connect Google Drive" through the API; the fake plays
// Google's consent page ("Allow").
func (p *panel) connectDrive(t *testing.T, fg *fakegoogle.Server) string {
	t.Helper()
	r := p.post(t, "/api/connections/gdrive", "{}")
	var v map[string]string
	_ = json.NewDecoder(r.Body).Decode(&v)
	if r.StatusCode != 200 {
		t.Fatalf("connect: %d %v", r.StatusCode, v)
	}
	loc, err := fg.Connect(v["auth_url"])
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(mustReq(loc)) // no session cookie, as from Google
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	final := resp.Request.URL.String()
	if !strings.Contains(final, "gdrive=connected") {
		t.Fatalf("callback ended at %s", final)
	}
	return final
}

func mustReq(u string) *http.Request {
	r, _ := http.NewRequest("GET", u, nil)
	return r
}

// TestGoogleDriveEndToEnd: connect Google Drive in the panel (OAuth with
// PKCE against a protocol-faithful fake of Google), `unth run` copies the
// verified backup to Drive encrypted and proves it, and a new machine
// connects the same Drive, fetches the copy and restores it → VERIFIED.
func TestGoogleDriveEndToEnd(t *testing.T) {
	fg := fakegoogle.New()
	defer fg.Close()
	googleEnv := []string{"UNTH_GOOGLE_TEST_ENDPOINT=" + fg.URL, "UNTH_GOOGLE_CLIENT_ID=" + fg.ClientID, "UNTH_GOOGLE_CLIENT_SECRET=" + fg.ClientSecret}
	e := newEnv(t)
	e.extra = googleEnv
	p := startPanel(t, e)
	p.connectDrive(t, fg)
	r, _ := p.c.Get(p.base + "/api/connections")
	body, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(body), `"name":"gdrive"`) || !strings.Contains(string(body), "user@example.com") {
		t.Fatalf("connections: %s", body)
	}

	keyDir := filepath.Join(t.TempDir(), "keys")
	if r := e.run("keys", "generate", "--dir", keyDir); r.code != 0 {
		t.Fatal(r.stderr)
	}
	cfg := e.config(cfgOpts{queries: true, extra: fmt.Sprintf(`encryption:
  identity_file: %s/age.key
signing:
  key_file: %s/sign.key
offsite:
  type: gdrive
  connection: gdrive
  prefix: servers/it
`, keyDir, keyDir)})
	res := e.run("run", "--config", cfg, "--format", "json")
	var sum struct {
		BackupID string `json:"backup_id"`
		Offsite  struct{ Status, Target string }
	}
	if json.Unmarshal([]byte(res.stdout), &sum) != nil || res.code != 0 || sum.Offsite.Status != "REMOTE_VERIFIED" ||
		sum.Offsite.Target != "gdrive:user@example.com" {
		t.Fatalf("run: %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	for _, f := range fg.FindByName("artifact.dump.age") {
		if strings.Contains(string(f.Data), "orders") {
			t.Fatal("plaintext on Drive")
		}
	}

	// New machine: empty data dir, same Drive account connected again.
	dr := newEnv(t)
	dr.extra = googleEnv
	startPanel(t, dr).connectDrive(t, fg)
	if n := len(fg.FindByName("Unth Recover Backups")); n != 1 {
		t.Fatalf("backup folders on Drive: %d", n)
	}
	if r := dr.run("offsite", "fetch", "--config", cfg, sum.BackupID); r.code != 0 || !strings.Contains(r.stdout, "signature: valid") {
		t.Fatalf("fetch: %d %s %s", r.code, r.stdout, r.stderr)
	}
	rep, vr := dr.verify(sum.BackupID)
	if vr.code != 0 || rep.FinalStatus != "VERIFIED" {
		t.Fatalf("restore test of the copy from Drive: %d %s", vr.code, vr.stderr)
	}

	conn, err := connections.NewStore(e.dataDir).Load("gdrive")
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{e.logs.String(), dr.logs.String(), p.stderr.String()} {
		if strings.Contains(out, conn.RefreshToken) {
			t.Fatal("refresh token in output")
		}
	}
	e.assertNoResources("")

	if os.Getenv("UNTH_TEST_BROWSER") != "1" {
		t.Log("browser part NOT RUN (set UNTH_TEST_BROWSER=1)")
		return
	}
	out, err := exec.Command("node", filepath.Join(repoRoot, "tests", "e2e", "gdrive.mjs"), p.base, p.token).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "GDRIVE E2E: PASS") {
		t.Fatalf("browser: %v\n%s", err, out)
	}
}
