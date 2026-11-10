//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/unmadethings/unthrecover/internal/notify"
)

type hookCall struct {
	path, sig, ts string
	body          []byte
}

// hookServer is a local webhook receiver (loopback http is allowed by unth).
type hookServer struct {
	mu    sync.Mutex
	calls []hookCall
	url   string
	srv   *http.Server
}

func newHookServer(t *testing.T) *hookServer {
	h := &hookServer{}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.url = "http://" + l.Addr().String()
	h.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.calls = append(h.calls, hookCall{r.URL.Path, r.Header.Get("X-Unth-Signature"), r.Header.Get("X-Unth-Timestamp"), b})
		h.mu.Unlock()
		w.WriteHeader(204)
	})}
	go func() { _ = h.srv.Serve(l) }()
	t.Cleanup(func() { _ = h.srv.Close() })
	return h
}

func (h *hookServer) byPath(p string) []hookCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []hookCall
	for _, c := range h.calls {
		if c.path == p {
			out = append(out, c)
		}
	}
	return out
}

const notifyYAML = `retention:
  keep_last: 2
notifications:
  - name: ops
    type: webhook
    url_env: UNTH_IT_HOOK
    secret_env: UNTH_IT_HOOK_SECRET
    on: always
  - name: chat
    type: slack
    url_env: UNTH_IT_SLACK
    on: failure
`

type runSummary struct {
	Outcome            string `json:"outcome"`
	Stage              string `json:"failed_stage"`
	BackupID           string `json:"backup_id"`
	VerificationStatus string `json:"verification_status"`
	ErrorCategory      string `json:"error_category"`
	PruneSkipped       string `json:"prune_skipped"`
	Prune              *struct {
		Expired []string `json:"expired"`
	} `json:"prune"`
	Notifications []struct {
		Name, Error, Skipped string
		Sent                 bool
	} `json:"notifications"`
}

func (e *env) runJob(cfg string) (runSummary, result) {
	e.t.Helper()
	r := e.run("run", "--config", cfg, "--format", "json")
	var s runSummary
	_ = json.Unmarshal([]byte(r.stdout), &s)
	return s, r
}

func TestRunJobVerifiesPrunesAndNotifies(t *testing.T) {
	h := newHookServer(t)
	e := newEnv(t)
	const secret = "it-hook-secret-0123456789"
	e.extra = []string{"UNTH_IT_HOOK=" + h.url + "/ops?token=hooktoken-abc", "UNTH_IT_HOOK_SECRET=" + secret, "UNTH_IT_SLACK=" + h.url + "/slack"}
	cfg := e.config(cfgOpts{extra: notifyYAML})

	var ids []string
	for i := 0; i < 3; i++ {
		s, r := e.runJob(cfg)
		if r.code != 0 || s.Outcome != "success" || s.VerificationStatus != "VERIFIED" {
			t.Fatalf("run %d: code=%d %+v stderr=%s", i, r.code, s, r.stderr)
		}
		ids = append(ids, s.BackupID)
		if i == 2 && (s.Prune == nil || len(s.Prune.Expired) != 1 || s.Prune.Expired[0] != ids[0]) {
			t.Fatalf("third run should expire the oldest backup: %+v", s.Prune)
		}
	}
	// Webhook: 3 signed events; Slack (on=failure): none.
	ops := h.byPath("/ops")
	if len(ops) != 3 || len(h.byPath("/slack")) != 0 {
		t.Fatalf("ops=%d slack=%d", len(ops), len(h.byPath("/slack")))
	}
	for _, c := range ops {
		if c.sig != "sha256="+notify.Sign(secret, c.ts, c.body) {
			t.Fatalf("bad signature on %s", c.body)
		}
		var ev notify.Event
		if json.Unmarshal(c.body, &ev) != nil || ev.Outcome != "success" || ev.VerificationStatus != "VERIFIED" {
			t.Fatalf("event %s", c.body)
		}
	}
	// Expired backup: directory gone, status EXPIRED; cannot be verified.
	if _, err := os.Stat(filepath.Join(e.dataDir, "backups", ids[0])); !os.IsNotExist(err) {
		t.Fatal("expired backup directory still present")
	}
	if !strings.Contains(e.run("backup", "list").stdout, "EXPIRED") {
		t.Fatal("EXPIRED status not listed")
	}
	if _, r := e.verify(ids[0]); r.code != 2 {
		t.Fatalf("verifying an expired backup must be refused, got %d", r.code)
	}
	// prune --dry-run: nothing more to expire with keep_last 2.
	if r := e.run("prune", "--config", cfg, "--dry-run"); r.code != 0 || !strings.Contains(r.stdout, "would expire 0 backup(s)") {
		t.Fatalf("prune dry-run: %d %s %s", r.code, r.stdout, r.stderr)
	}
	// Secrets (webhook URL token, HMAC key, DB password) never leak.
	assertNoSecret(t, e, "hooktoken-abc")
	assertNoSecret(t, e, secret)
	assertNoSecret(t, e, src.Password)
}

func TestRunNotVerifiedSkipsPruneAndNotifiesFailure(t *testing.T) {
	h := newHookServer(t)
	e := newEnv(t)
	e.extra = []string{"UNTH_IT_HOOK=" + h.url + "/ops", "UNTH_IT_HOOK_SECRET=s3cret-value", "UNTH_IT_SLACK=" + h.url + "/slack"}
	good := e.config(cfgOpts{extra: notifyYAML})
	for i := 0; i < 2; i++ {
		if _, r := e.runJob(good); r.code != 0 {
			t.Fatalf("seed run %d failed: %s", i, r.stderr)
		}
	}
	bad := e.config(cfgOpts{extra: notifyYAML, image: "unth-recover-test/does-not-exist:0", pull: "never"})
	s, r := e.runJob(bad)
	if r.code != 3 || s.Outcome != "failure" || s.Stage != "verify" || s.ErrorCategory != "ENVIRONMENT_FAILED" {
		t.Fatalf("code=%d %+v", r.code, s)
	}
	if s.Prune != nil || !strings.Contains(s.PruneSkipped, "verification did not succeed") {
		t.Fatalf("prune must be skipped after a failed verification: %+v", s)
	}
	slack := h.byPath("/slack")
	if len(slack) != 1 || !strings.Contains(string(slack[0].body), "ENVIRONMENT_FAILED") {
		t.Fatalf("slack failure notification missing: %d", len(slack))
	}
}

func TestRunNotificationFailureExitCode(t *testing.T) {
	e := newEnv(t)
	e.extra = []string{"UNTH_IT_HOOK=http://127.0.0.1:1/unreachable", "UNTH_IT_HOOK_SECRET=s3cret-value", "UNTH_IT_SLACK=http://127.0.0.1:1/x"}
	s, r := e.runJob(e.config(cfgOpts{extra: notifyYAML}))
	if r.code != 4 || s.Outcome != "success" || len(s.Notifications) != 2 || s.Notifications[0].Error == "" {
		t.Fatalf("code=%d %+v", r.code, s)
	}
}

func TestRunRefusesOverlappingJob(t *testing.T) {
	e := newEnv(t)
	cfg := e.config(cfgOpts{})
	_ = os.MkdirAll(filepath.Join(e.dataDir, "locks"), 0o700)
	f, err := os.OpenFile(filepath.Join(e.dataDir, "locks", "it-shop.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	s, r := e.runJob(cfg)
	if r.code != 1 || s.Stage != "lock" || s.BackupID != "" {
		t.Fatalf("code=%d %+v", r.code, s)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if s, r := e.runJob(cfg); r.code != 0 || s.Outcome != "success" {
		t.Fatalf("after unlock: code=%d %+v", r.code, s)
	}
}

func TestPruneRequiresRules(t *testing.T) {
	e := newEnv(t)
	if r := e.run("prune", "--config", e.config(cfgOpts{})); r.code != 2 || !strings.Contains(r.stderr, "no retention rules") {
		t.Fatalf("%d %s", r.code, r.stderr)
	}
}
