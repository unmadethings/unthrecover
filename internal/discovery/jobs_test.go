package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unmadethings/unthrecover/internal/config"
)

func TestSuggestPlatforms(t *testing.T) {
	cases := []struct {
		name string
		c    Candidate
		want JobSuggestion
	}{
		{"coolify", Candidate{Name: "k8s0wc4kgc4s0kog", Image: "postgres:16-alpine", State: "running",
			Labels:   map[string]string{"coolify.managed": "true", "coolify.name": "Shop DB", "coolify.type": "database"},
			Env:      map[string]string{"POSTGRES_DB": "shop", "POSTGRES_USER": "shopuser"},
			Networks: []string{"coolify"}},
			JobSuggestion{Platform: PlatformCoolify, Workload: "Shop-DB", Host: "k8s0wc4kgc4s0kog", Database: "shop", User: "shopuser",
				PasswordEnv: "SHOP_DB_PASSWORD", Network: "coolify", RestoreImage: "postgres:16-alpine"}},
		{"dokploy", Candidate{Name: "billing-db-xyz.1.abcdef", Image: "postgis/postgis:16-3.4", State: "running",
			Labels:   map[string]string{"com.docker.swarm.service.name": "billing-db-xyz"},
			Env:      map[string]string{"POSTGRES_USER": "billing"},
			Networks: []string{"dokploy-network", "ingress"}},
			JobSuggestion{Platform: PlatformDokploy, Host: "billing-db-xyz", Database: "billing", User: "billing",
				Network: "dokploy-network", RestoreImage: "postgis/postgis:16-3.4"}},
		{"compose", Candidate{Name: "myapp-db-1", Image: "postgres:15", State: "exited",
			Labels:   map[string]string{"com.docker.compose.project": "myapp", "com.docker.compose.service": "db"},
			Networks: []string{"myapp_default"}},
			JobSuggestion{Platform: PlatformCompose, Workload: "myapp-db", Host: "db", Database: "postgres", User: "postgres",
				PasswordEnv: "MYAPP_DB_PASSWORD", Network: "myapp_default"}},
		{"plain bridge", Candidate{Name: "pg", Image: "postgres", State: "running", Networks: []string{"bridge"}},
			JobSuggestion{Platform: PlatformDocker, Workload: "pg", Host: "pg", Network: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Suggest(tc.c)
			w := tc.want
			check := func(field, got, want string) {
				if want != "" && got != want {
					t.Errorf("%s = %q, want %q", field, got, want)
				}
			}
			check("platform", s.Platform, w.Platform)
			check("workload", s.Workload, w.Workload)
			check("host", s.Host, w.Host)
			check("database", s.Database, w.Database)
			check("user", s.User, w.User)
			check("password_env", s.PasswordEnv, w.PasswordEnv)
			check("restore_image", s.RestoreImage, w.RestoreImage)
			if s.Network != w.Network {
				t.Errorf("network = %q, want %q", s.Network, w.Network)
			}
			// Every suggestion must be a valid job configuration as written.
			cfg, err := config.Parse([]byte(s.YAML))
			if err != nil {
				t.Fatalf("suggested YAML does not validate: %v\n%s", err, s.YAML)
			}
			if cfg.Source.PasswordEnv != s.PasswordEnv || cfg.Source.Host != s.Host {
				t.Fatalf("yaml/fields mismatch")
			}
			for _, line := range strings.Split(s.YAML, "\n") {
				if k := strings.TrimSpace(line); strings.HasPrefix(k, "password:") || strings.HasPrefix(k, "password_file:") {
					t.Fatalf("a password key must never be generated: %q", line)
				}
			}
		})
	}
	if s := Suggest(Candidate{Name: "x", Image: "postgres", State: "exited"}); len(s.Notes) < 2 {
		t.Fatalf("expected notes for stopped container on no network: %v", s.Notes)
	}
}

func TestSuggestSanitizesUntrustedLabels(t *testing.T) {
	s := Suggest(Candidate{Name: "db", Image: "postgres:16", State: "running",
		Labels: map[string]string{"coolify.name": "../../etc\nversion: 2\npassword: x"},
		Env:    map[string]string{"POSTGRES_DB": "shop\nsource:", "POSTGRES_USER": "a b"},
	})
	if strings.ContainsAny(s.Workload, "/\n ") || s.Database != "postgres" || s.User != "postgres" {
		t.Fatalf("%+v", s)
	}
	if _, err := config.Parse([]byte(s.YAML)); err != nil {
		t.Fatalf("%v\n%s", err, s.YAML)
	}
}

func TestWriteJobsNeverOverwrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jobs")
	js := []JobSuggestion{Suggest(Candidate{Name: "pg", Image: "postgres:16", State: "running", Networks: []string{"app"}})}
	w, sk, err := WriteJobs(dir, js)
	if err != nil || len(w) != 1 || len(sk) != 0 {
		t.Fatalf("%v %v %v", w, sk, err)
	}
	_ = os.WriteFile(w[0], []byte("mine"), 0o640)
	w, sk, err = WriteJobs(dir, js)
	if err != nil || len(w) != 0 || len(sk) != 1 {
		t.Fatalf("%v %v %v", w, sk, err)
	}
	if b, _ := os.ReadFile(sk[0]); string(b) != "mine" {
		t.Fatal("existing job overwritten")
	}
}
