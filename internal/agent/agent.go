// Package agent connects an unth installation to a control plane (Unth
// Recover Cloud): enrollment, signed outbound-only requests, job sync,
// commands, local scheduling and result events. See pkg/agentproto.
package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/unmadethings/unthrecover/internal/buildinfo"
	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/connections"
	"github.com/unmadethings/unthrecover/internal/offsite"
	"github.com/unmadethings/unthrecover/internal/security"
	"github.com/unmadethings/unthrecover/pkg/agentproto"
)

// State is the agent's enrollment, stored in <data-dir>/agent/agent.json.
type State struct {
	AgentID    string    `json:"agent_id"`
	URL        string    `json:"url"`
	OrgName    string    `json:"org_name"`
	EnrolledAt time.Time `json:"enrolled_at"`
}

// Dir returns the agent directory of a data dir.
func Dir(dataDir string) string { return filepath.Join(dataDir, "agent") }

// CheckURL accepts https, or http to a loopback address (tests, local setups).
func CheckURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("invalid control plane URL %q", raw)
	}
	if u.Scheme == "https" {
		return u.String(), nil
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) {
		return u.String(), nil
	}
	return "", fmt.Errorf("control plane URL must use https")
}

// Enroll creates the agent identity (if missing) and registers it with the
// one-time token. It refuses to enroll twice.
func Enroll(ctx context.Context, dataDir, rawURL, token string, hc *http.Client) (*State, error) {
	base, err := CheckURL(rawURL)
	if err != nil {
		return nil, err
	}
	dir := Dir(dataDir)
	if _, err := os.Stat(filepath.Join(dir, "agent.json")); err == nil {
		return nil, errors.New("this installation is already enrolled (remove <data-dir>/agent to enroll again)")
	}
	if err := security.EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, offsite.AgeKeyFile)); errors.Is(err, os.ErrNotExist) {
		if _, err := offsite.GenerateKeys(dir); err != nil {
			return nil, err
		}
	}
	keys, err := offsite.LoadKeys(filepath.Join(dir, offsite.AgeKeyFile), nil, filepath.Join(dir, offsite.SignKeyFile), "")
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	req := agentproto.EnrollRequest{Token: token, PublicKey: b64(keys.VerifyKey), AgeRecipient: keys.RecipientS[0],
		Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: buildinfo.Version, Protocol: agentproto.Version}
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, base+agentproto.PathEnroll, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("enroll: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("enroll refused: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var er agentproto.EnrollResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, agentproto.MaxBody)).Decode(&er); err != nil || er.AgentID == "" {
		return nil, errors.New("enroll: invalid response")
	}
	st := &State{AgentID: er.AgentID, URL: base, OrgName: er.OrgName, EnrolledAt: time.Now().UTC()}
	return st, writeJSONFile(filepath.Join(dir, "agent.json"), st)
}

// LoadState reads the enrollment.
func LoadState(dataDir string) (*State, error) {
	raw, err := os.ReadFile(filepath.Join(Dir(dataDir), "agent.json")) // #nosec G304 -- fixed path in the data dir
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("not enrolled: run `unth agent enroll --url ... --token ...` first")
	}
	if err != nil {
		return nil, err
	}
	var st State
	return &st, json.Unmarshal(raw, &st)
}

func writeJSONFile(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Client sends signed requests.
type Client struct {
	State *State
	Key   ed25519.PrivateKey
	HTTP  *http.Client
	Now   func() time.Time
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.State.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	agentproto.Sign(req, c.State.AgentID, c.Key, connections.RandomToken(), now, body)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s: HTTP %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, agentproto.MaxBody)).Decode(out)
}

func b64(k ed25519.PublicKey) string { return encodeStd(k) }

// ---------------------------------------------------------------- jobs

// Hooks run the actual work (provided by the CLI to avoid import cycles).
type Hooks struct {
	// RunJob runs one job like `unth run` and returns the summary as a map.
	RunJob func(ctx context.Context, cfg *config.Config) map[string]any
	// Verify verifies a backup and returns the summary.
	Verify func(ctx context.Context, backupID string) map[string]any
	// Status returns docker version and free bytes for the heartbeat.
	Status func(ctx context.Context) (docker string, free int64)
}

// Agent is the running agent.
type Agent struct {
	DataDir string
	Client  *Client
	Ident   *age.X25519Identity
	Hooks   Hooks
	Log     *slog.Logger
	Redact  func(string) string
	// Secrets registers values to redact from logs (e.g. Drive access tokens).
	Secrets func(...string)

	mu       sync.Mutex
	jobs     agentproto.JobsResponse
	next     map[string]time.Time
	queue    []work
	running  map[string]bool
	events   []agentproto.Event
	interval time.Duration
}

type work struct {
	cmdID, jobID, backupID string
}

// New loads the enrollment and keys.
func New(dataDir string, hooks Hooks, log *slog.Logger, redact func(string) string) (*Agent, error) {
	st, err := LoadState(dataDir)
	if err != nil {
		return nil, err
	}
	dir := Dir(dataDir)
	keys, err := offsite.LoadKeys(filepath.Join(dir, offsite.AgeKeyFile), nil, filepath.Join(dir, offsite.SignKeyFile), "")
	if err != nil {
		return nil, err
	}
	a := &Agent{DataDir: dataDir, Client: &Client{State: st, Key: keys.SignKey, HTTP: &http.Client{Timeout: 60 * time.Second}},
		Ident: keys.Identity, Hooks: hooks, Log: log, Redact: redact, next: map[string]time.Time{}, running: map[string]bool{},
		interval: 30 * time.Second}
	if raw, err := os.ReadFile(filepath.Join(dir, "jobs.json")); err == nil { // #nosec G304 -- fixed path
		_ = json.Unmarshal(raw, &a.jobs)
	}
	return a, nil
}

// jobPolicy applies the rules for jobs coming from the control plane: they
// may not read local files or choose keys; offsite copies always use this
// agent's own keys.
func (a *Agent) jobPolicy(j agentproto.Job) (*config.Config, error) {
	if !idOK(j.ID) {
		return nil, errors.New("invalid job id")
	}
	if _, _, _, _, err := agentproto.ParseSchedule(j.Schedule); err != nil {
		return nil, err
	}
	cfg, err := config.ParseUnvalidated([]byte(j.Config))
	if err != nil {
		return nil, err
	}
	switch {
	case cfg.Source.PasswordFile != "":
		return nil, errors.New("password_file is not allowed in managed jobs (use a secret)")
	case cfg.Encryption.IdentityFile != "" || cfg.Signing.KeyFile != "" || cfg.Signing.PublicKeyFile != "":
		return nil, errors.New("managed jobs use the agent's own keys; key paths are not allowed")
	case cfg.Offsite.RcloneBinary != "" || cfg.Offsite.RcloneConfig != "":
		return nil, errors.New("rclone_binary/rclone_config are not allowed in managed jobs")
	}
	dir := Dir(a.DataDir)
	if cfg.Offsite.Enabled() {
		cfg.Encryption.IdentityFile = filepath.Join(dir, offsite.AgeKeyFile)
		cfg.Signing.KeyFile = filepath.Join(dir, offsite.SignKeyFile)
		cfg.Offsite.AllowUnencrypted = false // managed copies are always encrypted
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	for name := range j.Secrets {
		if !envOK(name) {
			return nil, fmt.Errorf("invalid secret name %q (must start with UNTH_SECRET_)", name)
		}
	}
	return cfg, nil
}

func idOK(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func envOK(s string) bool {
	if !strings.HasPrefix(s, "UNTH_SECRET_") || len(s) > 80 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func (a *Agent) decrypt(armored string) (string, error) {
	r, err := age.Decrypt(armorReader(armored), a.Ident)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(r, 64<<10))
	return string(b), err
}

func (a *Agent) addEvent(ev agentproto.Event) {
	ev.ID = connections.RandomToken()[:22]
	ev.At = time.Now().UTC()
	a.mu.Lock()
	a.events = append(a.events, ev)
	if len(a.events) > 500 { // bounded while the control plane is unreachable
		a.events = a.events[len(a.events)-500:]
	}
	a.mu.Unlock()
}

// sync fetches the job set, keeps the jobs that pass the policy and reports
// rejected ones.
func (a *Agent) sync(ctx context.Context) error {
	var jr agentproto.JobsResponse
	if err := a.Client.do(ctx, http.MethodGet, agentproto.PathJobs, nil, &jr); err != nil {
		return err
	}
	var ok []agentproto.Job
	for _, j := range jr.Jobs {
		if _, err := a.jobPolicy(j); err != nil {
			a.addEvent(agentproto.Event{Type: agentproto.EvJobRejected, JobID: j.ID, Summary: map[string]any{"error": err.Error()}})
			continue
		}
		ok = append(ok, j)
	}
	jr.Jobs = ok
	a.mu.Lock()
	a.jobs = jr
	a.next = map[string]time.Time{}
	a.mu.Unlock()
	return writeJSONFile(filepath.Join(Dir(a.DataDir), "jobs.json"), jr) // secrets stay age-encrypted on disk
}

func (a *Agent) findJob(id string) (agentproto.Job, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, j := range a.jobs.Jobs {
		if j.ID == id {
			return j, true
		}
	}
	return agentproto.Job{}, false
}

// heartbeat sends status, flushes events and queues commands.
func (a *Agent) heartbeat(ctx context.Context) error {
	host, _ := os.Hostname()
	hb := agentproto.Heartbeat{Version: buildinfo.Version, Protocol: agentproto.Version, Hostname: host}
	if a.Hooks.Status != nil {
		hb.Docker, hb.FreeBytes = a.Hooks.Status(ctx)
	}
	a.mu.Lock()
	hb.JobsVersion = a.jobs.Version
	for id := range a.running {
		hb.Running = append(hb.Running, id)
	}
	a.mu.Unlock()
	var resp agentproto.HeartbeatResponse
	if err := a.Client.do(ctx, http.MethodPost, agentproto.PathHeartbeat, hb, &resp); err != nil {
		return err
	}
	if resp.IntervalSec >= 5 && resp.IntervalSec <= 600 {
		a.interval = time.Duration(resp.IntervalSec) * time.Second
	}
	if resp.JobsVersion != "" && resp.JobsVersion != hb.JobsVersion {
		if err := a.sync(ctx); err != nil {
			a.Log.Warn("job sync failed", "error", err)
		}
	}
	for _, c := range resp.Commands {
		switch {
		case c.Type == agentproto.CmdSync:
			_ = a.sync(ctx)
		case c.Type == agentproto.CmdRunJob && idOK(c.JobID):
			a.enqueue(work{cmdID: c.ID, jobID: c.JobID})
		case c.Type == agentproto.CmdVerify && idOK(c.BackupID):
			a.enqueue(work{cmdID: c.ID, backupID: c.BackupID})
		default:
			a.addEvent(agentproto.Event{Type: agentproto.EvCommandRejected, CommandID: c.ID, Summary: map[string]any{"error": "unknown or invalid command"}})
		}
	}
	return a.flush(ctx)
}

func (a *Agent) flush(ctx context.Context) error {
	a.mu.Lock()
	evs := a.events
	a.events = nil
	a.mu.Unlock()
	if len(evs) == 0 {
		return nil
	}
	if err := a.Client.do(ctx, http.MethodPost, agentproto.PathEvents, agentproto.Events{Events: evs}, nil); err != nil {
		a.mu.Lock()
		a.events = append(evs, a.events...)
		a.mu.Unlock()
		return err
	}
	return nil
}

func (a *Agent) enqueue(w work) {
	a.mu.Lock()
	a.queue = append(a.queue, w)
	a.mu.Unlock()
}

// due queues scheduled jobs whose time has come (local schedule: backups run
// even when the control plane is unreachable).
func (a *Agent) due(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, j := range a.jobs.Jobs {
		if j.Disabled || j.Schedule == "" {
			continue
		}
		daily, h, m, every, err := agentproto.ParseSchedule(j.Schedule)
		if err != nil {
			continue
		}
		n, ok := a.next[j.ID]
		if !ok {
			a.next[j.ID] = nextTime(now, daily, h, m, every)
			continue
		}
		if !now.Before(n) {
			a.queue = append(a.queue, work{jobID: j.ID})
			a.next[j.ID] = nextTime(now, daily, h, m, every)
		}
	}
}

func nextTime(now time.Time, daily bool, h, m int, every time.Duration) time.Time {
	if !daily {
		return now.Add(every)
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !t.After(now) {
		t = time.Date(now.Year(), now.Month(), now.Day()+1, h, m, 0, 0, now.Location())
	}
	return t
}

// process runs queued work one item at a time.
func (a *Agent) process(ctx context.Context) {
	for {
		a.mu.Lock()
		if len(a.queue) == 0 || ctx.Err() != nil {
			a.mu.Unlock()
			return
		}
		w := a.queue[0]
		a.queue = a.queue[1:]
		a.mu.Unlock()
		if w.backupID != "" {
			sum := a.Hooks.Verify(ctx, w.backupID)
			a.addEvent(agentproto.Event{Type: agentproto.EvVerifyCompleted, CommandID: w.cmdID, Summary: sum})
			continue
		}
		a.runJob(ctx, w)
	}
}

func (a *Agent) runJob(ctx context.Context, w work) {
	j, ok := a.findJob(w.jobID)
	if !ok || j.Disabled {
		a.addEvent(agentproto.Event{Type: agentproto.EvCommandRejected, CommandID: w.cmdID, JobID: w.jobID,
			Summary: map[string]any{"error": "unknown or disabled job"}})
		return
	}
	cfg, err := a.jobPolicy(j)
	if err != nil {
		a.addEvent(agentproto.Event{Type: agentproto.EvJobRejected, CommandID: w.cmdID, JobID: j.ID, Summary: map[string]any{"error": err.Error()}})
		return
	}
	// Secrets are decrypted only for the duration of the run and handed to
	// the job through its *_env names. Runs are strictly sequential.
	var set []string
	for name, enc := range j.Secrets {
		v, err := a.decrypt(enc)
		if err != nil {
			a.addEvent(agentproto.Event{Type: agentproto.EvJobRejected, CommandID: w.cmdID, JobID: j.ID,
				Summary: map[string]any{"error": "secret " + name + " cannot be decrypted with this agent's key"}})
			unsetAll(set)
			return
		}
		_ = os.Setenv(name, v)
		set = append(set, name)
	}
	a.mu.Lock()
	a.running[j.ID] = true
	a.mu.Unlock()
	if cfg.Offsite.Type == config.OffsiteGDrive {
		// Managed Drive copies use the Drive connected in the control plane:
		// it hands out short-lived access tokens; the refresh token stays there.
		ctx = offsite.WithDriveBroker(ctx, a.driveToken)
	}
	sum := a.Hooks.RunJob(ctx, cfg)
	unsetAll(set)
	a.mu.Lock()
	delete(a.running, j.ID)
	a.mu.Unlock()
	a.addEvent(agentproto.Event{Type: agentproto.EvRunCompleted, CommandID: w.cmdID, JobID: j.ID, Summary: sum})
}

// driveToken asks the control plane for an access token of one of the
// organization's Drive connections.
func (a *Agent) driveToken(ctx context.Context, connection string) (*offsite.BrokeredToken, error) {
	var r agentproto.DriveTokenResponse
	if err := a.Client.do(ctx, http.MethodPost, agentproto.PathDriveToken, agentproto.DriveTokenRequest{Connection: connection}, &r); err != nil {
		return nil, err
	}
	if a.Secrets != nil {
		a.Secrets(r.AccessToken)
	}
	return &offsite.BrokeredToken{AccessToken: r.AccessToken, Expiry: r.ExpiresAt, Account: r.Account,
		FolderID: r.FolderID, FolderName: r.FolderName}, nil
}

func unsetAll(names []string) {
	for _, n := range names {
		_ = os.Unsetenv(n)
	}
}

// Run is the agent loop: heartbeat, commands, local schedule. once=true does
// one cycle (tests, `unth agent run --once`).
func (a *Agent) Run(ctx context.Context, once bool) error {
	for {
		if err := a.heartbeat(ctx); err != nil {
			a.Log.Warn("control plane unreachable; local schedule continues", "error", a.Redact(err.Error()))
			if once {
				return err
			}
		}
		a.due(time.Now())
		a.process(ctx)
		if err := a.flush(ctx); err != nil && once {
			return err
		}
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.interval):
		}
	}
}
