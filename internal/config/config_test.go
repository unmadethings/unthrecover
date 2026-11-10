package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `
version: 1
source:
  name: shop-db
  host: 127.0.0.1
  database: shop
  user: reader
  password_env: SHOP_PW
verification:
  queries:
    - name: q1
      sql: SELECT 1
`

func TestParseValidAndDefaults(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Source.Port != 5432 || c.Source.SSLMode != "prefer" || c.Source.Type != "postgres" {
		t.Fatalf("defaults not applied: %+v", c.Source)
	}
	if c.Verification.ImagePull != PullIfMissing || c.Verification.Timeout.D() != 30*time.Minute {
		t.Fatalf("verification defaults: %+v", c.Verification)
	}
	if c.Backup.MinFreeBytes <= 0 {
		t.Fatal("min_free_bytes default")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"plaintext password": strings.Replace(valid, "password_env: SHOP_PW", "password: hunter2", 1),
		"unknown key":        valid + "\nfoo: bar\n",
		"bad port":           strings.Replace(valid, "host: 127.0.0.1", "host: 127.0.0.1\n  port: 70000", 1),
		"missing name":       strings.Replace(valid, "name: shop-db", "name: \"\"", 1),
		"bad name":           strings.Replace(valid, "name: shop-db", "name: \"shop db;rm\"", 1),
		"bad type":           strings.Replace(valid, "host: 127.0.0.1", "host: 127.0.0.1\n  type: mysql", 1),
		"both secrets":       strings.Replace(valid, "password_env: SHOP_PW", "password_env: SHOP_PW\n  password_file: /x", 1),
		"bad env name":       strings.Replace(valid, "SHOP_PW", "1BAD-NAME", 1),
		"bad sslmode":        strings.Replace(valid, "host: 127.0.0.1", "host: 127.0.0.1\n  sslmode: maybe", 1),
		"bad duration":       valid + "backup:\n  timeout: soon\n",
		"bad image":          valid[:strings.Index(valid, "verification:")] + "verification:\n  restore_image: \"postgres:16; rm -rf /\"\n",
		"bad pull":           valid[:strings.Index(valid, "verification:")] + "verification:\n  image_pull: sometimes\n",
		"tiny memory":        valid[:strings.Index(valid, "verification:")] + "verification:\n  memory_mb: 16\n",
		"empty query":        strings.Replace(valid, "sql: SELECT 1", "sql: \"  \"", 1),
		"dup query":          valid + "    - name: q1\n      sql: SELECT 2\n",
		"version":            strings.Replace(valid, "version: 1", "version: 9", 1),
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	_, err := Parse([]byte(cases["plaintext password"]))
	if err == nil || !strings.Contains(err.Error(), "password_env") {
		t.Errorf("plaintext password error should point to password_env: %v", err)
	}
}

func TestResolvePassword(t *testing.T) {
	s := Source{PasswordEnv: "UNTH_TEST_PW_X"}
	if _, err := s.ResolvePassword(); err == nil {
		t.Fatal("unset env must error")
	}
	t.Setenv("UNTH_TEST_PW_X", "abc123")
	if v, err := s.ResolvePassword(); err != nil || v != "abc123" {
		t.Fatalf("got %q %v", v, err)
	}
	f := filepath.Join(t.TempDir(), "pw")
	_ = os.WriteFile(f, []byte("filepw\n"), 0o600)
	s = Source{PasswordFile: f}
	if v, err := s.ResolvePassword(); err != nil || v != "filepw" {
		t.Fatalf("got %q %v", v, err)
	}
	red := s.Redacted()
	for _, v := range red {
		if v == "filepw" {
			t.Fatal("Redacted() contains the secret")
		}
	}
}

func TestLoadMissingAndHuge(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("missing file must error")
	}
	big := filepath.Join(t.TempDir(), "big.yaml")
	_ = os.WriteFile(big, make([]byte, 2<<20), 0o600)
	if _, err := Load(big); err == nil {
		t.Fatal("oversized config must error")
	}
}

func TestRetentionAndNotifications(t *testing.T) {
	good := valid + `retention:
  keep_last: 7
  keep_daily: 7
  max_age: 720h
notifications:
  - name: ops
    type: webhook
    url_env: OPS_HOOK
    secret_env: OPS_SECRET
  - name: chat
    type: slack
    url_env: SLACK_HOOK
    on: always
`
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Retention.Enabled() || c.Notify[0].On != OnFailure || c.Notify[0].Timeout.D() != 10*time.Second {
		t.Fatalf("defaults: %+v %+v", c.Retention, c.Notify)
	}
	bad := map[string]string{
		"negative keep":   valid + "retention:\n  keep_last: -1\n",
		"short max_age":   valid + "retention:\n  max_age: 10m\n",
		"url literal":     valid + "notifications:\n  - name: a\n    type: webhook\n    url: https://x\n",
		"missing url_env": valid + "notifications:\n  - name: a\n    type: webhook\n",
		"bad type":        valid + "notifications:\n  - name: a\n    type: email\n    url_env: X\n",
		"slack secret":    valid + "notifications:\n  - name: a\n    type: slack\n    url_env: X\n    secret_env: Y\n",
		"bad on":          valid + "notifications:\n  - name: a\n    type: webhook\n    url_env: X\n    on: sometimes\n",
		"dup name":        valid + "notifications:\n  - name: a\n    type: webhook\n    url_env: X\n  - name: a\n    type: slack\n    url_env: Y\n",
		"long timeout":    valid + "notifications:\n  - name: a\n    type: webhook\n    url_env: X\n    timeout: 5m\n",
	}
	for name, raw := range bad {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if (Retention{}).Enabled() {
		t.Fatal("empty retention enabled")
	}
}
