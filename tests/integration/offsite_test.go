//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type s3Server struct {
	endpoint, access, secret, bucket string
	client                           *minio.Client
}

// startS3 runs an S3-compatible server (SeaweedFS by default) with
// credential checking enabled and creates a bucket.
func startS3(t *testing.T) *s3Server {
	t.Helper()
	image := envOr("UNTH_TEST_S3_IMAGE", "chrislusf/seaweedfs:3.97")
	if err := ensureImage(image); err != nil {
		t.Skipf("NOT RUN: S3 test server image %s unavailable: %v", image, err)
	}
	s := &s3Server{access: "UNTHIT" + strings.ToUpper(randHex(6)), secret: randHex(20), bucket: "unth-it-" + randHex(3)}
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o755)
	conf := fmt.Sprintf(`{"identities":[{"name":"unth","credentials":[{"accessKey":"%s","secretKey":"%s"}],"actions":["Admin","Read","Write","List","Tagging"]}]}`, s.access, s.secret)
	if err := os.WriteFile(filepath.Join(dir, "s3.json"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "unth-it-s3-" + randHex(3)
	out, err := exec.Command("docker", "run", "-d", "--name", name, "--label", "io.unthrecover.test="+sessionLabel,
		"-p", "127.0.0.1::8333", "-v", dir+":/cfg:ro", "--memory", "1g", image,
		"server", "-dir=/data", "-s3", "-s3.config=/cfg/s3.json", "-volume.max=0", "-master.volumeSizeLimitMB=64").CombinedOutput()
	if err != nil {
		t.Fatalf("start S3 server: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() })
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		p, _ := exec.Command("docker", "port", name, "8333").Output()
		if f := strings.SplitN(strings.TrimSpace(strings.Split(string(p), "\n")[0]), ":", 2); len(f) == 2 {
			s.endpoint = "127.0.0.1:" + f[1]
			c, err := minio.New(s.endpoint, &minio.Options{Creds: credentials.NewStaticV4(s.access, s.secret, ""), BucketLookup: minio.BucketLookupPath})
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err = c.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{})
				cancel()
				if err == nil {
					s.client = c
					return s
				}
			}
		}
		time.Sleep(time.Second)
	}
	logs, _ := exec.Command("docker", "logs", "--tail", "40", name).CombinedOutput()
	t.Fatalf("S3 server did not become ready:\n%s", logs)
	return nil
}

func (s *s3Server) objects(t *testing.T, prefix string) map[string][]byte {
	t.Helper()
	ctx := context.Background()
	out := map[string][]byte{}
	for o := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			t.Fatal(o.Err)
		}
		obj, err := s.client.GetObject(ctx, s.bucket, o.Key, minio.GetObjectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(obj)
		obj.Close()
		out[o.Key] = b
	}
	return out
}

// TestOffsiteS3EndToEnd: unth run copies every verified backup to S3
// (encrypted, signed), proves the copy, applies offsite retention; a new
// machine fetches a copy and restores it in an isolated container; tampered
// copies and wrong credentials never pass.
func TestOffsiteS3EndToEnd(t *testing.T) {
	s3 := startS3(t)
	e := newEnv(t)
	keyDir := filepath.Join(t.TempDir(), "keys")
	if r := e.run("keys", "generate", "--dir", keyDir); r.code != 0 || !strings.Contains(r.stdout, "age1") {
		t.Fatalf("keys generate: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := e.run("keys", "generate", "--dir", keyDir); r.code == 0 {
		t.Fatal("keys generate overwrote existing keys")
	}
	e.extra = []string{"IT_S3_KEY=" + s3.access, "IT_S3_SECRET=" + s3.secret}
	offsiteYAML := fmt.Sprintf(`retention:
  keep_last: 5
encryption:
  identity_file: %s/age.key
signing:
  key_file: %s/sign.key
offsite:
  type: s3
  endpoint: http://%s
  bucket: %s
  prefix: it/server1
  path_style: true
  access_key_env: IT_S3_KEY
  secret_key_env: IT_S3_SECRET
  retention:
    keep_last: 2
`, keyDir, keyDir, s3.endpoint, s3.bucket)
	cfg := e.config(cfgOpts{queries: true, extra: offsiteYAML})

	var ids []string
	for i := 0; i < 3; i++ {
		r := e.run("run", "--config", cfg, "--format", "json")
		var sum struct {
			Outcome  string `json:"outcome"`
			BackupID string `json:"backup_id"`
			Offsite  struct {
				Status, Target, Location string
				Encrypted, Signed        bool
				Pruned                   []string `json:"pruned_backup_ids"`
			} `json:"offsite"`
		}
		if err := json.Unmarshal([]byte(r.stdout), &sum); err != nil || r.code != 0 || sum.Outcome != "success" ||
			sum.Offsite.Status != "REMOTE_VERIFIED" || !sum.Offsite.Encrypted || !sum.Offsite.Signed {
			t.Fatalf("run %d: code %d %v\n%s\n%s", i, r.code, err, r.stdout, r.stderr)
		}
		if i == 2 && len(sum.Offsite.Pruned) != 1 {
			t.Fatalf("offsite retention keep_last 2 should prune 1 copy, got %v", sum.Offsite.Pruned)
		}
		ids = append(ids, sum.BackupID)
	}
	objs := s3.objects(t, "it/server1/it-shop/")
	if len(objs) != 2*4 {
		t.Fatalf("expected 2 copies x 4 objects, got %d", len(objs))
	}
	for k, v := range objs {
		for _, plain := range []string{"orders", "billing", "invoices", "CREATE TABLE", "127.0.0.1"} {
			if bytes.Contains(v, []byte(plain)) && !strings.HasSuffix(k, "copy.json") {
				t.Fatalf("plaintext %q in %s", plain, k)
			}
		}
	}
	if _, ok := objs["it/server1/it-shop/"+ids[0]+"/copy.json"]; ok {
		t.Fatal("oldest copy should have been pruned")
	}
	r := e.run("offsite", "list", "--config", cfg, "--format", "json")
	var list []struct {
		BackupID  string `json:"backup_id"`
		Encrypted bool   `json:"encrypted"`
		Signed    bool   `json:"signed"`
		Error     string `json:"error"`
	}
	if json.Unmarshal([]byte(r.stdout), &list) != nil || len(list) != 2 || list[0].BackupID != ids[2] || !list[0].Signed || list[0].Error != "" {
		t.Fatalf("offsite list: %s %s", r.stdout, r.stderr)
	}
	if r := e.run("offsite", "check", "--config", cfg, ids[2]); r.code != 0 || !strings.Contains(r.stdout, "REMOTE_VERIFIED") {
		t.Fatalf("offsite check: %d %s %s", r.code, r.stdout, r.stderr)
	}

	// Disaster recovery on a "new machine": empty data dir, same job file and keys.
	dr := newEnv(t)
	dr.extra = e.extra
	r = dr.run("offsite", "fetch", "--config", cfg, ids[2])
	if r.code != 0 || !strings.Contains(r.stdout, "signature: valid") {
		t.Fatalf("fetch: %d %s %s", r.code, r.stdout, r.stderr)
	}
	rep, vr := dr.verify(ids[2])
	if vr.code != 0 || rep.FinalStatus != "VERIFIED" {
		t.Fatalf("restore test of the fetched copy: %d %s %s", vr.code, vr.stdout, vr.stderr)
	}
	if r := dr.run("offsite", "fetch", "--config", cfg, ids[2]); r.code == 0 {
		t.Fatal("fetching over an existing local backup must be refused")
	}

	// Tamper with the newest artifact object in the bucket.
	key := "it/server1/it-shop/" + ids[2] + "/artifact.dump.age"
	data := objs[key]
	data[len(data)/2] ^= 0xff
	if _, err := s3.client.PutObject(context.Background(), s3.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	r = e.run("offsite", "check", "--config", cfg, ids[2])
	if r.code != 3 || !strings.Contains(r.stdout, "DECRYPTION_FAILED") {
		t.Fatalf("tampered copy: %d %s %s", r.code, r.stdout, r.stderr)
	}
	dr2 := newEnv(t)
	dr2.extra = e.extra
	if r := dr2.run("offsite", "fetch", "--config", cfg, ids[2]); r.code == 0 {
		t.Fatal("tampered copy fetched")
	}

	// Wrong secret: backup is verified locally, offsite fails -> exit 5, nothing pruned.
	before := e.run("backup", "list", "--format", "json").stdout
	e.extra = []string{"IT_S3_KEY=" + s3.access, "IT_S3_SECRET=wrong-" + randHex(8)}
	r = e.run("run", "--config", cfg)
	if r.code != 5 || !strings.Contains(r.stdout, "failed at offsite") || !strings.Contains(r.stdout, "nothing was pruned") {
		t.Fatalf("wrong credentials: %d %s %s", r.code, r.stdout, r.stderr)
	}
	after := e.run("backup", "list", "--format", "json").stdout
	if strings.Count(after, "EXPIRED") != strings.Count(before, "EXPIRED") {
		t.Fatal("a backup was pruned although the offsite copy failed")
	}

	for _, secret := range []string{s3.secret, src.Password} {
		if strings.Contains(e.logs.String(), secret) || strings.Contains(dr.logs.String(), secret) {
			t.Fatal("secret in unth output")
		}
	}
	e.assertNoResources("")
}

// TestOffsiteRcloneRun: the same job flow through rclone (the path used for
// Google Drive, OneDrive, Dropbox), here with a local-storage rclone remote.
func TestOffsiteRcloneRun(t *testing.T) {
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("NOT RUN: rclone not installed")
	}
	e := newEnv(t)
	keyDir := filepath.Join(t.TempDir(), "keys")
	if r := e.run("keys", "generate", "--dir", keyDir); r.code != 0 {
		t.Fatal(r.stderr)
	}
	store := t.TempDir()
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	_ = os.WriteFile(conf, []byte("[itdrive]\ntype = local\n"), 0o600)
	cfg := e.config(cfgOpts{extra: fmt.Sprintf(`encryption:
  identity_file: %s/age.key
offsite:
  type: rclone
  remote: "itdrive:%s/Unth Backups"
  rclone_config: %s
`, keyDir, store, conf)})
	r := e.run("run", "--config", cfg)
	if r.code != 0 || !strings.Contains(r.stdout, "Offsite:       REMOTE_VERIFIED rclone:itdrive:") {
		t.Fatalf("run: %d %s %s", r.code, r.stdout, r.stderr)
	}
	r = e.run("offsite", "list", "--config", cfg)
	if r.code != 0 || !strings.Contains(r.stdout, "complete") {
		t.Fatalf("list: %s %s", r.stdout, r.stderr)
	}
	e.assertNoResources("")
}
