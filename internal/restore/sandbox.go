// Package restore implements the Restore Engine. In this release restores
// ONLY target freshly created, isolated, disposable PostgreSQL containers.
// Restoring into an existing (production) database or volume is not
// implemented and cannot be triggered by any command.
package restore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/docker"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/postgres"
	"github.com/unmadethings/unthrecover/internal/security"
)

// TargetDatabase is the database created inside the sandbox.
const TargetDatabase = "unth_verify"

// Capabilities required by the official postgres image entrypoint to
// initialise its data directory and drop to the postgres user. Everything
// else is dropped.
var sandboxCaps = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID"}

// Sandbox is an isolated PostgreSQL instance created for one verification run.
type Sandbox struct {
	RunID       string
	Image       string
	NetworkName string
	VolumeName  string
	ContainerID string
	Params      postgres.ConnParams
	DockerInfo  *docker.VersionInfo
	ServerMajor int

	dc  *docker.Client
	log *slog.Logger
}

// Labels returns the labels applied to every sandbox resource.
func Labels(runID, backupID, role string) map[string]string {
	return map[string]string{
		docker.LabelManaged: "true",
		docker.LabelRun:     runID,
		docker.LabelBackup:  backupID,
		docker.LabelRole:    role,
	}
}

// DefaultImage returns the restore image for a source major version. The
// Debian-based image is used because it ships the common libc locales.
func DefaultImage(major int) string { return fmt.Sprintf("postgres:%d", major) }

// Create provisions network, volume and container and waits until PostgreSQL
// accepts connections. On error, already-created resources are removed.
func Create(ctx context.Context, dc *docker.Client, runID, backupID string, major int, v config.Verification,
	red *security.Redactor, log *slog.Logger) (sb *Sandbox, err error) {
	if err := security.ValidateID(runID); err != nil {
		return nil, err
	}
	info, err := dc.Negotiate(ctx)
	if err != nil {
		return nil, failure.New(failure.DockerUnavailable, "Docker Engine is not reachable", err).
			WithHint("start the Docker daemon or fix DOCKER_HOST / socket permissions")
	}
	image := v.RestoreImage
	if image == "" {
		image = DefaultImage(major)
	}
	sb = &Sandbox{RunID: runID, Image: image, DockerInfo: info, dc: dc, log: log, ServerMajor: major,
		NetworkName: "unth-net-" + runID, VolumeName: "unth-vol-" + runID}
	defer func() {
		if err != nil {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			_, _ = Cleanup(cctx, dc, runID)
		}
	}()

	if err := ensureImage(ctx, dc, image, v.ImagePull); err != nil {
		return sb, err
	}
	if _, err := dc.CreateNetwork(ctx, sb.NetworkName, Labels(runID, backupID, "network")); err != nil {
		return sb, envErr("create isolated network", err)
	}
	if err := dc.CreateVolume(ctx, sb.VolumeName, Labels(runID, backupID, "data")); err != nil {
		return sb, envErr("create sandbox volume", err)
	}
	password := security.RandomHex(24)
	red.Add(password)
	id, err := dc.CreateContainer(ctx, docker.ContainerSpec{
		Name:  "unth-pg-" + runID,
		Image: image,
		Env: []string{
			"POSTGRES_PASSWORD=" + password,
			"POSTGRES_USER=postgres",
			"POSTGRES_DB=" + TargetDatabase,
		},
		Labels:      Labels(runID, backupID, "postgres"),
		NetworkName: sb.NetworkName,
		VolumeName:  sb.VolumeName,
		VolumeDest:  "/var/lib/postgresql/data",
		MemoryBytes: v.MemoryMB << 20,
		NanoCPUs:    int64(v.CPUs * 1e9),
		PidsLimit:   v.PidsLimit,
		CapAdd:      sandboxCaps,
		ShmSize:     256 << 20,
	})
	if err != nil {
		return sb, envErr("create sandbox container", err)
	}
	sb.ContainerID = id
	if err := dc.StartContainer(ctx, id); err != nil {
		return sb, envErr("start sandbox container", err)
	}
	ci, err := dc.InspectContainer(ctx, id)
	if err != nil {
		return sb, envErr("inspect sandbox container", err)
	}
	ip := ci.NetworkSettings.Networks[sb.NetworkName].IPAddress
	if ip == "" {
		return sb, envErr("sandbox container has no address on the isolated network", nil)
	}
	sb.Params = postgres.ConnParams{Host: ip, Port: 5432, Database: TargetDatabase, User: "postgres",
		Password: password, SSLMode: "disable", ConnectTimeout: 5}
	if err := sb.waitReady(ctx); err != nil {
		return sb, err
	}
	return sb, nil
}

func envErr(msg string, err error) error {
	var ae *docker.AccessError
	if errors.As(err, &ae) {
		return failure.New(failure.DockerUnavailable, msg, err).
			WithHint("start the Docker daemon or fix DOCKER_HOST / socket permissions")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return failure.New(failure.EnvironmentFailed, msg, err)
}

// EnsureImage makes sure an image is present according to the pull policy.
func EnsureImage(ctx context.Context, dc *docker.Client, image, policy string) error {
	return ensureImage(ctx, dc, image, policy)
}

func ensureImage(ctx context.Context, dc *docker.Client, image, policy string) error {
	exists, err := dc.ImageExists(ctx, image)
	if err != nil {
		return envErr("inspect restore image", err)
	}
	switch {
	case policy == config.PullAlways, policy == config.PullIfMissing && !exists:
		if err := dc.PullImage(ctx, image); err != nil {
			return envErr("pull restore image "+image, err)
		}
	case !exists:
		return failure.New(failure.EnvironmentFailed,
			fmt.Sprintf("restore image %s is not present and image_pull is %q", image, policy), nil).
			WithHint("pull the image manually or set verification.image_pull: if-missing")
	}
	return nil
}

// waitReady polls until PostgreSQL accepts TCP connections and answers a query.
func (sb *Sandbox) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ci, err := sb.dc.InspectContainer(ctx, sb.ContainerID)
		if err != nil {
			return envErr("inspect sandbox container", err)
		}
		if !ci.State.Running {
			logs, _ := sb.dc.ContainerLogs(ctx, sb.ContainerID, 20)
			return failure.New(failure.EnvironmentFailed,
				fmt.Sprintf("sandbox PostgreSQL exited (code %d, oom=%v): %s", ci.State.ExitCode, ci.State.OOMKilled, logs), nil)
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		conn, err := postgres.Connect(cctx, sb.Params)
		if err == nil {
			var one int
			qerr := conn.QueryRow(cctx, "SELECT 1").Scan(&one)
			_ = conn.Close(cctx)
			cancel()
			if qerr == nil && one == 1 {
				return nil
			}
		} else {
			cancel()
		}
		if time.Now().After(deadline) {
			return failure.New(failure.EnvironmentFailed, "sandbox PostgreSQL did not become ready within 3m", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// CleanupResult lists removed resources and errors.
type CleanupResult struct {
	Removed []string `json:"removed"`
	Errors  []string `json:"errors,omitempty"`
}

// Cleanup removes every resource that carries BOTH the managed label and the
// given run label. Nothing else is ever touched.
func Cleanup(ctx context.Context, dc *docker.Client, runID string) (*CleanupResult, error) {
	if err := security.ValidateID(runID); err != nil {
		return nil, err
	}
	res := &CleanupResult{}
	filters := []string{docker.LabelManaged + "=true", docker.LabelRun + "=" + runID}

	containers, err := dc.ListContainers(ctx, filters...)
	if err != nil {
		res.Errors = append(res.Errors, "list containers: "+err.Error())
	}
	for _, c := range containers {
		if c.Labels[docker.LabelManaged] != "true" || c.Labels[docker.LabelRun] != runID {
			continue // defence in depth: never trust the filter alone
		}
		if err := dc.RemoveContainer(ctx, c.ID); err != nil && !errors.Is(err, docker.ErrNotFound) {
			res.Errors = append(res.Errors, "remove container "+c.ID[:12]+": "+err.Error())
		} else {
			res.Removed = append(res.Removed, "container:"+c.ID[:12])
		}
	}
	vols, err := dc.ListVolumes(ctx, filters...)
	if err != nil {
		res.Errors = append(res.Errors, "list volumes: "+err.Error())
	}
	for _, v := range vols {
		if v.Labels[docker.LabelManaged] != "true" || v.Labels[docker.LabelRun] != runID {
			continue
		}
		if err := dc.RemoveVolume(ctx, v.Name); err != nil && !errors.Is(err, docker.ErrNotFound) {
			res.Errors = append(res.Errors, "remove volume "+v.Name+": "+err.Error())
		} else {
			res.Removed = append(res.Removed, "volume:"+v.Name)
		}
	}
	nets, err := dc.ListNetworks(ctx, filters...)
	if err != nil {
		res.Errors = append(res.Errors, "list networks: "+err.Error())
	}
	for _, n := range nets {
		if n.Labels[docker.LabelManaged] != "true" || n.Labels[docker.LabelRun] != runID {
			continue
		}
		if err := dc.RemoveNetwork(ctx, n.ID); err != nil && !errors.Is(err, docker.ErrNotFound) {
			res.Errors = append(res.Errors, "remove network "+n.Name+": "+err.Error())
		} else {
			res.Removed = append(res.Removed, "network:"+n.Name)
		}
	}
	if len(res.Errors) > 0 {
		return res, fmt.Errorf("cleanup incomplete: %v", res.Errors)
	}
	return res, nil
}

// ManagedRunIDs returns run IDs that currently own Docker resources.
func ManagedRunIDs(ctx context.Context, dc *docker.Client) (map[string]bool, error) {
	ids := map[string]bool{}
	f := docker.LabelManaged + "=true"
	cs, err := dc.ListContainers(ctx, f)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		if id := c.Labels[docker.LabelRun]; id != "" {
			ids[id] = true
		}
	}
	vs, err := dc.ListVolumes(ctx, f)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		if id := v.Labels[docker.LabelRun]; id != "" {
			ids[id] = true
		}
	}
	ns, err := dc.ListNetworks(ctx, f)
	if err != nil {
		return nil, err
	}
	for _, n := range ns {
		if id := n.Labels[docker.LabelRun]; id != "" {
			ids[id] = true
		}
	}
	return ids, nil
}
