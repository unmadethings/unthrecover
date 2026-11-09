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
