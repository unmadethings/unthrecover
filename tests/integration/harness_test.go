//go:build integration

// Package integration contains end-to-end tests that drive the real `unth`
// binary against real PostgreSQL containers. Run with:
//
//	make test-integration     (or: go test -tags integration ./tests/integration/...)
//
// Requirements: Docker daemon, pg_dump/pg_restore/psql on PATH.
// Every container the harness creates carries the label
// io.unthrecover.test=<session id>; only those are removed at the end.
// If UNTH_INTEGRATION_REQUIRED=1, a missing prerequisite FAILS the suite
// instead of skipping it (used in CI so tests are never silently not run).
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var (
	binPath      string
	sessionLabel string
	src          *pgContainer
	restoreImage = envOr("UNTH_TEST_RESTORE_IMAGE", "postgres:16-alpine")
	sourceImage  = envOr("UNTH_TEST_SOURCE_IMAGE", "postgres:16-alpine")
	repoRoot     string
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type pgContainer struct {
	Name     string
	Port     string
	Password string
	PassFile string
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	_, file, _, _ := runtime.Caller(0)
	repoRoot = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	required := os.Getenv("UNTH_INTEGRATION_REQUIRED") == "1"
	missing := func(why string) int {
		if required {
			fmt.Fprintln(os.Stderr, "INTEGRATION PREREQUISITE MISSING:", why)
			return 1
		}
		fmt.Fprintln(os.Stderr, "SKIP integration tests (NOT RUN):", why)
		return 0
	}
	for _, tool := range []string{"docker", "pg_dump", "pg_restore", "psql"} {
		if _, err := exec.LookPath(tool); err != nil {
			return missing(tool + " not found on PATH")
		}
	}
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return missing("Docker daemon not reachable: " + strings.TrimSpace(string(out)))
	}

	tmp, err := os.MkdirTemp("", "unth-it-")
	if err != nil {
		return missing(err.Error())
	}
	defer os.RemoveAll(tmp)
	binPath = filepath.Join(tmp, "unth")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/unth")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintln(os.Stderr, "build failed:", string(out))
		return 1
	}

	sessionLabel = "it-" + randHex(4)
	defer removeTestContainers()
	for _, img := range []string{sourceImage, restoreImage} {
		if err := ensureImage(img); err != nil {
			return missing("cannot obtain image " + img + ": " + err.Error())
		}
	}
	src, err = startPostgres(sourceImage, tmp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start source:", err)
		return 1
	}
	if err := psqlFile(src, "shop", filepath.Join(repoRoot, "tests", "fixtures", "seed.sql")); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		return 1
	}
	code := m.Run()
	if left := managedResources(""); len(left) > 0 {
		fmt.Fprintln(os.Stderr, "LEFTOVER unth-managed resources after suite:", left)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func ensureImage(img string) error {
	if exec.Command("docker", "image", "inspect", img).Run() == nil {
		return nil
	}
	var lastErr error
	for i := 0; i < 3; i++ {
		out, err := exec.Command("docker", "pull", "-q", img).CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		time.Sleep(time.Duration(5*(i+1)) * time.Second)
	}
	return lastErr
}

func startPostgres(image, dir string) (*pgContainer, error) {
	c := &pgContainer{Name: "unth-it-src-" + randHex(4), Password: "src-" + randHex(12)}
	c.PassFile = filepath.Join(dir, c.Name+".pass")
	if err := os.WriteFile(c.PassFile, []byte(c.Password+"\n"), 0o600); err != nil {
		return nil, err
	}
	out, err := exec.Command("docker", "run", "-d", "--name", c.Name, "--label", "io.unthrecover.test="+sessionLabel,
		"-e", "POSTGRES_PASSWORD="+c.Password, "-e", "POSTGRES_DB=shop", "-p", "127.0.0.1::5432",
		"--memory", "1g", image).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run: %v %s", err, out)
	}
	for i := 0; i < 60; i++ {
		p, _ := exec.Command("docker", "port", c.Name, "5432").Output()
		if f := strings.SplitN(strings.TrimSpace(strings.Split(string(p), "\n")[0]), ":", 2); len(f) == 2 {
			c.Port = f[1]
			if psql(c, "shop", "SELECT 1") == nil {
				return c, nil
			}
		}
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("source postgres %s did not become ready", c.Name)
}

func psqlCmd(c *pgContainer, db string, args ...string) *exec.Cmd {
	cmd := exec.Command("psql", append([]string{"-X", "-q", "-v", "ON_ERROR_STOP=1", "-h", "127.0.0.1", "-p", c.Port, "-U", "postgres", "-d", db}, args...)...)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+c.Password) // test harness only
	return cmd
}

func psql(c *pgContainer, db, sql string) error {
	out, err := psqlCmd(c, db, "-c", sql).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

func psqlValue(t *testing.T, c *pgContainer, db, sql string) string {
	t.Helper()
	out, err := psqlCmd(c, db, "-At", "-c", sql).Output()
	if err != nil {
		t.Fatalf("psql %q: %v", sql, err)
	}
	return strings.TrimSpace(string(out))
}

func psqlFile(c *pgContainer, db, file string) error {
	out, err := psqlCmd(c, db, "-f", file).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

func removeTestContainers() {
	out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=io.unthrecover.test="+sessionLabel).Output()
	for _, id := range strings.Fields(string(out)) {
		_ = exec.Command("docker", "rm", "-f", "-v", id).Run()
	}
}

// managedResources lists Docker resources labeled as created by unth
// (optionally for one run).
func managedResources(runID string) []string {
	filter := "label=io.unthrecover.managed=true"
	var left []string
	for _, kind := range [][]string{{"ps", "-aq"}, {"volume", "ls", "-q"}, {"network", "ls", "-q"}} {
		args := append(append([]string{}, kind...), "--filter", filter)
		if runID != "" {
			args = append(args, "--filter", "label=io.unthrecover.run="+runID)
		}
		out, _ := exec.Command("docker", args...).Output()
		for _, id := range strings.Fields(string(out)) {
			left = append(left, kind[0]+":"+id)
		}
	}
	return left
}

// env is a per-test workspace with its own data dir.
type env struct {
	t       *testing.T
	dataDir string
	extra   []string
	logs    *bytes.Buffer // all stdout+stderr produced by unth in this test
}

func newEnv(t *testing.T) *env {
	return &env{t: t, dataDir: filepath.Join(t.TempDir(), "data"), logs: &bytes.Buffer{}}
}

type result struct {
	code   int
	stdout string
	stderr string
}

func (e *env) run(args ...string) result {
	e.t.Helper()
	return e.runCtx(context.Background(), args...)
}

func (e *env) runCtx(ctx context.Context, args ...string) result {
	e.t.Helper()
	cmd := e.command(ctx, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatalf("run unth: %v", err)
	}
	e.logs.WriteString(out.String())
	e.logs.WriteString(errb.String())
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

func (e *env) command(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{"--data-dir", e.dataDir, "--log-level", "debug"}, args...)
	cmd := exec.CommandContext(ctx, binPath, full...)
	cmd.Env = append(os.Environ(), e.extra...)
	return cmd
}

type cfgOpts struct {
	db, passFile, port, image, pull, timeout string
	minFree                                  int64
	queries                                  bool
	extra                                    string // raw YAML appended at top level
}

func (e *env) config(o cfgOpts) string {
	e.t.Helper()
	if o.db == "" {
		o.db = "shop"
	}
	if o.passFile == "" {
		o.passFile = src.PassFile
	}
	if o.port == "" {
		o.port = src.Port
	}
	if o.image == "" {
		o.image = restoreImage
	}
	if o.pull == "" {
		o.pull = "if-missing"
	}
	if o.timeout == "" {
		o.timeout = "10m"
	}
	if o.minFree == 0 {
		o.minFree = 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, "version: 1\nsource:\n  name: it-%s\n  host: 127.0.0.1\n  port: %s\n  database: %s\n  user: postgres\n  password_file: %s\n  sslmode: disable\n",
		o.db, o.port, o.db, o.passFile)
	fmt.Fprintf(&b, "backup:\n  timeout: 10m\n  min_free_bytes: %d\n", o.minFree)
	fmt.Fprintf(&b, "verification:\n  restore_image: %s\n  image_pull: %s\n  timeout: %s\n  memory_mb: 512\n", o.image, o.pull, o.timeout)
	if o.queries {
		b.WriteString("  queries:\n    - name: paid_total\n      sql: SELECT sum(total_cents) FROM orders WHERE status = 'paid'\n" +
			"    - name: invoice_amounts\n      sql: SELECT order_id, amount FROM billing.invoices WHERE order_id % 97 = 0\n")
	}
	b.WriteString(o.extra)
	p := filepath.Join(e.t.TempDir(), "job.yaml")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// backup runs `unth backup postgres` and returns the backup view.
func (e *env) backup(cfg string) (map[string]any, result) {
	e.t.Helper()
	r := e.run("backup", "postgres", "--config", cfg, "--format", "json")
	var v map[string]any
	_ = json.Unmarshal([]byte(r.stdout), &v)
	return v, r
}

func (e *env) mustBackup(cfg string) string {
	e.t.Helper()
	v, r := e.backup(cfg)
	if r.code != 0 || v["status"] != "BACKUP_CREATED" {
		e.t.Fatalf("backup failed: code=%d stdout=%s stderr=%s", r.code, r.stdout, r.stderr)
	}
	return v["id"].(string)
}

type report struct {
	RunID         string `json:"run_id"`
	FinalStatus   string `json:"final_status"`
	ErrorCategory string `json:"error_category"`
	ErrorMessage  string `json:"error_message"`
	Steps         []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"steps"`
	Comparison *struct {
		TablesChecked  int   `json:"tables_checked"`
		TablesHashed   int   `json:"tables_hashed"`
		RowsExpected   int64 `json:"rows_expected"`
		RowsActual     int64 `json:"rows_actual"`
		QueriesChecked int   `json:"queries_checked"`
	} `json:"comparison"`
	Cleanup struct {
		Status  string   `json:"status"`
		Removed []string `json:"removed"`
	} `json:"cleanup"`
	Scope struct {
		Covered    []string `json:"covered"`
		NotCovered []string `json:"not_covered"`
	} `json:"verification_scope"`
}

func (e *env) verify(id string, extra ...string) (*report, result) {
	e.t.Helper()
	r := e.run(append([]string{"verify", id, "--format", "json"}, extra...)...)
	var rep report
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		return nil, r
	}
	return &rep, r
}

func (rep *report) step(name string) string {
	for _, s := range rep.Steps {
		if s.Name == name {
			return s.Status
		}
	}
	return "ABSENT"
}

func (e *env) assertNoResources(runID string) {
	e.t.Helper()
	if left := managedResources(runID); len(left) > 0 {
		e.t.Fatalf("resources of run %s were not cleaned up: %v", runID, left)
	}
}

func (e *env) artifactPath(id string) string {
	return filepath.Join(e.dataDir, "backups", id, "artifact.dump")
}
