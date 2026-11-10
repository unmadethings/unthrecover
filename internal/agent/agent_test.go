package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/fakecloud"
	"github.com/unmadethings/unthrecover/pkg/agentproto"
)

const jobYAML = `version: 1
source:
  name: shop
  host: db.internal
  database: shop
  user: backup
  password_env: UNTH_SECRET_DB
`

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func enrolled(t *testing.T, fc *fakecloud.Server, hooks Hooks) (*Agent, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := Enroll(context.Background(), dir, fc.URL, fc.NewToken(), fc.Client())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(dir, hooks, quiet(), func(s string) string { return s })
	if err != nil {
		t.Fatal(err)
	}
	a.Client.HTTP = fc.Client()
	return a, st.AgentID
}

func TestEnrollment(t *testing.T) {
	fc := fakecloud.New()
	defer fc.Close()
	dir := t.TempDir()
	tok := fc.NewToken()
	if _, err := Enroll(context.Background(), dir, "http://cloud.example.com", tok, fc.Client()); err == nil {
		t.Fatal("plain http to a non-loopback control plane accepted")
	}
	st, err := Enroll(context.Background(), dir, fc.URL, tok, fc.Client())
	if err != nil || st.AgentID == "" {
		t.Fatal(err)
	}
	for _, f := range []string{"agent.json", "age.key", "sign.key"} {
		fi, err := os.Stat(filepath.Join(Dir(dir), f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, err, fi)
		}
	}
	if _, err := Enroll(context.Background(), dir, fc.URL, fc.NewToken(), fc.Client()); err == nil {
		t.Fatal("enrolled twice")
	}
	// The token is single-use.
	if _, err := Enroll(context.Background(), t.TempDir(), fc.URL, tok, fc.Client()); err == nil {
		t.Fatal("token reused")
	}
	if len(fc.Agents()) != 1 {
		t.Fatal("agents")
	}
}

func TestManagedJobRun(t *testing.T) {
	fc := fakecloud.New()
	defer fc.Close()
	var secrets []string
	var cfgs []*config.Config
	hooks := Hooks{
		RunJob: func(_ context.Context, cfg *config.Config) map[string]any {
			secrets, cfgs = append(secrets, os.Getenv("UNTH_SECRET_DB")), append(cfgs, cfg)
			return map[string]any{"outcome": "success", "verification_status": "VERIFIED"}
		},
		Verify: func(_ context.Context, id string) map[string]any {
			return map[string]any{"backup_id": id, "final_status": "VERIFIED"}
		},
		Status: func(context.Context) (string, int64) { return "Engine 29", 1 << 30 },
	}
	a, id := enrolled(t, fc, hooks)
	offsiteJob := jobYAML + "offsite:\n  type: gdrive\n  connection: gdrive\n"
	jobs := []agentproto.Job{
		{ID: "job-shop", Name: "shop", Schedule: "daily 03:00", Config: jobYAML},
		{ID: "job-files", Name: "files", Config: offsiteJob},
		{ID: "job-bad-file", Config: strings.Replace(jobYAML, "password_env: UNTH_SECRET_DB", "password_file: /etc/shadow", 1)},
		{ID: "job-bad-key", Config: offsiteJob + "encryption:\n  identity_file: /tmp/attacker.key\n"},
		{ID: "job-bad-secret", Config: jobYAML},
	}
	if err := fc.SetJobs(id, "v1", jobs, map[string]map[string]string{
		"job-shop":       {"UNTH_SECRET_DB": "s3cr3t-pw"},
		"job-bad-secret": {"PATH": "/evil"},
	}); err != nil {
		t.Fatal(err)
	}
	fc.Command(id, agentproto.Command{ID: "c1", Type: agentproto.CmdRunJob, JobID: "job-shop"})
	fc.Command(id, agentproto.Command{ID: "c2", Type: agentproto.CmdVerify, BackupID: "bk-20261110t000000-abcd"})
	fc.Command(id, agentproto.Command{ID: "c3", Type: "rm_rf"})
	fc.Command(id, agentproto.Command{ID: "c4", Type: agentproto.CmdRunJob, JobID: "job-files"})
	if err := a.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 2 || secrets[0] != "s3cr3t-pw" || secrets[1] != "" {
		t.Fatalf("secrets seen by the runs: %q (want the secret only in job-shop)", secrets)
	}
	seenCfg := cfgs[1]
	if os.Getenv("UNTH_SECRET_DB") != "" {
		t.Fatal("secret left in the environment after the run")
	}
	if seenCfg.Encryption.IdentityFile != filepath.Join(Dir(a.DataDir), "age.key") || seenCfg.Signing.KeyFile == "" {
		t.Fatalf("offsite job does not use the agent's own keys: %+v", seenCfg.Encryption)
	}
	got := map[string]int{}
	for _, e := range fc.TakeEvents() {
		got[e.Type+":"+e.JobID+e.CommandID]++
	}
	for _, want := range []string{"run.completed:job-shopc1", "verify.completed:c2", "command.rejected:c3", "run.completed:job-filesc4",
		"job.rejected:job-bad-file", "job.rejected:job-bad-key", "job.rejected:job-bad-secret"} {
		if got[want] != 1 {
			t.Errorf("event %s: %d (all: %v)", want, got[want], got)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(Dir(a.DataDir), "jobs.json"))
	if strings.Contains(string(raw), "s3cr3t-pw") || !strings.Contains(string(raw), "AGE ENCRYPTED FILE") {
		t.Fatal("secret stored in plaintext on disk")
	}
	if hb := fc.Agents()[0].LastHeartbeat; hb.Docker != "Engine 29" || hb.JobsVersion != "" && hb.Protocol != agentproto.Version {
		t.Fatalf("heartbeat %+v", hb)
	}
}

func TestSignatureAndOutage(t *testing.T) {
	fc := fakecloud.New()
	a, _ := enrolled(t, fc, Hooks{Verify: func(context.Context, string) map[string]any { return map[string]any{} }})
	// Another agent's key: rejected.
	_, other := enrolled(t, fc, Hooks{})
	a2 := *a.Client
	a2.State = &State{AgentID: other, URL: a.Client.State.URL}
	if err := a2.do(context.Background(), http.MethodPost, agentproto.PathHeartbeat, agentproto.Heartbeat{}, nil); err == nil {
		t.Fatal("request signed with another agent's key accepted")
	}
	// Clock skew is rejected.
	a3 := *a.Client
	a3.Now = func() time.Time { return time.Now().Add(-time.Hour) }
	if err := a3.do(context.Background(), http.MethodPost, agentproto.PathHeartbeat, agentproto.Heartbeat{}, nil); err == nil {
		t.Fatal("skewed request accepted")
	}
	// Control plane down: events are kept for later.
	a.addEvent(agentproto.Event{Type: agentproto.EvVerifyCompleted})
	fc.Close()
	if err := a.Run(context.Background(), true); err == nil {
		t.Fatal("expected an error with the control plane down")
	}
	if len(a.events) != 1 {
		t.Fatalf("events lost during outage: %d", len(a.events))
	}
}

func TestLocalSchedule(t *testing.T) {
	a := &Agent{next: map[string]time.Time{}, running: map[string]bool{}}
	a.jobs = agentproto.JobsResponse{Jobs: []agentproto.Job{{ID: "j1", Schedule: "every 15m"}, {ID: "j2", Schedule: "daily 03:00"}, {ID: "j3"}}}
	t0 := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)
	a.due(t0)
	if len(a.queue) != 0 {
		t.Fatal("ran on first sight")
	}
	a.due(t0.Add(16 * time.Minute))
	if len(a.queue) != 1 || a.queue[0].jobID != "j1" {
		t.Fatalf("%+v", a.queue)
	}
	a.due(t0.Add(61 * time.Minute)) // 03:01
	ids := []string{}
	for _, w := range a.queue {
		ids = append(ids, w.jobID)
	}
	if strings.Join(ids, ",") != "j1,j1,j2" {
		t.Fatalf("%v", ids)
	}
}
