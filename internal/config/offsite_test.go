package config

import (
	"strings"
	"testing"
	"time"
)

const offsiteBase = valid + `retention:
  keep_last: 7
encryption:
  identity_file: /etc/unth/keys/age.key
signing:
  key_file: /etc/unth/keys/sign.key
`

func TestOffsiteValidAndDefaults(t *testing.T) {
	c, err := Parse([]byte(offsiteBase + `offsite:
  type: s3
  endpoint: https://storage.googleapis.com
  bucket: my-backups
  prefix: /servers/web1/
  access_key_env: GCS_HMAC_ID
  secret_key_env: GCS_HMAC_SECRET
`))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Offsite
	if o.Prefix != "servers/web1" || o.Timeout.D() != 6*time.Hour || o.Retention.KeepLast != 7 {
		t.Fatalf("%+v", o)
	}
	c, err = Parse([]byte(offsiteBase + `offsite:
  type: s3
  bucket: b-1
  access_key_env: A
  secret_key_env: S
  retention:
    keep_daily: 30
`))
	if err != nil || c.Offsite.Endpoint != "https://s3.amazonaws.com" || c.Offsite.Retention.KeepLast != 0 || c.Offsite.Retention.KeepDaily != 30 {
		t.Fatalf("%v %+v", err, c.Offsite)
	}
	if _, err := Parse([]byte(offsiteBase + "offsite:\n  type: rclone\n  remote: \"gdrive:Unth Backups\"\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(offsiteBase + "offsite:\n  type: s3\n  endpoint: http://127.0.0.1:8333\n  bucket: bk1\n  access_key_env: A\n  secret_key_env: S\n  path_style: true\n")); err != nil {
		t.Fatalf("loopback http must be allowed: %v", err)
	}
}

func TestOffsiteRejects(t *testing.T) {
	s3 := "offsite:\n  type: s3\n  bucket: bk1\n  access_key_env: A\n  secret_key_env: S\n"
	cases := map[string]string{
		"no encryption":        valid + s3,
		"http to remote host":  offsiteBase + "offsite:\n  type: s3\n  endpoint: http://minio.internal:9000\n  bucket: bk1\n  access_key_env: A\n  secret_key_env: S\n",
		"endpoint with path":   offsiteBase + "offsite:\n  type: s3\n  endpoint: https://x.example/bucket\n  bucket: bk1\n  access_key_env: A\n  secret_key_env: S\n",
		"endpoint credentials": offsiteBase + "offsite:\n  type: s3\n  endpoint: https://key:secret@x.example\n  bucket: bk1\n  access_key_env: A\n  secret_key_env: S\n",
		"literal secret key":   offsiteBase + "offsite:\n  type: s3\n  bucket: bk1\n  access_key: AKIA\n  secret_key: abc\n",
		"missing key envs":     offsiteBase + "offsite:\n  type: s3\n  bucket: bk1\n",
		"bad bucket":           offsiteBase + "offsite:\n  type: s3\n  bucket: Bad_Bucket\n  access_key_env: A\n  secret_key_env: S\n",
		"prefix traversal":     offsiteBase + s3 + "  prefix: a/../b\n",
		"unknown type":         offsiteBase + "offsite:\n  type: ftp\n",
		"rclone flag remote":   offsiteBase + "offsite:\n  type: rclone\n  remote: --config=/tmp/x:\n",
		"rclone no colon":      offsiteBase + "offsite:\n  type: rclone\n  remote: gdrive\n",
		"rclone with bucket":   offsiteBase + "offsite:\n  type: rclone\n  remote: \"gdrive:\"\n  bucket: bk1\n",
		"bad recipient":        offsiteBase + s3 + "encryption:\n  identity_file: /k\n  recipients: [age1nope]\n",
		"sign key and pub":     valid + "signing:\n  key_file: /a\n  public_key_file: /b\n",
		"timeout too short":    offsiteBase + s3 + "  timeout: 10s\n",
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// allow_unencrypted is an explicit, visible opt-out.
	if _, err := Parse([]byte(valid + s3 + "  allow_unencrypted: true\n")); err != nil {
		t.Fatal(err)
	}
	_, err := Parse([]byte(valid + s3))
	if err == nil || !strings.Contains(err.Error(), "encryption.identity_file is required") {
		t.Fatalf("message: %v", err)
	}
}

func TestOffsiteGDrive(t *testing.T) {
	if _, err := Parse([]byte(offsiteBase + "offsite:\n  type: gdrive\n  connection: gdrive\n")); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"no connection":        offsiteBase + "offsite:\n  type: gdrive\n",
		"bad connection name":  offsiteBase + "offsite:\n  type: gdrive\n  connection: ../x\n",
		"gdrive with bucket":   offsiteBase + "offsite:\n  type: gdrive\n  connection: gdrive\n  bucket: b1x\n",
		"s3 with connection":   offsiteBase + "offsite:\n  type: s3\n  bucket: bk1\n  access_key_env: A\n  secret_key_env: S\n  connection: gdrive\n",
		"gdrive without crypt": valid + "offsite:\n  type: gdrive\n  connection: gdrive\n",
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
