package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.5.1-beta", "v0.5.0-beta", true}, {"v0.5.0-beta", "v0.5.0-beta", false}, {"v0.4.0-beta", "v0.5.0-beta", false},
		{"v0.5.0", "v0.5.0-beta", true}, {"v0.5.0-beta", "v0.5.0", false}, {"v1.0.0", "v0.9.9", true},
		{"v0.5.0-beta.2", "v0.5.0-beta.1", true}, {"v0.5.0-beta.10", "v0.5.0-beta.9", true}, {"v0.5.0", "0.1.0-alpha.dev", true},
	} {
		if got, err := Newer(c.a, c.b); err != nil || got != c.want {
			t.Errorf("Newer(%s, %s) = %v %v", c.a, c.b, got, err)
		}
	}
	if _, err := Newer("latest", "v0.1.0"); err == nil {
		t.Error("invalid version accepted")
	}
}

// release serves SHA256SUMS and one asset like GitHub's download URLs.
func release(t *testing.T, version, name string, body []byte, sums string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + version + "/SHA256SUMS":
			_, _ = fmt.Fprint(w, sums)
		case "/" + version + "/" + name:
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestApplyBinaryAndRequest(t *testing.T) {
	ctx := context.Background()
	body := []byte("#!/bin/sh\necho unth v0.6.0\n")
	sum := sha256.Sum256(body)
	name := "unth_v0.6.0_linux_amd64"
	ts := release(t, "v0.6.0", name, body, hex.EncodeToString(sum[:])+"  "+name+"\n")
	dir := t.TempDir()
	bin := filepath.Join(dir, "unth")
	_ = os.WriteFile(bin, []byte("old"), 0o755) // #nosec G306 -- test binary
	var ran []string
	u := &Updater{Base: ts.URL, HTTP: ts.Client(), Current: "v0.5.0-beta", Arch: "amd64", Method: "binary", Binary: bin,
		Run: func(_ context.Context, name string, args ...string) error {
			ran = append(ran, filepath.Base(name)+" "+strings.Join(args, " "))
			return nil
		}}
	agentDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(agentDir, "update-request"), []byte("v0.6.0\n"), 0o600)
	if err := u.ApplyRequest(ctx, agentDir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(bin); string(got) != string(body) {
		t.Fatalf("binary not replaced: %q", got)
	}
	if len(ran) != 2 || !strings.HasSuffix(ran[0], " version") || ran[1] != "systemctl try-restart unth-agent.service" {
		t.Fatalf("commands %v", ran)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "update-request")); !os.IsNotExist(err) {
		t.Fatal("request not removed")
	}
	var res Result
	raw, _ := os.ReadFile(filepath.Join(agentDir, "update-result.json"))
	if json.Unmarshal(raw, &res) != nil || !res.OK || res.Version != "v0.6.0" {
		t.Fatalf("result %s", raw)
	}
	// Same or older versions are refused; nothing is downloaded or run.
	ran = nil
	u.Current = "v0.6.0"
	if err := u.Apply(ctx, "v0.6.0"); err == nil || len(ran) != 0 {
		t.Fatalf("same version: %v %v", err, ran)
	}
	if err := u.Apply(ctx, "v0.4.0-beta"); err == nil {
		t.Fatal("downgrade accepted")
	}
	// A request with anything but a version is refused.
	for _, bad := range []string{"v0.7.0; rm -rf /", "../../etc/passwd", "https://evil.example/unth", strings.Repeat("v", 200)} {
		_ = os.WriteFile(filepath.Join(agentDir, "update-request"), []byte(bad), 0o600)
		if err := u.ApplyRequest(ctx, agentDir); err == nil {
			t.Errorf("request %q accepted", bad)
		}
	}
}

func TestApplyRejectsBadChecksumsAndPackages(t *testing.T) {
	ctx := context.Background()
	body := []byte("package bytes")
	name := "unthrecover_0.6.0_amd64.deb"
	wrong := strings.Repeat("0", 64)
	ts := release(t, "v0.6.0", name, body, wrong+"  "+name+"\n")
	var ran []string
	u := &Updater{Base: ts.URL, HTTP: ts.Client(), Current: "v0.5.0-beta", Arch: "amd64", Method: "deb",
		Run: func(_ context.Context, name string, args ...string) error { ran = append(ran, name); return nil }}
	if err := u.Apply(ctx, "v0.6.0"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") || len(ran) != 0 {
		t.Fatalf("bad checksum: %v %v", err, ran)
	}
	// Correct checksum: the package manager installs it, then the agent restarts.
	sum := sha256.Sum256(body)
	ts2 := release(t, "v0.6.0", name, body, hex.EncodeToString(sum[:])+"  "+name+"\n")
	u.Base, u.HTTP = ts2.URL, ts2.Client()
	if err := u.Apply(ctx, "v0.6.0"); err != nil || len(ran) != 2 || ran[0] != "dpkg" || ran[1] != "systemctl" {
		t.Fatalf("deb: %v %v", err, ran)
	}
	// No entry in SHA256SUMS for the asset.
	ts3 := release(t, "v0.6.0", name, body, wrong+"  other-file\n")
	u.Base, u.HTTP, ran = ts3.URL, ts3.Client(), nil
	if err := u.Apply(ctx, "v0.6.0"); err == nil || len(ran) != 0 {
		t.Fatalf("missing entry: %v", err)
	}
}
