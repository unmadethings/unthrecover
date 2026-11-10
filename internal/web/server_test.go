package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/repo"
	"github.com/unmadethings/unthrecover/internal/security"
)

const tok = "test-token-0123456789abcdef-xyz"

func newTestServer(t *testing.T, tls bool) (*Server, *httptest.Server, *metadata.Store) {
	t.Helper()
	ctx := context.Background()
	r, err := repo.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := metadata.Open(ctx, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	for i, id := range []string{"bk-verified-aaaa", "bk-failed-bbbb"} {
		b := &metadata.Backup{ID: id, WorkloadID: "shop", BackupType: "postgres-logical", Status: metadata.BackupPending,
			CreatedAt: now.Add(-time.Duration(i) * time.Hour), SourceJSON: "{}"}
		_ = st.InsertBackup(ctx, b)
		b.Status, b.SizeBytes = metadata.BackupCreated, 1000
		if i == 0 {
			b.VerificationStatus = metadata.VerifyVerified
		} else {
			b.VerificationStatus = metadata.VerifyFailed
		}
		_ = st.UpdateBackup(ctx, b)
	}
	run := &metadata.Run{ID: "vr-run-aaaa", BackupID: "bk-verified-aaaa", Status: metadata.VerifyRunning, StartedAt: now}
	_ = st.InsertRun(ctx, run)
	run.Status, run.RestoreDurationMS = metadata.VerifyVerified, 1500
	run.ReportJSON = `{"run_id":"vr-run-aaaa","final_status":"VERIFIED","steps":[],"verification_scope":{"covered":["x"],"not_covered":["y"]}}`
	_ = st.UpdateRun(ctx, run)
	fb := &metadata.Backup{ID: "bk-broken-cccc", WorkloadID: "shop", BackupType: "postgres-logical", Status: metadata.BackupPending,
		CreatedAt: now.Add(-3 * time.Hour), SourceJSON: "{}"}
	_ = st.InsertBackup(ctx, fb)
	fb.Status, fb.ErrorCategory, fb.ErrorMessage = metadata.BackupFailed, "AUTHENTICATION_FAILED", "password hunter2-secret-x rejected"
	_ = st.UpdateBackup(ctx, fb)

	red := security.NewRedactor()
	red.Add("hunter2-secret-x")
	s := &Server{Store: st, Repo: r, Redactor: red, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Token: tok, TLS: tls}
	ts := httptest.NewServer(s.Handler(ctx))
	t.Cleanup(ts.Close)
	return s, ts, st
}

func do(t *testing.T, ts *httptest.Server, method, path, body, cookie string, csrf bool) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if cookie != "" {
		req.Header.Set("Cookie", cookieName+"="+cookie)
	}
	if csrf {
		req.Header.Set(csrfHeader, "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func login(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp := do(t, ts, "POST", "/api/login", `{"token":"`+tok+`"}`, "", false)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			return c.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}

func TestEveryAPIEndpointRequiresAuth(t *testing.T) {
	_, ts, _ := newTestServer(t, false)
	for _, p := range []string{"/api/session", "/api/overview", "/api/backups", "/api/backups/bk-verified-aaaa",
		"/api/runs/vr-run-aaaa/report", "/api/audit", "/api/info"} {
		if r := do(t, ts, "GET", p, "", "", false); r.StatusCode != 401 {
			t.Errorf("%s without session: %d", p, r.StatusCode)
		}
		if r := do(t, ts, "GET", p, "", "forged-session-id", false); r.StatusCode != 401 {
			t.Errorf("%s with forged session: %d", p, r.StatusCode)
		}
	}
	if r := do(t, ts, "POST", "/api/backups/bk-verified-aaaa/verify", "{}", "", true); r.StatusCode != 401 {
		t.Errorf("verify without session: %d", r.StatusCode)
	}
}

func TestLoginCookieFlagsAndLogout(t *testing.T) {
	for _, tls := range []bool{false, true} {
		_, ts, _ := newTestServer(t, tls)
		resp := do(t, ts, "POST", "/api/login", `{"token":"`+tok+`"}`, "", false)
		var c *http.Cookie
		for _, x := range resp.Cookies() {
			if x.Name == cookieName {
				c = x
			}
		}
		if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure != tls || len(c.Value) != 64 {
			t.Fatalf("tls=%v cookie %+v", tls, c)
		}
		if r := do(t, ts, "GET", "/api/session", "", c.Value, false); r.StatusCode != 200 {
			t.Fatal("session not accepted")
		}
		if r := do(t, ts, "POST", "/api/logout", "{}", c.Value, true); r.StatusCode != 200 {
			t.Fatal("logout failed")
		}
		if r := do(t, ts, "GET", "/api/session", "", c.Value, false); r.StatusCode != 401 {
			t.Fatal("session still valid after logout")
		}
	}
}

func TestWrongTokenAndRateLimit(t *testing.T) {
	_, ts, st := newTestServer(t, false)
	for i := 0; i < maxFailures; i++ {
		if r := do(t, ts, "POST", "/api/login", `{"token":"wrong-token-wrong-token-wrong"}`, "", false); r.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, r.StatusCode)
		}
	}
	r := do(t, ts, "POST", "/api/login", `{"token":"`+tok+`"}`, "", false)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("expected 429 even with the right token while limited, got %d", r.StatusCode)
	}
	evs, _ := st.ListAudit(context.Background(), 20)
	failed := 0
	for _, e := range evs {
		if e.Action == "ui.login" && e.Outcome == "failed" {
			failed++
		}
	}
	if failed != maxFailures {
		t.Fatalf("failed logins audited: %d", failed)
	}
}

func TestSessionExpiry(t *testing.T) {
	s, ts, _ := newTestServer(t, false)
	sid := login(t, ts)
	s.Now = func() time.Time { return time.Now().Add(sessionTTL + time.Minute) }
	if r := do(t, ts, "GET", "/api/session", "", sid, false); r.StatusCode != 401 {
		t.Fatal("expired session accepted")
	}
}

func TestCSRFHeaderRequired(t *testing.T) {
	_, ts, _ := newTestServer(t, false)
	sid := login(t, ts)
	if r := do(t, ts, "POST", "/api/backups/bk-verified-aaaa/verify", "{}", sid, false); r.StatusCode != 403 {
		t.Fatalf("POST without CSRF header: %d", r.StatusCode)
	}
	if r := do(t, ts, "POST", "/api/logout", "{}", sid, false); r.StatusCode != 403 {
		t.Fatalf("logout without CSRF header: %d", r.StatusCode)
	}
}

func TestSecurityHeadersAndSPA(t *testing.T) {
	_, ts, _ := newTestServer(t, true)
	for _, p := range []string{"/", "/some/deep/link", "/api/overview"} {
		r := do(t, ts, "GET", p, "", "", false)
		h := r.Header
		if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") || h.Get("X-Content-Type-Options") != "nosniff" ||
			h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Frame-Options") != "DENY" || h.Get("Strict-Transport-Security") == "" {
			t.Errorf("%s: missing security headers %v", p, h)
		}
	}
	r := do(t, ts, "GET", "/some/deep/link", "", "", false)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(b), "<title>Unth Recover</title>") {
		t.Fatalf("SPA fallback: %d", r.StatusCode)
	}
	if r := do(t, ts, "POST", "/", "", "", false); r.StatusCode != 405 {
		t.Fatalf("POST to static: %d", r.StatusCode)
	}
	if r := do(t, ts, "GET", "/api/unknown", "", "", false); r.StatusCode != 404 {
		t.Fatalf("unknown api: %d", r.StatusCode)
	}
}

func TestDataEndpoints(t *testing.T) {
	_, ts, _ := newTestServer(t, false)
	sid := login(t, ts)
	var ov map[string]any
	_ = json.NewDecoder(do(t, ts, "GET", "/api/overview", "", sid, false).Body).Decode(&ov)
	if ov["protected_workloads"].(float64) != 1 || ov["verified_runs_30d"].(float64) != 1 || ov["storage_bytes"].(float64) != 2000 {
		t.Fatalf("overview %v", ov)
	}
	var list []map[string]any
	_ = json.NewDecoder(do(t, ts, "GET", "/api/backups?workload=shop", "", sid, false).Body).Decode(&list)
	if len(list) != 3 {
		t.Fatalf("backups %d", len(list))
	}
	for _, b := range list {
		if msg, _ := b["error_message"].(string); strings.Contains(msg, "hunter2-secret-x") {
			t.Fatal("secret leaked through API")
		}
	}
	var det map[string]any
	_ = json.NewDecoder(do(t, ts, "GET", "/api/backups/bk-verified-aaaa", "", sid, false).Body).Decode(&det)
	if len(det["runs"].([]any)) != 1 {
		t.Fatalf("detail %v", det)
	}
	if r := do(t, ts, "GET", "/api/runs/vr-run-aaaa/report", "", sid, false); r.StatusCode != 200 {
		t.Fatalf("report %d", r.StatusCode)
	}
	if r := do(t, ts, "GET", "/api/backups/..%2f..%2fetc", "", sid, false); r.StatusCode != 400 && r.StatusCode != 404 {
		t.Fatalf("invalid id: %d", r.StatusCode)
	}
	if r := do(t, ts, "GET", "/api/backups/bk-missing-zzzz", "", sid, false); r.StatusCode != 404 {
		t.Fatalf("missing: %d", r.StatusCode)
	}
	if r := do(t, ts, "POST", "/api/backups/bk-broken-cccc/verify", "{}", sid, true); r.StatusCode != 409 {
		t.Fatalf("verify FAILED backup: %d", r.StatusCode)
	}
	if r := do(t, ts, "GET", "/api/audit", "", sid, false); r.StatusCode != 200 {
		t.Fatal("audit")
	}
}

func TestListenAndTokenPolicy(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:8484": true, "localhost:1": true, "[::1]:8484": true,
		"0.0.0.0:8484": false, "10.0.0.5:80": false, ":8484": false} {
		if err := CheckListen(addr, false); (err == nil) != ok {
			t.Errorf("%s plain: %v", addr, err)
		}
	}
	if CheckListen("0.0.0.0:8443", true) != nil {
		t.Error("TLS on all interfaces must be allowed")
	}
	if ValidateToken("short") == nil || ValidateToken(tok) != nil {
		t.Error("token policy")
	}
}
