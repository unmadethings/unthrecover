// Package postgres wraps the official PostgreSQL client tools (pg_dump,
// pg_restore) and computes database fingerprints used for verification.
//
// Connection parameters are passed to the tools through PG* environment
// variables (never as a connection string argument, which could be abused
// for conninfo injection) and the password through a private PGPASSFILE.
package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/security"
)

// MinServerMajor is the oldest PostgreSQL major version Unth Recover accepts.
const MinServerMajor = 12

// ConnParams describes a PostgreSQL endpoint.
type ConnParams struct {
	Host           string
	Port           int
	Database       string
	User           string
	Password       string
	SSLMode        string
	ConnectTimeout int // seconds
}

// ToolInfo describes a client binary.
type ToolInfo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version"`
	Major   int    `json:"major"`
}

var versionRe = regexp.MustCompile(`\(PostgreSQL\)\s+(\d+)(?:\.(\d+))?`)

// LookupTool finds a PostgreSQL client tool and parses its version.
func LookupTool(ctx context.Context, name string) (*ToolInfo, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, failure.New(failure.ToolMissing, name+" not found in PATH", err).
			WithHint("install the PostgreSQL client tools (e.g. apt install postgresql-client-16)")
	}
	out, err := exec.CommandContext(ctx, path, "--version").Output() // #nosec G204 -- fixed tool, fixed arg
	if err != nil {
		return nil, failure.New(failure.ToolMissing, name+" --version failed", err)
	}
	m := versionRe.FindStringSubmatch(string(out))
	if m == nil {
		return nil, failure.New(failure.ToolMissing, "cannot parse "+name+" version: "+strings.TrimSpace(string(out)), nil)
	}
	major, _ := strconv.Atoi(m[1])
	return &ToolInfo{Name: name, Path: path, Version: strings.TrimSpace(string(out)), Major: major}, nil
}

// CheckDumpCompatibility enforces that pg_dump can dump the server.
func CheckDumpCompatibility(tool *ToolInfo, serverMajor int) error {
	if serverMajor < MinServerMajor {
		return failure.New(failure.UnsupportedVersion,
			fmt.Sprintf("PostgreSQL %d is not supported (minimum %d)", serverMajor, MinServerMajor), nil)
	}
	if tool.Major < serverMajor {
		return failure.New(failure.UnsupportedVersion,
			fmt.Sprintf("server is PostgreSQL %d but %s is version %d; pg_dump cannot dump a newer server", serverMajor, tool.Name, tool.Major), nil).
			WithHint(fmt.Sprintf("install postgresql-client-%d or newer", serverMajor))
	}
	return nil
}

// toolEnv builds a minimal child environment. Ambient PG* variables from the
// caller are deliberately not inherited so that only the configured
// connection is used.
func toolEnv(p ConnParams, passfile string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"LC_ALL=C",
		"PGHOST=" + p.Host,
		"PGPORT=" + strconv.Itoa(p.Port),
		"PGUSER=" + p.User,
		"PGDATABASE=" + p.Database,
		"PGAPPNAME=unth-recover",
	}
	if p.SSLMode != "" {
		env = append(env, "PGSSLMODE="+p.SSLMode)
	}
	if p.ConnectTimeout > 0 {
		env = append(env, "PGCONNECT_TIMEOUT="+strconv.Itoa(p.ConnectTimeout))
	}
	if passfile != "" {
		env = append(env, "PGPASSFILE="+passfile)
	} else if home := os.Getenv("HOME"); home != "" {
		env = append(env, "HOME="+home) // allow a user-managed ~/.pgpass
	}
	return env
}

// limitedBuffer keeps the first n bytes written.
type limitedBuffer struct {
	buf bytes.Buffer
	n   int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}

// Dump runs pg_dump in custom format, streaming the archive to w. If snapshot
// is non-empty, the dump uses that exported snapshot so it is consistent with
// the fingerprint taken in the same transaction.
func Dump(ctx context.Context, tool *ToolInfo, p ConnParams, snapshot string, w io.Writer, red *security.Redactor) error {
	pf, err := security.NewPassFile(p.Host, p.Port, p.Database, p.User, p.Password)
	if err != nil {
		return failure.New(failure.Internal, "create password file", err)
	}
	defer pf.Remove()

	args := []string{"--format=custom", "--no-password", "--compress=6", "--lock-wait-timeout=60000"}
	if snapshot != "" {
		args = append(args, "--snapshot="+snapshot)
	}
	cmd := exec.CommandContext(ctx, tool.Path, args...) // #nosec G204 -- args are fixed flags; connection via env
	cmd.Env = toolEnv(p, pf.Path())
	stderr := &limitedBuffer{n: 64 << 10}
	cmd.Stderr = stderr
	cmd.Stdout = w
	runErr := cmd.Run()
	if runErr != nil {
		msg := red.Redact(strings.TrimSpace(stderr.buf.String()))
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return classifyToolError("pg_dump failed", msg, runErr)
	}
	return nil
}

// ListArchive runs `pg_restore --list` to validate the archive table of contents.
// It returns the number of TOC entries.
func ListArchive(ctx context.Context, tool *ToolInfo, path string) (int, error) {
	cmd := exec.CommandContext(ctx, tool.Path, "--list", "--", path) // #nosec G204 -- path validated by caller
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
	var out bytes.Buffer
	stderr := &limitedBuffer{n: 16 << 10}
	cmd.Stdout = &limitedWriter{w: &out, n: 64 << 20}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, failure.New(failure.ArtifactInvalid, "archive is not a readable pg_dump archive: "+strings.TrimSpace(stderr.buf.String()), err)
	}
	n := 0
	for _, line := range strings.Split(out.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, ";") {
			n++
		}
	}
	return n, nil
}

type limitedWriter struct {
	w io.Writer
	n int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("output limit exceeded")
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.w.Write(p)
	l.n -= int64(n)
	return n, err
}

// Restore runs pg_restore into the target in a single transaction, aborting
// on the first error. Ownership and privileges are not restored because the
// isolated target has no application roles.
func Restore(ctx context.Context, tool *ToolInfo, p ConnParams, path string, red *security.Redactor) error {
	pf, err := security.NewPassFile(p.Host, p.Port, p.Database, p.User, p.Password)
	if err != nil {
		return failure.New(failure.Internal, "create password file", err)
	}
	defer pf.Remove()
	args := []string{"--no-password", "--exit-on-error", "--single-transaction", "--no-owner", "--no-privileges",
		"--dbname=" + p.Database, "--", path}
	cmd := exec.CommandContext(ctx, tool.Path, args...) // #nosec G204 -- db name is internal constant; path validated
	cmd.Env = toolEnv(p, pf.Path())
	stderr := &limitedBuffer{n: 64 << 10}
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failure.New(failure.RestoreFailed, "pg_restore failed: "+red.Redact(strings.TrimSpace(stderr.buf.String())), err)
	}
	return nil
}

func classifyToolError(prefix, stderr string, err error) error {
	low := strings.ToLower(stderr)
	cat := failure.BackupFailed
	hint := ""
	switch {
	case strings.Contains(low, "password authentication failed"), strings.Contains(low, "no password supplied"),
		strings.Contains(low, "authentication failed"):
		cat, hint = failure.AuthenticationFail, "check source.user and the password source (password_env/password_file)"
	case strings.Contains(low, "server version mismatch"), strings.Contains(low, "aborting because of server version mismatch"):
		cat, hint = failure.UnsupportedVersion, "install a pg_dump whose major version is >= the server's"
	case strings.Contains(low, "could not connect"), strings.Contains(low, "connection refused"),
		strings.Contains(low, "connection to server"), strings.Contains(low, "timeout expired"):
		cat, hint = failure.ConnectionFailed, "check source.host/port and network reachability"
	case strings.Contains(low, "no space left"):
		cat = failure.InsufficientStorage
	}
	if stderr == "" {
		stderr = err.Error()
	}
	return failure.New(cat, prefix+": "+stderr, err).WithHint(hint)
}
