//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unmadethings/unthrecover/internal/fakecloud"
	"github.com/unmadethings/unthrecover/pkg/agentproto"
)

// TestAgentManagedJob: a server enrolls with a control plane, receives a job
// whose database password is encrypted to the server's key, runs it on a
// "Run now" command (real backup + isolated restore) and reports the result;
// a "Verify now" command re-verifies the backup. The password never appears
// in events, logs or files.
func TestAgentManagedJob(t *testing.T) {
	fc := fakecloud.New()
	defer fc.Close()
	e := newEnv(t)
	e.extra = []string{"UNTH_ENROLL_TOKEN=" + fc.NewToken()}
	if r := e.run("agent", "enroll", "--url", fc.URL); r.code != 0 || !strings.Contains(r.stdout, "Enrolled as") {
		t.Fatalf("enroll: %d %s %s", r.code, r.stdout, r.stderr)
	}
	e.extra = nil
	agents := fc.Agents()
	if len(agents) != 1 {
		t.Fatal("agent not registered")
	}
	id := agents[0].ID
	job := fmt.Sprintf(`version: 1
source:
  name: managed-shop
  host: 127.0.0.1
  port: %s
  database: shop
  user: postgres
  password_env: UNTH_SECRET_DB
  sslmode: disable
verification:
  restore_image: %s
`, src.Port, restoreImage)
	if err := fc.SetJobs(id, "v1", []agentproto.Job{{ID: "job-shop", Name: "Shop DB", Schedule: "daily 03:00", Config: job}},
		map[string]map[string]string{"job-shop": {"UNTH_SECRET_DB": src.Password}}); err != nil {
		t.Fatal(err)
	}
	fc.Command(id, agentproto.Command{ID: "cmd-1", Type: agentproto.CmdRunJob, JobID: "job-shop"})
	if r := e.run("agent", "run", "--once"); r.code != 0 {
		t.Fatalf("agent run: %d %s", r.code, r.stderr)
	}
	evs := fc.TakeEvents()
	raw, _ := json.Marshal(evs)
	if len(evs) != 1 || evs[0].Type != agentproto.EvRunCompleted || evs[0].Summary["outcome"] != "success" ||
		evs[0].Summary["verification_status"] != "VERIFIED" {
		t.Fatalf("events: %s", raw)
	}
	backupID, _ := evs[0].Summary["backup_id"].(string)
	fc.Command(id, agentproto.Command{ID: "cmd-2", Type: agentproto.CmdVerify, BackupID: backupID})
	if r := e.run("agent", "run", "--once"); r.code != 0 {
		t.Fatalf("agent run: %d %s", r.code, r.stderr)
	}
	evs2 := fc.TakeEvents()
	raw2, _ := json.Marshal(evs2)
	if len(evs2) != 1 || evs2[0].Type != agentproto.EvVerifyCompleted || evs2[0].Summary["final_status"] != "VERIFIED" {
		t.Fatalf("verify events: %s", raw2)
	}
	jobsFile, _ := os.ReadFile(filepath.Join(e.dataDir, "agent", "jobs.json"))
	for what, s := range map[string]string{"events": string(raw) + string(raw2), "logs": e.logs.String(), "jobs.json": string(jobsFile)} {
		if strings.Contains(s, src.Password) {
			t.Fatalf("database password in %s", what)
		}
	}
	if hb := fc.Agents()[0]; hb.Heartbeats < 2 || hb.LastHeartbeat.Docker == "unreachable" {
		t.Fatalf("heartbeat %+v", hb.LastHeartbeat)
	}
	e.assertNoResources("")
}
