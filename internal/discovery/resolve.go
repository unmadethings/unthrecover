package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/unmadethings/unthrecover/internal/docker"
)

// ContainerDB is what a PostgreSQL container tells about itself.
type ContainerDB struct {
	Host     string // an address of the container this host can reach
	Database string
	User     string
	Password string // POSTGRES_PASSWORD; kept in memory, never persisted
	Image    string
}

// ErrNoPassword: the container has no POSTGRES_PASSWORD in its environment.
var ErrNoPassword = errors.New("the container has no POSTGRES_PASSWORD (or uses POSTGRES_PASSWORD_FILE); set a password for the job instead")

// ResolveContainer reads a PostgreSQL container's address, database, user and
// password from Docker. Only official-image variables are used:
// POSTGRES_USER (default postgres), POSTGRES_DB (default: the user),
// POSTGRES_PASSWORD. It runs at every backup, so a new address after a
// restart is picked up.
func ResolveContainer(ctx context.Context, dc *docker.Client, name string) (*ContainerDB, error) {
	if _, err := dc.Negotiate(ctx); err != nil {
		return nil, err
	}
	info, err := dc.InspectContainer(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("container %s: %w", name, err)
	}
	if docker.ClassifyImage(info.Config.Image) != "postgres" {
		return nil, fmt.Errorf("container %s does not run a PostgreSQL image (%s)", name, info.Config.Image)
	}
	if !info.State.Running {
		return nil, fmt.Errorf("container %s is %s; it must be running", name, info.State.Status)
	}
	db := &ContainerDB{User: "postgres", Image: info.Config.Image}
	var pw string
	var hasPw bool
	for _, kv := range info.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "POSTGRES_USER":
			if safeDBIdent.MatchString(v) {
				db.User = v
			}
		case "POSTGRES_DB":
			if safeDBIdent.MatchString(v) {
				db.Database = v
			}
		case "POSTGRES_PASSWORD":
			pw, hasPw = v, true
		}
	}
	if db.Database == "" {
		db.Database = db.User // the official image's default
	}
	if !hasPw || pw == "" {
		return nil, ErrNoPassword
	}
	db.Password = pw
	db.Host = containerAddress(info)
	if db.Host == "" {
		return nil, fmt.Errorf("container %s has no IP address on a Docker network", name)
	}
	return db, nil
}

// containerAddress picks an IPv4 address of the container: user-defined
// networks first (sorted, stable), then the default bridge.
func containerAddress(info *docker.ContainerInfo) string {
	var names []string
	for n := range info.NetworkSettings.Networks {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		bi, bj := names[i] == "bridge", names[j] == "bridge"
		if bi != bj {
			return !bi
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		if ip := net.ParseIP(info.NetworkSettings.Networks[n].IPAddress); ip != nil && ip.To4() != nil {
			return ip.String()
		}
	}
	return ""
}
