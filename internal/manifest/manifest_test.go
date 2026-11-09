package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	m := &Manifest{FormatVersion: FormatVersion, BackupID: "bk-x-1", Artifact: Artifact{Path: ArtifactName, SHA256: "ab"},
		Integrity: DefaultIntegrity()}
	if err := Write(p, m); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
	got, err := Read(p)
	if err != nil || got.BackupID != "bk-x-1" || got.Integrity.Signed {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestReadRejects(t *testing.T) {
	d := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(d, name)
		_ = os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	for name, body := range map[string]string{
		"bad-json":  "{",
		"version":   `{"format_version": 2}`,
		"traversal": `{"format_version": 1, "artifact": {"path": "../../etc/passwd"}}`,
		"subdir":    `{"format_version": 1, "artifact": {"path": "a/b"}}`,
	} {
		if _, err := Read(write(name, body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestHashFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	data := strings.Repeat("unth", 1000)
	_ = os.WriteFile(p, []byte(data), 0o600)
	sum, n, err := HashFile(p)
	want := sha256.Sum256([]byte(data))
	if err != nil || n != int64(len(data)) || sum != hex.EncodeToString(want[:]) {
		t.Fatalf("%s %d %v", sum, n, err)
	}
}
