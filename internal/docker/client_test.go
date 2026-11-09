package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeEngine is a minimal Docker Engine API stub over a Unix socket.
type fakeEngine struct {
	mu      sync.Mutex
	deleted []string
}

func startFake(t *testing.T, fe *fakeEngine) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(VersionInfo{Version: "99.0", APIVersion: "1.99", MinAPIVersion: "1.40", Os: "linux"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/containers/json"):
			// The filter is deliberately ignored: the client must re-check labels itself.
			_ = json.NewEncoder(w).Encode([]ContainerSummary{
				{ID: "aaaaaaaaaaaaaaaa", Labels: map[string]string{LabelManaged: "true", LabelRun: "vr-target-1"}},
				{ID: "bbbbbbbbbbbbbbbb", Labels: map[string]string{LabelManaged: "true", LabelRun: "vr-other-2"}},
				{ID: "cccccccccccccccc", Labels: map[string]string{"com.example.prod": "true"}},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/volumes"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Volumes": []Volume{
				{Name: "unth-vol-vr-target-1", Labels: map[string]string{LabelManaged: "true", LabelRun: "vr-target-1"}},
				{Name: "prod-data", Labels: map[string]string{}},
			}})
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/networks"):
			_ = json.NewEncoder(w).Encode([]Network{
				{ID: "n1", Name: "unth-net-vr-target-1", Labels: map[string]string{LabelManaged: "true", LabelRun: "vr-target-1"}},
				{ID: "n2", Name: "prod-net", Labels: map[string]string{LabelRun: "vr-target-1"}}, // missing managed label
			})
		case r.Method == http.MethodDelete:
			fe.mu.Lock()
			fe.deleted = append(fe.deleted, p)
			fe.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(p, "/images/missing"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such image"}`))
		case strings.HasSuffix(p, "/_ping"):
			_, _ = w.Write([]byte("OK"))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"unexpected ` + r.Method + " " + p + `"}`))
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "unix://" + sock
}

func TestNegotiateCapsVersion(t *testing.T) {
	t.Setenv("DOCKER_HOST", startFake(t, &fakeEngine{}))
	c, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Negotiate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.apiVersion != maxAPIVersion {
		t.Fatalf("api version %s, want %s", c.apiVersion, maxAPIVersion)
	}
	ok, err := c.ImageExists(context.Background(), "missing:1")
	if err != nil || ok {
		t.Fatalf("ImageExists missing: %v %v", ok, err)
	}
}

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{{"1.44", "1.44", 0}, {"1.9", "1.44", -1}, {"1.51", "1.44", 1}, {"2.0", "1.99", 1}}
	for _, c := range cases {
		if got := compareVersion(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s = %d", c.a, c.b, got)
		}
	}
}

func TestAccessErrorIsDescriptive(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "absent.sock"))
	c, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	err = c.Ping(context.Background())
	var ae *AccessError
	if !errors.As(err, &ae) {
		t.Fatalf("expected AccessError, got %v", err)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("message not descriptive: %v", err)
	}
}

func TestRejectsTCPHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.1:2375")
	if _, err := NewFromEnv(); err == nil {
		t.Fatal("unauthenticated tcp DOCKER_HOST accepted")
	}
}

func TestDemux(t *testing.T) {
	frame := func(stream byte, s string) []byte {
		n := len(s)
		return append([]byte{stream, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, s...)
	}
	data := append(frame(1, "hello "), frame(2, "world")...)
	if got := demux(strings.NewReader(string(data))); got != "hello world" {
		t.Fatalf("demux = %q", got)
	}
}
