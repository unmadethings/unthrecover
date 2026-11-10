package offsite

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strings"

	"github.com/unmadethings/unthrecover/internal/config"
)

// Rclone stores copies on any rclone remote: Google Drive, OneDrive, Dropbox,
// Box, pCloud, SFTP, WebDAV, ... The remote is configured once with
// `rclone config` (OAuth logins happen there, never in unth). unth runs the
// rclone binary with an argument list (no shell) and never passes secrets on
// the command line.
type Rclone struct {
	bin    string
	remote string // e.g. "gdrive:unth-backups" or "gdrive:"
	conf   string
}

// NewRclone creates the backend.
func NewRclone(o config.Offsite) (*Rclone, error) {
	bin := o.RcloneBinary
	if bin == "" {
		bin = "rclone"
	}
	p, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("rclone not found (%s); install rclone or set offsite.rclone_binary", bin)
	}
	return &Rclone{bin: p, remote: o.Remote, conf: o.RcloneConfig}, nil
}

// Target implements Backend.
func (r *Rclone) Target() string { return "rclone:" + r.remote }

func (r *Rclone) path(key string) string {
	if key == "" {
		return r.remote
	}
	if strings.HasSuffix(r.remote, ":") || strings.HasSuffix(r.remote, "/") {
		return r.remote + key
	}
	return r.remote + "/" + key
}

func (r *Rclone) cmd(ctx context.Context, args ...string) *exec.Cmd {
	base := []string{"--retries", "3", "--low-level-retries", "10", "--stats", "0", "--log-level", "ERROR"}
	if r.conf != "" {
		base = append(base, "--config", r.conf)
	}
	c := exec.CommandContext(ctx, r.bin, append(base, args...)...) // #nosec G204 -- fixed binary, argument list, validated remote/keys
	return c
}

func (r *Rclone) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	c := r.cmd(ctx, args...)
	c.Stdin = stdin
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &limitedBuffer{max: 4096, buf: &errb}
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("rclone %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// Check makes sure the remote is reachable (creating the folder if needed).
func (r *Rclone) Check(ctx context.Context) error {
	_, err := r.run(ctx, nil, "mkdir", "--", r.remote)
	return err
}

// Put implements Backend.
func (r *Rclone) Put(ctx context.Context, key string, rd io.Reader, size int64) error {
	args := []string{"rcat"}
	if size >= 0 {
		args = append(args, "--size", fmt.Sprint(size))
	}
	_, err := r.run(ctx, rd, append(args, "--", r.path(key))...)
	return err
}

// Get implements Backend.
func (r *Rclone) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if ok, err := r.exists(ctx, key); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNotExist
	}
	c := r.cmd(ctx, "cat", "--", r.path(key))
	var errb bytes.Buffer
	c.Stderr = &limitedBuffer{max: 4096, buf: &errb}
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	return &cmdReader{r: out, c: c, errb: &errb}, nil
}

func (r *Rclone) exists(ctx context.Context, key string) (bool, error) {
	out, err := r.run(ctx, nil, "lsf", "--files-only", "--max-depth", "1", "--", r.path(path.Dir(key)))
	if err != nil {
		if strings.Contains(err.Error(), "directory not found") {
			return false, nil
		}
		return false, err
	}
	for _, l := range strings.Split(string(out), "\n") {
		if l == path.Base(key) {
			return true, nil
		}
	}
	return false, nil
}

// List implements Backend.
func (r *Rclone) List(ctx context.Context, prefix string) ([]string, error) {
	dir := strings.TrimSuffix(prefix, "/")
	out, err := r.run(ctx, nil, "lsf", "-R", "--files-only", "--", r.path(dir))
	if err != nil {
		if strings.Contains(err.Error(), "directory not found") {
			return nil, nil
		}
		return nil, err
	}
	var keys []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if l := sc.Text(); l != "" {
			if dir == "" {
				keys = append(keys, l)
			} else {
				keys = append(keys, dir+"/"+l)
			}
		}
	}
	return keys, sc.Err()
}

// Delete implements Backend.
func (r *Rclone) Delete(ctx context.Context, key string) error {
	_, err := r.run(ctx, nil, "deletefile", "--", r.path(key))
	if err != nil && (strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "doesn't exist")) {
		return nil
	}
	return err
}

type cmdReader struct {
	r    io.ReadCloser
	c    *exec.Cmd
	errb *bytes.Buffer
	done bool
}

func (c *cmdReader) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF // Wait closed the pipe; further reads see end of stream
	}
	n, err := c.r.Read(p)
	if errors.Is(err, io.EOF) && !c.done {
		c.done = true
		if werr := c.c.Wait(); werr != nil {
			return n, fmt.Errorf("rclone cat: %w: %s", werr, strings.TrimSpace(c.errb.String()))
		}
	}
	return n, err
}

func (c *cmdReader) Close() error {
	_ = c.r.Close()
	if !c.done {
		c.done = true
		if c.c.Process != nil {
			_ = c.c.Process.Kill()
		}
		_ = c.c.Wait()
	}
	return nil
}

type limitedBuffer struct {
	max int
	buf *bytes.Buffer
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}
