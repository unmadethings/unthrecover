package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unmadethings/unthrecover/internal/security"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Execute(context.Background(), append([]string{"--data-dir", filepath.Join(t.TempDir(), "d")}, args...), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersionJSON(t *testing.T) {
	code, out, _ := run(t, "version", "--format", "json")
	var v map[string]string
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v["version"] == "" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	if code, _, _ := run(t, "version", "--format", "xml"); code != ExitUsage {
		t.Fatalf("bad format code %d", code)
	}
	if code, _, _ := run(t, "--log-level", "loud", "version"); code != ExitUsage {
		t.Fatalf("bad log level code %d", code)
	}
	if code, _, _ := run(t, "nosuchcommand"); code != ExitFailure {
		t.Fatalf("unknown command code %d", code)
	}
}

func TestInvalidConfigExitCode(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(cfg, []byte("version: 1\nsource:\n  name: x\n  host: h\n  database: d\n  user: u\n  password: hunter2\n"), 0o600)
	code, _, stderr := run(t, "backup", "postgres", "--config", cfg)
	if code != ExitUsage || !strings.Contains(stderr, "plaintext 'password' is not allowed") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stderr, "hunter2") {
		t.Fatal("secret echoed in error")
	}
	code, _, _ = run(t, "backup", "postgres", "--config", filepath.Join(t.TempDir(), "missing.yaml"))
	if code != ExitUsage {
		t.Fatalf("missing config code %d", code)
	}
}

func TestVerifyUnknownBackup(t *testing.T) {
	code, _, stderr := run(t, "verify", "bk-20260101t000000-deadbeef")
	if code != ExitUsage || !strings.Contains(stderr, "not found") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	code, _, _ = run(t, "verify", "../../etc")
	if code == 0 {
		t.Fatal("path-like id accepted")
	}
}

func TestEmptyListsAndCleanup(t *testing.T) {
	if code, out, _ := run(t, "backup", "list", "--format", "json"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("list: %d %q", code, out)
	}
	if code, out, _ := run(t, "audit", "list"); code != 0 || !strings.Contains(out, "ACTION") {
		t.Fatalf("audit: %d %q", code, out)
	}
}

func TestRedactingHandler(t *testing.T) {
	r := security.NewRedactor()
	r.Add("topsecretvalue")
	var buf bytes.Buffer
	log := slog.New(newRedactingHandler(slog.NewTextHandler(&buf, nil), r))
	log.With("dsn", "password=topsecretvalue").Info("connecting with topsecretvalue", "err", os.ErrNotExist, "x", []string{"topsecretvalue"})
	if strings.Contains(buf.String(), "topsecretvalue") {
		t.Fatalf("log leaked secret: %s", buf.String())
	}
}

func TestNextRunDaily(t *testing.T) {
	loc := time.FixedZone("X", 3*3600)
	now := time.Date(2026, 3, 10, 2, 59, 0, 0, loc)
	if got := nextRun(now, 0, 3, 0, true); !got.Equal(time.Date(2026, 3, 10, 3, 0, 0, 0, loc)) {
		t.Fatalf("same day: %v", got)
	}
	now = time.Date(2026, 3, 10, 3, 0, 0, 0, loc) // exactly at: next day
	if got := nextRun(now, 0, 3, 0, true); !got.Equal(time.Date(2026, 3, 11, 3, 0, 0, 0, loc)) {
		t.Fatalf("next day: %v", got)
	}
	now = time.Date(2026, 12, 31, 23, 30, 0, 0, loc)
	if got := nextRun(now, 0, 0, 15, true); !got.Equal(time.Date(2027, 1, 1, 0, 15, 0, 0, loc)) {
		t.Fatalf("year rollover: %v", got)
	}
	if got := nextRun(now, 6*time.Hour, 0, 0, false); !got.Equal(now.Add(6 * time.Hour)) {
		t.Fatalf("every: %v", got)
	}
}

func TestParseClock(t *testing.T) {
	for _, bad := range []string{"", "3", "25:00", "03:60", "3pm", "03:00:00"} {
		if _, _, err := parseClock(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if h, m, err := parseClock("03:30"); err != nil || h != 3 || m != 30 {
		t.Fatalf("03:30 -> %d %d %v", h, m, err)
	}
}

func TestListJobs(t *testing.T) {
	d := t.TempDir()
	for _, n := range []string{"b.yaml", "a.yml", ".hidden.yaml", "notes.txt", "c.env"} {
		_ = os.WriteFile(filepath.Join(d, n), []byte("x"), 0o600)
	}
	_ = os.Mkdir(filepath.Join(d, "sub.yaml"), 0o700)
	got, err := listJobs(d)
	if err != nil || len(got) != 2 || filepath.Base(got[0]) != "a.yml" || filepath.Base(got[1]) != "b.yaml" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestScheduleRejectsBadUsage(t *testing.T) {
	d := t.TempDir()
	empty := t.TempDir()
	_ = os.WriteFile(filepath.Join(d, "bad.yaml"), []byte("version: 1\nsource: {}\n"), 0o600)
	cases := [][]string{
		{"schedule", "--jobs-dir", d},                                   // neither --at nor --every
		{"schedule", "--jobs-dir", d, "--at", "03:00", "--every", "1h"}, // both
		{"schedule", "--jobs-dir", d, "--every", "30s"},                 // too frequent
		{"schedule", "--jobs-dir", d, "--at", "3pm"},                    // bad clock
		{"schedule", "--jobs-dir", empty, "--every", "1h"},              // no jobs
		{"schedule", "--jobs-dir", filepath.Join(d, "missing"), "--every", "1h"},
		{"schedule", "--jobs-dir", d, "--every", "1h"}, // invalid job config reported at start
	}
	for _, c := range cases {
		if code, _, _ := run(t, c...); code != 2 {
			t.Fatalf("%v: exit %d, want 2", c, code)
		}
	}
}
