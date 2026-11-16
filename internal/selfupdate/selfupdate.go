// Package selfupdate installs a newer official release of unth. It runs as
// root (unth-update.service), started when the unprivileged agent writes a
// version to its request file. The request can only name a version: files
// come from the project's GitHub releases over HTTPS and must match the
// release's SHA256SUMS; older or equal versions are refused.
package selfupdate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultBase is where release files are downloaded from.
const DefaultBase = "https://github.com/unmadethings/unthrecover/releases/download"

const maxAsset = 200 << 20

var versionPattern = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z.]+))?$`)

// Updater installs releases.
type Updater struct {
	Base    string // release download base (tests: a local server)
	HTTP    *http.Client
	Current string // running version
	Arch    string // amd64 | arm64
	// Method is deb, rpm or binary (detected when empty).
	Method string
	// Binary is the file replaced by the binary method (default: this executable).
	Binary string
	// Run executes a command (dpkg, rpm, systemctl); tests replace it.
	Run func(ctx context.Context, name string, args ...string) error
}

// Newer reports whether version a is newer than b (semver; a pre-release
// sorts before its release).
func Newer(a, b string) (bool, error) {
	pa, err := parse(a)
	if err != nil {
		return false, err
	}
	pb, err := parse(b)
	if err != nil {
		return true, nil // a development build is always replaced by a release
	}
	for i := 0; i < 3; i++ {
		if pa.n[i] != pb.n[i] {
			return pa.n[i] > pb.n[i], nil
		}
	}
	switch {
	case pa.pre == pb.pre:
		return false, nil
	case pa.pre == "":
		return true, nil // release after its pre-releases
	case pb.pre == "":
		return false, nil
	}
	return comparePre(pa.pre, pb.pre) > 0, nil
}

type ver struct {
	n   [3]int
	pre string
}

func parse(v string) (ver, error) {
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	m := versionPattern.FindStringSubmatch(v)
	if m == nil {
		return ver{}, fmt.Errorf("invalid version %q", v)
	}
	var out ver
	for i := 0; i < 3; i++ {
		out.n[i], _ = strconv.Atoi(m[i+1])
	}
	out.pre = m[4]
	return out, nil
}

func comparePre(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		switch {
		case ea == nil && eb == nil && na != nb:
			if na > nb {
				return 1
			}
			return -1
		case pa[i] != pb[i]:
			if pa[i] > pb[i] {
				return 1
			}
			return -1
		}
	}
	return len(pa) - len(pb)
}

func (u *Updater) run(ctx context.Context, name string, args ...string) error {
	if u.Run != nil {
		return u.Run(ctx, name, args...)
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() // #nosec G204 -- fixed tools, validated file paths
	if err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (u *Updater) method(ctx context.Context) string {
	if u.Method != "" {
		return u.Method
	}
	if _, err := exec.LookPath("dpkg-query"); err == nil && exec.CommandContext(ctx, "dpkg-query", "-W", "unthrecover").Run() == nil {
		return "deb"
	}
	if _, err := exec.LookPath("rpm"); err == nil && exec.CommandContext(ctx, "rpm", "-q", "unthrecover").Run() == nil {
		return "rpm"
	}
	return "binary"
}

// asset is the release file for this install method.
func (u *Updater) asset(method, version string) string {
	plain := strings.TrimPrefix(version, "v")
	switch method {
	case "deb":
		return "unthrecover_" + plain + "_" + u.Arch + ".deb"
	case "rpm":
		return "unthrecover_" + plain + "_" + u.Arch + ".rpm"
	}
	return "unth_" + version + "_linux_" + u.Arch
}

func (u *Updater) get(ctx context.Context, url string, w io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("download %s: too large", url)
	}
	return nil
}

// Apply installs version if it is newer than the running one.
func (u *Updater) Apply(ctx context.Context, version string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("invalid version %q", version)
	}
	newer, err := Newer(version, u.Current)
	if err != nil {
		return err
	}
	if !newer {
		return fmt.Errorf("%s is not newer than the installed %s", version, u.Current)
	}
	if u.Arch == "" {
		u.Arch = runtime.GOARCH
	}
	if u.Base == "" {
		u.Base = DefaultBase
	}
	if u.HTTP == nil {
		u.HTTP = &http.Client{Timeout: 10 * time.Minute}
	}
	method := u.method(ctx)
	name := u.asset(method, version)
	base := strings.TrimRight(u.Base, "/") + "/" + version + "/"

	var sums strings.Builder
	if err := u.get(ctx, base+"SHA256SUMS", &sums, 64<<10); err != nil {
		return err
	}
	want := ""
	sc := bufio.NewScanner(strings.NewReader(sums.String()))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			if want != "" {
				return fmt.Errorf("SHA256SUMS lists %s twice", name)
			}
			want = strings.ToLower(f[0])
		}
	}
	if len(want) != 64 {
		return fmt.Errorf("SHA256SUMS has no entry for %s", name)
	}
	dir, err := os.MkdirTemp("", "unth-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if filepath.Base(name) != name || strings.Contains(name, "..") {
		return fmt.Errorf("invalid asset name %q", name)
	}
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 G703 -- private temp dir; name from a validated version (no separators)
	if err != nil {
		return err
	}
	h := sha256.New()
	err = u.get(ctx, base+name, io.MultiWriter(f, h), maxAsset)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s; nothing installed", name, want, got)
	}
	switch method {
	case "deb":
		err = u.run(ctx, "dpkg", "-i", p)
	case "rpm":
		err = u.run(ctx, "rpm", "-U", p)
	default:
		err = u.replaceBinary(ctx, p)
	}
	if err != nil {
		return err
	}
	return u.run(ctx, "systemctl", "try-restart", "unth-agent.service")
}

func (u *Updater) replaceBinary(ctx context.Context, p string) error {
	target := u.Binary
	if target == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if target, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
	}
	if err := os.Chmod(p, 0o755); err != nil { // #nosec G302 G703 -- an executable in our temp dir
		return err
	}
	if err := u.run(ctx, p, "version"); err != nil {
		return fmt.Errorf("the downloaded binary does not run here: %w", err)
	}
	tmp := filepath.Join(filepath.Dir(target), ".unth.new")
	in, err := os.Open(p) // #nosec G304 G703 -- verified download in our temp dir
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) // #nosec G302 G304 -- next to the installed binary
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// Result is written for the agent to report.
type Result struct {
	Version  string    `json:"version"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
	Finished time.Time `json:"finished"`
}

// ApplyRequest reads <agentDir>/update-request (removing it), applies it and
// writes <agentDir>/update-result.json.
func (u *Updater) ApplyRequest(ctx context.Context, agentDir string) error {
	req := filepath.Join(agentDir, "update-request")
	fi, err := os.Lstat(req)
	if errors.Is(err, os.ErrNotExist) {
		return nil // nothing asked (the path unit may fire on removal)
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() > 128 {
		_ = os.Remove(req)
		return errors.New("update request is not a small regular file")
	}
	raw, err := os.ReadFile(req) // #nosec G304 -- fixed name in the agent dir; content validated
	_ = os.Remove(req)
	if err != nil {
		return err
	}
	version := strings.TrimSpace(string(raw))
	err = u.Apply(ctx, version)
	r := Result{Version: version, OK: err == nil, Finished: time.Now().UTC()}
	if err != nil {
		r.Error = err.Error()
		if len(r.Error) > 500 {
			r.Error = r.Error[:500]
		}
	}
	b, _ := json.Marshal(r)
	out := filepath.Join(agentDir, "update-result.json")
	if werr := os.WriteFile(out+".tmp", b, 0o644); werr == nil { // #nosec G306 -- version and error text only; the agent (another user) reads it
		_ = os.Rename(out+".tmp", out)
	}
	return err
}
