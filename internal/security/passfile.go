package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PassFile is a temporary PostgreSQL password file (PGPASSFILE). libpq tools
// read the password from it, so the secret never appears in process
// arguments (visible in `ps`) or in the child's environment.
type PassFile struct {
	dir  string
	path string
}

// NewPassFile writes a single-entry pgpass file inside a fresh 0700 temp
// directory. The caller must call Remove.
func NewPassFile(host string, port int, database, user, password string) (*PassFile, error) {
	dir, err := os.MkdirTemp("", "unth-pgpass-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- private directory, 0700 is correct
		_ = os.RemoveAll(dir)
		return nil, err
	}
	line := fmt.Sprintf("%s:%d:%s:%s:%s\n",
		pgpassEscape(host), port, pgpassEscape(database), pgpassEscape(user), pgpassEscape(password))
	p := filepath.Join(dir, "pgpass")
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &PassFile{dir: dir, path: p}, nil
}

// Path returns the file location to use as PGPASSFILE.
func (p *PassFile) Path() string { return p.path }

// Remove deletes the file and its directory. It is safe to call twice.
func (p *PassFile) Remove() {
	if p == nil || p.dir == "" {
		return
	}
	_ = os.RemoveAll(p.dir)
	p.dir = ""
}

// pgpassEscape escapes ':' and '\' as required by the pgpass format.
func pgpassEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `:`, `\:`)
}

// ReadSecretFile reads a secret from a file, refusing files that are
// readable by group or others, and trims a single trailing newline.
func ReadSecretFile(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("secret file %q is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("secret file %q has permissions %o; must not be accessible by group/others (chmod 600)", path, fi.Mode().Perm())
	}
	if fi.Size() > 64*1024 {
		return "", fmt.Errorf("secret file %q is unexpectedly large", path)
	}
	b, err := os.ReadFile(path) // #nosec G304 -- operator-configured secret file; mode and type checked above
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
