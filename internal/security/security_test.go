package security

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactor(t *testing.T) {
	r := NewRedactor()
	r.Add("s3cr3t-pass", "", "ab", "s3cr3t-pass-longer")
	got := r.Redact("dsn password=s3cr3t-pass-longer and s3cr3t-pass end")
	if strings.Contains(got, "s3cr3t") {
		t.Fatalf("secret leaked: %q", got)
	}
	if !strings.Contains(got, RedactedPlaceholder) {
		t.Fatalf("placeholder missing: %q", got)
	}
	if r.Redact("ab cd") != "ab cd" {
		t.Fatal("short values must not be registered")
	}
	if r.RedactError(errors.New("x s3cr3t-pass y")) != "x [REDACTED] y" {
		t.Fatal("RedactError")
	}
	var nilR *Redactor
	if nilR.Redact("a") != "a" {
		t.Fatal("nil redactor must be a no-op")
	}
}

func TestValidateID(t *testing.T) {
	for _, ok := range []string{"bk-20261109t181640-3f9a1c2b", NewID("vr"), "abc"} {
		if err := ValidateID(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ab", "../etc", "a/b", "A-UPPER", "-lead", "x y", strings.Repeat("a", 65), "id;rm"} {
		if err := ValidateID(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	if p, err := SafeJoin(root, "backups/x/artifact.dump"); err != nil || !strings.HasPrefix(p, root) {
		t.Fatalf("valid path rejected: %v", err)
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "backups/../../x", "a/../../b"} {
		if _, err := SafeJoin(root, bad); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("%q: expected ErrUnsafePath, got %v", bad, err)
		}
	}
}

func TestCheckNoSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "backups", "b1"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, "backups", "b1", "artifact.dump")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckNoSymlinks(root, f); err != nil {
		t.Fatalf("regular file rejected: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(outside, []byte("y"), 0o600)
	link := filepath.Join(root, "backups", "b1", "link.dump")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unsupported")
	}
	if err := CheckNoSymlinks(root, link); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink not detected: %v", err)
	}
	// symlinked directory component
	dirLink := filepath.Join(root, "backups", "b2")
	_ = os.Symlink(filepath.Dir(outside), dirLink)
	if err := CheckNoSymlinks(root, filepath.Join(dirLink, "secret")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlinked dir not detected: %v", err)
	}
	if err := CheckNoSymlinks(root, outside); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("outside path not rejected: %v", err)
	}
}

func TestEnsurePrivateDir(t *testing.T) {
	d := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(d); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(d)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %o, want 700", fi.Mode().Perm())
	}
}

func TestPassFile(t *testing.T) {
	pf, err := NewPassFile("db:host", 5432, "my:db", `us\er`, "pa:ss\\word")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(pf.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("pgpass mode %o", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(pf.Path())
	want := `db\:host:5432:my\:db:us\\er:pa\:ss\\word` + "\n"
	if string(b) != want {
		t.Fatalf("pgpass content %q, want %q", b, want)
	}
	dir := filepath.Dir(pf.Path())
	pf.Remove()
	pf.Remove()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("pgpass dir not removed")
	}
}

func TestReadSecretFile(t *testing.T) {
	d := t.TempDir()
	good := filepath.Join(d, "good")
	_ = os.WriteFile(good, []byte("hunter22\n"), 0o600)
	if v, err := ReadSecretFile(good); err != nil || v != "hunter22" {
		t.Fatalf("got %q %v", v, err)
	}
	loose := filepath.Join(d, "loose")
	_ = os.WriteFile(loose, []byte("x"), 0o644)
	if _, err := ReadSecretFile(loose); err == nil {
		t.Fatal("world-readable secret file accepted")
	}
	if _, err := ReadSecretFile(d); err == nil {
		t.Fatal("directory accepted")
	}
}
