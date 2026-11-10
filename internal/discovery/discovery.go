// Package discovery implements read-only inventory of Docker and PostgreSQL
// resources. Nothing in this package modifies system state: it only issues
// GET requests to the Docker Engine and read-only queries to PostgreSQL.
package discovery

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/docker"
	"github.com/unmadethings/unthrecover/internal/postgres"
)

// ContainerInfo is a discovered container.
type ContainerInfo struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Image       string   `json:"image"`
	State       string   `json:"state"`
	Volumes     []string `json:"volumes"`
	Kind        string   `json:"kind"`
	Assessment  string   `json:"backup_assessment"`
	ManagedByUs bool     `json:"managed_by_unth"`
}

// VolumeInfo is a discovered volume.
type VolumeInfo struct {
	Name       string   `json:"name"`
	Driver     string   `json:"driver"`
	UsedBy     []string `json:"used_by"`
	Assessment string   `json:"backup_assessment"`
}

// DockerInventory is the result of Docker discovery.
type DockerInventory struct {
	DockerVersion string          `json:"docker_version"`
	APIVersion    string          `json:"api_version"`
	Containers    []ContainerInfo `json:"containers"`
	Volumes       []VolumeInfo    `json:"volumes"`
}

func classify(image string) string { return docker.ClassifyImage(image) }

// Docker inventories containers and volumes (read-only).
func Docker(ctx context.Context, dc *docker.Client) (*DockerInventory, error) {
	v, err := dc.Negotiate(ctx)
	if err != nil {
		return nil, err
	}
	inv := &DockerInventory{DockerVersion: v.Version, APIVersion: v.APIVersion}
	cs, err := dc.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	usedBy := map[string][]string{}
	dbVolumes := map[string]bool{}
	for _, c := range cs {
		name := strings.TrimPrefix(strings.Join(c.Names, ","), "/")
		ci := ContainerInfo{ID: shortID(c.ID), Name: name, Image: c.Image, State: c.State,
			Kind: classify(c.Image), ManagedByUs: c.Labels[docker.LabelManaged] == "true"}
		for _, m := range c.Mounts {
			if m.Type == "volume" {
				ci.Volumes = append(ci.Volumes, m.Name)
				usedBy[m.Name] = append(usedBy[m.Name], name)
				if ci.Kind != "generic" {
					dbVolumes[m.Name] = true
				}
			}
		}
		switch {
		case ci.ManagedByUs:
			ci.Assessment = "temporary Unth Recover verification resource"
		case ci.Kind == "postgres":
			ci.Assessment = "supported: logical backup with `unth backup postgres` (connect via host/port)"
		case ci.Kind != "generic":
			ci.Assessment = "not supported in this release (" + ci.Kind + ")"
		default:
			ci.Assessment = "no direct backup; back up its volumes with `type: docker-volume`"
		}
		inv.Containers = append(inv.Containers, ci)
	}
	vs, err := dc.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		vi := VolumeInfo{Name: v.Name, Driver: v.Driver, UsedBy: usedBy[v.Name]}
		switch {
		case v.Labels[docker.LabelManaged] == "true":
			vi.Assessment = "temporary Unth Recover verification resource"
		case dbVolumes[v.Name]:
			vi.Assessment = "database data volume: prefer a logical backup; file-level backup only with consistency: stop"
		default:
			vi.Assessment = "supported: `type: docker-volume` (consistency: offline or stop)"
		}
		inv.Volumes = append(inv.Volumes, vi)
	}
	return inv, nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// PostgresReport is the result of PostgreSQL discovery.
type PostgresReport struct {
	Reachable     bool   `json:"reachable"`
	ServerVersion string `json:"server_version,omitempty"`
	ServerMajor   int    `json:"server_major,omitempty"`
	DatabaseSize  int64  `json:"database_size_bytes,omitempty"`
	UserTables    int    `json:"user_tables"`
	InRecovery    bool   `json:"in_recovery"`
	PgDumpVersion string `json:"pg_dump_version,omitempty"`
	Compatible    bool   `json:"compatible"`
	Compatibility string `json:"compatibility"`
	Error         string `json:"error,omitempty"`
}

// Postgres connects (read-only) and evaluates backup compatibility.
func Postgres(ctx context.Context, src config.Source, password string) (*PostgresReport, error) {
	r := &PostgresReport{}
	conn, err := postgres.Connect(ctx, postgres.ConnParams{Host: src.Host, Port: src.Port, Database: src.Database,
		User: src.User, Password: password, SSLMode: src.SSLMode, ConnectTimeout: int(src.ConnectTimeout.D().Seconds())})
	if err != nil {
		return r, err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	r.Reachable = true
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return r, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if r.ServerVersion, r.ServerMajor, err = postgres.ServerVersion(ctx, tx); err != nil {
		return r, err
	}
	_ = tx.QueryRow(ctx, `SELECT pg_database_size(current_database()), pg_is_in_recovery(),
		(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		 WHERE c.relkind='r' AND n.nspname NOT IN ('pg_catalog','information_schema','pg_toast'))`).
		Scan(&r.DatabaseSize, &r.InRecovery, &r.UserTables)
	tool, err := postgres.LookupTool(ctx, "pg_dump")
	if err != nil {
		r.Compatibility = err.Error()
		return r, nil
	}
	r.PgDumpVersion = tool.Version
	if err := postgres.CheckDumpCompatibility(tool, r.ServerMajor); err != nil {
		r.Compatibility = err.Error()
		return r, nil
	}
	r.Compatible = true
	r.Compatibility = "supported: logical backup (pg_dump custom format) with snapshot-consistent fingerprint"
	return r, nil
}
