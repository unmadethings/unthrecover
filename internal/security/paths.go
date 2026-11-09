package security

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrUnsafePath is returned when a path escapes its root or traverses a symlink.
var ErrUnsafePath = errors.New("unsafe path")

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)

// ValidateID checks that an identifier (backup ID, run ID) only contains
// characters that are safe to embed in file names, Docker resource names and
// labels. IDs are generated internally, but IDs also arrive from the CLI.
func ValidateID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid identifier %q: must match %s", id, idPattern.String())
	}
	return nil
}

// SafeJoin joins a relative path onto root and guarantees the result stays
// inside root. Absolute paths and ".." components are rejected rather than
// cleaned, so that a tampered metadata record cannot point outside the
// repository.
func SafeJoin(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("%w: empty path", ErrUnsafePath)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: absolute path %q not allowed", ErrUnsafePath, rel)
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: parent reference in %q", ErrUnsafePath, rel)
		}
	}
	cleanRoot := filepath.Clean(root)
	joined := filepath.Join(cleanRoot, rel)
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q escapes %q", ErrUnsafePath, rel, root)
	}
	return joined, nil
}

// CheckNoSymlinks verifies that no component of path below root is a
// symbolic link. It defends against an attacker with write access to the
// repository replacing an artifact or directory with a link to another file.
func CheckNoSymlinks(root, path string) error {
	cleanRoot := filepath.Clean(root)
	rel, err := filepath.Rel(cleanRoot, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("%w: %q is outside %q", ErrUnsafePath, path, root)
	}
	cur := cleanRoot
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %q is a symbolic link", ErrUnsafePath, cur)
		}
	}
	return nil
}

// EnsurePrivateDir creates dir (and parents) with 0700 permissions and
// tightens the mode of an existing directory that is group/world accessible.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %q is a symbolic link", ErrUnsafePath, dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%q is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700) // #nosec G302 -- private directory, 0700 is correct
	}
	return nil
}
