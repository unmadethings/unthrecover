package restore

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/unmadethings/unthrecover/internal/docker"
)

// TestCleanupOnlyRemovesOwnLabeledResources verifies the cleanup safety rule
// against a fake Engine that IGNORES label filters and returns unrelated
// (production-like) resources: only resources carrying both the managed label
// and the exact run label may be deleted.
func TestCleanupOnlyRemovesOwnLabeledResources(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/version":
			_ = json.NewEncoder(w).Encode(docker.VersionInfo{APIVersion: "1.47"})
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, p)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(p, "/containers/json"):
			_ = json.NewEncoder(w).Encode([]docker.ContainerSummary{
				{ID: "aaaaaaaaaaaaaaaa", Labels: map[string]string{docker.LabelManaged: "true", docker.LabelRun: "vr-target-1"}},
				{ID: "bbbbbbbbbbbbbbbb", Labels: map[string]string{docker.LabelManaged: "true", docker.LabelRun: "vr-other-2"}},
				{ID: "cccccccccccccccc", Labels: map[string]string{"app": "production-db"}},
				{ID: "dddddddddddddddd", Labels: map[string]string{docker.LabelRun: "vr-target-1"}},
			})
		case strings.HasSuffix(p, "/volumes"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Volumes": []docker.Volume{
				{Name: "unth-vol-vr-target-1", Labels: map[string]string{docker.LabelManaged: "true", docker.LabelRun: "vr-target-1"}},
				{Name: "prod-data", Labels: nil},
			}})
		case strings.HasSuffix(p, "/networks"):
			_ = json.NewEncoder(w).Encode([]docker.Network{
				{ID: "n1", Name: "unth-net-vr-target-1", Labels: map[string]string{docker.LabelManaged: "true", docker.LabelRun: "vr-target-1"}},
				{ID: "n2", Name: "prod-net", Labels: map[string]string{docker.LabelManaged: "false", docker.LabelRun: "vr-target-1"}},
			})
		default:
			http.Error(w, "unexpected", 500)
		}
	})
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	t.Setenv("DOCKER_HOST", "unix://"+sock)
	dc, err := docker.NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	res, err := Cleanup(context.Background(), dc, "vr-target-1")
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 3 {
		t.Fatalf("expected exactly 3 deletions, got %v", deleted)
	}
	for _, d := range deleted {
		ok := strings.Contains(d, "/containers/aaaaaaaaaaaaaaaa") || strings.Contains(d, "/volumes/unth-vol-vr-target-1") ||
			strings.HasSuffix(d, "/networks/n1")
		if !ok {
			t.Errorf("deleted a resource that is not ours: %s", d)
		}
	}
	if len(res.Removed) != 3 {
		t.Fatalf("removed list: %v", res.Removed)
	}
	if _, err := Cleanup(context.Background(), dc, "../bad"); err == nil {
		t.Fatal("invalid run id accepted")
	}
}

func TestDefaultImage(t *testing.T) {
	if DefaultImage(16) != "postgres:16" {
		t.Fatal(DefaultImage(16))
	}
}
