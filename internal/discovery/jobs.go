package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/unmadethings/unthrecover/internal/docker"
)

// Platforms recognized from container labels.
const (
	PlatformCoolify = "coolify"
	PlatformDokploy = "dokploy"
	PlatformCompose = "compose"
	PlatformDocker  = "docker"
)

// Candidate is what discovery needs to know about one database container.
// Env holds only POSTGRES_DB / POSTGRES_USER (never secrets).
type Candidate struct {
	Name     string
	Image    string
	State    string
	Labels   map[string]string
	Env      map[string]string
	Networks []string
}

// JobSuggestion is a ready-to-review job configuration for one database.
type JobSuggestion struct {
	Container    string   `json:"container"`
	Platform     string   `json:"platform"`
	Workload     string   `json:"workload"`
	Host         string   `json:"host"`
	Database     string   `json:"database"`
	User         string   `json:"user"`
	PasswordEnv  string   `json:"password_env"`
	PasswordHint string   `json:"password_hint"`
	RestoreImage string   `json:"restore_image,omitempty"`
	Network      string   `json:"network,omitempty"`
	Notes        []string `json:"notes,omitempty"`
	YAML         string   `json:"yaml"`
}

var (
	nonName     = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
	imageRef    = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]{0,254}$`)
	safeDBIdent = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.$-]{0,62}$`)
)

// DetectPlatform names the tool that created the container.
func DetectPlatform(labels map[string]string, networks []string) string {
	for k := range labels {
		if strings.HasPrefix(k, "coolify.") {
			return PlatformCoolify
		}
		if strings.HasPrefix(k, "dokploy") {
			return PlatformDokploy
		}
	}
	for _, n := range networks {
		if n == "dokploy-network" {
			return PlatformDokploy
		}
		if n == "coolify" {
			return PlatformCoolify
		}
	}
	if labels["com.docker.compose.project"] != "" {
		return PlatformCompose
	}
	return PlatformDocker
}

// Suggest derives a job for a PostgreSQL container. It is pure.
func Suggest(c Candidate) JobSuggestion {
	plat := DetectPlatform(c.Labels, c.Networks)
	s := JobSuggestion{Container: c.Name, Platform: plat, Database: "postgres", User: "postgres"}
	if v := c.Env["POSTGRES_DB"]; safeDBIdent.MatchString(v) {
		s.Database = v
	}
	if v := c.Env["POSTGRES_USER"]; safeDBIdent.MatchString(v) {
		s.User = v
		if c.Env["POSTGRES_DB"] == "" {
			s.Database = v // the official image defaults the database to the user name
		}
	}

	// Host: a name resolvable on a user-defined network shared with unth.
	nets := userNetworks(c.Networks)
	s.Host = c.Name
	switch plat {
	case PlatformDokploy:
		if svc := c.Labels["com.docker.swarm.service.name"]; svc != "" {
			s.Host = svc
		}
	case PlatformCompose:
		if svc := c.Labels["com.docker.compose.service"]; svc != "" {
			s.Host = svc
		}
	}
	if len(nets) > 0 {
		s.Network = preferredNetwork(plat, nets)
	} else {
		s.Notes = append(s.Notes, "the container is only on the default bridge network: put it on a user-defined network (or publish a port) so unth can reach it")
	}

	base := c.Name
	if v := c.Labels["coolify.name"]; v != "" {
		base = v
	} else if v := c.Labels["com.docker.compose.service"]; v != "" && c.Labels["com.docker.compose.project"] != "" {
		base = c.Labels["com.docker.compose.project"] + "-" + v
	}
	s.Workload = workloadName(base)
	s.PasswordEnv = strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(s.Workload)) + "_PASSWORD"
	if s.PasswordEnv[0] >= '0' && s.PasswordEnv[0] <= '9' {
		s.PasswordEnv = "DB_" + s.PasswordEnv
	}
	switch plat {
	case PlatformCoolify:
		s.PasswordHint = "Coolify → the database resource → General → Password"
	case PlatformDokploy:
		s.PasswordHint = "Dokploy → the database → General → Database Password"
	default:
		s.PasswordHint = "the value of POSTGRES_PASSWORD in the container's environment / compose file"
	}
	if img := strings.ToLower(c.Image); imageRef.MatchString(img) && !strings.HasPrefix(img, "sha256:") {
		s.RestoreImage = img // same image as the source: same extensions (PostGIS, TimescaleDB, ...)
	}
	if c.State != "running" {
		s.Notes = append(s.Notes, "the container is "+c.State+"; backups need it running")
	}
	if s.Network != "" {
		s.Notes = append(s.Notes, fmt.Sprintf("unth must be on the Docker network %q (Compose: add it to the scheduler's networks; host install: unth reaches it through a published port instead)", s.Network))
	}
	s.YAML = renderJob(s)
	return s
}

func userNetworks(nets []string) []string {
	var out []string
	for _, n := range nets {
		if n != "bridge" && n != "host" && n != "none" && n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func preferredNetwork(plat string, nets []string) string {
	want := map[string]string{PlatformCoolify: "coolify", PlatformDokploy: "dokploy-network"}[plat]
	for _, n := range nets {
		if n == want {
			return n
		}
	}
	return nets[0]
}

func workloadName(s string) string {
	s = strings.Trim(nonName.ReplaceAllString(strings.TrimPrefix(s, "/"), "-"), "-._")
	if s == "" {
		s = "postgres"
	}
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-._")
	}
	return s
}

func renderJob(s JobSuggestion) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Suggested by `unth discover jobs` from container %q (%s). Review before use.\n", s.Container, s.Platform)
	fmt.Fprintf(&b, "# Password: set %s to the database password (%s).\n", s.PasswordEnv, s.PasswordHint)
	b.WriteString("# unth never reads the password from the container.\n")
	for _, n := range s.Notes {
		fmt.Fprintf(&b, "# Note: %s\n", n)
	}
	fmt.Fprintf(&b, "version: 1\nsource:\n  name: %s\n  host: %s\n  port: 5432\n  database: %s\n  user: %s\n  password_env: %s\n",
		s.Workload, s.Host, s.Database, s.User, s.PasswordEnv)
	b.WriteString("  sslmode: prefer\n")
	if s.RestoreImage != "" {
		fmt.Fprintf(&b, "verification:\n  restore_image: %s   # same image as the source (same extensions)\n", s.RestoreImage)
	}
	b.WriteString("retention:\n  keep_last: 7\n")
	b.WriteString("# Offsite copies (Google Cloud Storage, S3, Google Drive): see docs/OFFSITE.md\n")
	return b.String()
}

// SuggestJobs inspects running and stopped PostgreSQL containers (read-only)
// and returns one suggestion per container.
func SuggestJobs(ctx context.Context, dc *docker.Client) ([]JobSuggestion, error) {
	if _, err := dc.Negotiate(ctx); err != nil {
		return nil, err
	}
	cs, err := dc.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	out := []JobSuggestion{}
	for _, c := range cs {
		if c.Labels[docker.LabelManaged] == "true" { // unth's own sandboxes
			continue
		}
		if docker.ClassifyImage(c.Image) != "postgres" {
			continue
		}
		info, err := dc.InspectContainer(ctx, c.ID)
		if err != nil {
			continue
		}
		cand := Candidate{Name: strings.TrimPrefix(strings.Join(c.Names, ","), "/"), Image: c.Image, State: c.State,
			Labels: c.Labels, Env: map[string]string{}}
		for _, kv := range info.Config.Env {
			k, v, _ := strings.Cut(kv, "=")
			if k == "POSTGRES_DB" || k == "POSTGRES_USER" { // only non-secret settings are kept
				cand.Env[k] = v
			}
		}
		for n := range info.NetworkSettings.Networks {
			cand.Networks = append(cand.Networks, n)
		}
		out = append(out, Suggest(cand))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Workload < out[j].Workload })
	return out, nil
}

// WriteJobs writes each suggestion to dir/<workload>.yaml. Existing files are
// never overwritten; their names are returned in skipped.
func WriteJobs(dir string, js []JobSuggestion) (written, skipped []string, err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, nil, err
	}
	for _, j := range js {
		p := filepath.Join(dir, j.Workload+".yaml")
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640) // #nosec G304 -- operator-chosen directory, validated name
		if os.IsExist(err) {
			skipped = append(skipped, p)
			continue
		}
		if err != nil {
			return written, skipped, err
		}
		_, werr := f.WriteString(j.YAML)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			return written, skipped, fmt.Errorf("write %s: %v %v", p, werr, cerr)
		}
		written = append(written, p)
	}
	return written, skipped, nil
}
