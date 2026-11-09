// Package repo manages the local data directory layout:
//
//	<data-dir>/metadata.db          SQLite metadata
//	<data-dir>/backups/<id>/        artifact.dump + manifest.json
//	<data-dir>/reports/<run>.json   verification reports
//	<data-dir>/tmp/                 in-progress (partial) files
//
// All directories are created 0700 and files 0600.
package repo

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"syscall"

	"github.com/unmadethings/unthrecover/internal/security"
)

// Repo is a data directory.
type Repo struct {
	Root string
}

// Open prepares the directory layout.
func Open(root string) (*Repo, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{abs, filepath.Join(abs, "backups"), filepath.Join(abs, "reports"), filepath.Join(abs, "tmp")} {
		if err := security.EnsurePrivateDir(d); err != nil {
			return nil, fmt.Errorf("prepare %s: %w", d, err)
		}
	}
	return &Repo{Root: abs}, nil
}

// BackupDir returns the validated directory of a backup.
func (r *Repo) BackupDir(id string) (string, error) {
	if err := security.ValidateID(id); err != nil {
		return "", err
	}
	return security.SafeJoin(r.Root, filepath.Join("backups", id))
}

// TmpFile returns the path of the partial file for an operation.
func (r *Repo) TmpFile(id string) (string, error) {
	if err := security.ValidateID(id); err != nil {
		return "", err
	}
	return security.SafeJoin(r.Root, filepath.Join("tmp", id+".partial"))
}

// ReportPath returns the path of a verification report.
func (r *Repo) ReportPath(runID string) (string, error) {
	if err := security.ValidateID(runID); err != nil {
		return "", err
	}
	return security.SafeJoin(r.Root, filepath.Join("reports", runID+".json"))
}

// Resolve turns a repository-relative path from metadata into an absolute
// path, rejecting traversal and symlinks.
func (r *Repo) Resolve(rel string) (string, error) {
	p, err := security.SafeJoin(r.Root, rel)
	if err != nil {
		return "", err
	}
	if err := security.CheckNoSymlinks(r.Root, p); err != nil {
		return "", err
	}
	return p, nil
}

// Rel converts an absolute path inside the repo to a relative one.
func (r *Repo) Rel(abs string) (string, error) {
	return filepath.Rel(r.Root, abs)
}

// FreeBytes returns the free space available to unprivileged users.
func FreeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil // #nosec G115 -- filesystem sizes fit in int64
}

// Actor returns the OS user name for audit records.
func Actor() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return fmt.Sprintf("uid:%d", os.Getuid())
}
