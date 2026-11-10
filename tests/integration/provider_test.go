//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Real-provider tests. They run only when credentials are provided (in CI via
// the "Provider tests" workflow and repository secrets) and touch nothing but
// a random prefix that they delete at the end.

// runProviderFlow: keys → unth run with offsite → REMOTE_VERIFIED → list →
// fetch on an empty data dir → isolated restore VERIFIED.
func runProviderFlow(t *testing.T, e *env, offsiteYAML string) {
	t.Helper()
	keyDir := filepath.Join(t.TempDir(), "keys")
	if r := e.run("keys", "generate", "--dir", keyDir); r.code != 0 {
		t.Fatal(r.stderr)
	}
	cfg := e.config(cfgOpts{queries: true, extra: fmt.Sprintf("encryption:\n  identity_file: %s/age.key\nsigning:\n  key_file: %s/sign.key\n%s",
		keyDir, keyDir, offsiteYAML)})
	r := e.run("run", "--config", cfg, "--format", "json")
	var sum struct {
		Outcome  string `json:"outcome"`
		BackupID string `json:"backup_id"`
		Offsite  struct{ Status, Target string }
	}
	if json.Unmarshal([]byte(r.stdout), &sum) != nil || r.code != 0 || sum.Offsite.Status != "REMOTE_VERIFIED" {
		t.Fatalf("run: %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	t.Logf("copy REMOTE_VERIFIED on %s", sum.Offsite.Target)
	if r := e.run("offsite", "list", "--config", cfg); r.code != 0 || !strings.Contains(r.stdout, sum.BackupID) {
		t.Fatalf("list: %s %s", r.stdout, r.stderr)
	}
	dr := newEnv(t)
	dr.extra = e.extra
	if r := dr.run("offsite", "fetch", "--config", cfg, sum.BackupID); r.code != 0 || !strings.Contains(r.stdout, "signature: valid") {
		t.Fatalf("fetch: %d %s %s", r.code, r.stdout, r.stderr)
	}
	rep, vr := dr.verify(sum.BackupID)
	if vr.code != 0 || rep.FinalStatus != "VERIFIED" {
		t.Fatalf("restore test of the fetched copy: %d %s", vr.code, vr.stderr)
	}
	t.Log("fetched copy restored in an isolated container: VERIFIED")
}

// TestOffsiteRealS3 runs against a real S3-compatible provider, e.g. Google
// Cloud Storage with HMAC keys:
//
//	UNTH_TEST_S3_ENDPOINT=https://storage.googleapis.com UNTH_TEST_S3_BUCKET=...
//	UNTH_TEST_S3_ACCESS_KEY=... UNTH_TEST_S3_SECRET_KEY=... [UNTH_TEST_S3_REGION=auto]
func TestOffsiteRealS3(t *testing.T) {
	endpoint, bucket := os.Getenv("UNTH_TEST_S3_ENDPOINT"), os.Getenv("UNTH_TEST_S3_BUCKET")
	ak, sk := os.Getenv("UNTH_TEST_S3_ACCESS_KEY"), os.Getenv("UNTH_TEST_S3_SECRET_KEY")
	if endpoint == "" || bucket == "" || ak == "" || sk == "" {
		t.Skip("NOT RUN: real S3 provider credentials not provided")
	}
	region := os.Getenv("UNTH_TEST_S3_REGION")
	prefix := "unth-ci/" + randHex(6)
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: u.Scheme == "https", Region: region})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // only this test's random prefix
		ctx := context.Background()
		for o := range cl.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix + "/", Recursive: true}) {
			if o.Err == nil {
				_ = cl.RemoveObject(ctx, bucket, o.Key, minio.RemoveObjectOptions{})
			}
		}
	})
	e := newEnv(t)
	e.extra = []string{"IT_REAL_S3_KEY=" + ak, "IT_REAL_S3_SECRET=" + sk}
	y := fmt.Sprintf("offsite:\n  type: s3\n  endpoint: %s\n  bucket: %s\n  prefix: %s\n  access_key_env: IT_REAL_S3_KEY\n  secret_key_env: IT_REAL_S3_SECRET\n", endpoint, bucket, prefix)
	if region != "" {
		y += "  region: " + region + "\n"
	}
	runProviderFlow(t, e, y)
	if strings.Contains(e.logs.String(), sk) {
		t.Fatal("secret key in unth output")
	}
}

// TestOffsiteRealRclone runs against a real rclone remote, e.g. Google Drive:
//
//	UNTH_TEST_RCLONE_REMOTE=gdrive:unth-ci UNTH_TEST_RCLONE_CONFIG=/path/rclone.conf
func TestOffsiteRealRclone(t *testing.T) {
	remote, conf := os.Getenv("UNTH_TEST_RCLONE_REMOTE"), os.Getenv("UNTH_TEST_RCLONE_CONFIG")
	if remote == "" || conf == "" {
		t.Skip("NOT RUN: real rclone remote not provided")
	}
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("NOT RUN: rclone not installed")
	}
	folder := strings.TrimSuffix(remote, "/") + "/" + randHex(6)
	if strings.HasSuffix(remote, ":") {
		folder = remote + randHex(6)
	}
	t.Cleanup(func() { // only this test's random folder
		_ = exec.Command("rclone", "--config", conf, "purge", "--", folder).Run()
	})
	e := newEnv(t)
	runProviderFlow(t, e, fmt.Sprintf("offsite:\n  type: rclone\n  remote: %q\n  rclone_config: %s\n", folder, conf))
}
