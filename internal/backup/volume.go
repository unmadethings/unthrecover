package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/unmadethings/unthrecover/internal/buildinfo"
	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/docker"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/manifest"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/repo"
	"github.com/unmadethings/unthrecover/internal/restore"
	"github.com/unmadethings/unthrecover/internal/security"
	"github.com/unmadethings/unthrecover/internal/volume"
)

// TypeDockerVolume is the backup type identifier for volume backups.
const TypeDockerVolume = "docker-volume"

// Run dispatches on the source type.
func (e *Engine) Run(ctx context.Context, cfg *config.Config) (*metadata.Backup, error) {
	if cfg.Source.Type == config.TypeDockerVolume {
		return e.RunVolume(ctx, cfg)
	}
	return e.RunPostgres(ctx, cfg)
}

// RunVolume creates a file-level backup of a Docker volume.
//
// Consistency policies:
//   - offline (default): refuse if any running container mounts the volume;
//   - stop: stop exactly the running containers that mount the volume, take the
//     archive, then start them again (also on failure). Downtime = archive time.
func (e *Engine) RunVolume(ctx context.Context, cfg *config.Config) (*metadata.Backup, error) {
	if e.Docker == nil {
		return nil, errors.New("docker client not configured")
	}
	src := cfg.Source
	srcJSON, _ := json.Marshal(src.Redacted())
	b := &metadata.Backup{ID: security.NewID("bk"), WorkloadID: src.Name, BackupType: TypeDockerVolume,
		Status: metadata.BackupPending, CreatedAt: time.Now().UTC(), SourceJSON: string(srcJSON), PID: os.Getpid()}
	if err := e.Store.InsertBackup(ctx, b); err != nil {
		return nil, fmt.Errorf("record backup: %w", err)
	}
	e.audit(ctx, "backup.start", b.ID, "started", fmt.Sprintf("workload=%s volume=%s consistency=%s", src.Name, src.Volume, src.Consistency))

	tmpPath, _ := e.Repo.TmpFile(b.ID)
	var finalDir string
	err := e.runVolume(ctx, cfg, b, tmpPath, &finalDir)

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if dc, derr := e.Docker(); derr == nil {
		_, _ = restore.Cleanup(cctx, dc, b.ID) // helper container
	}
	var restartErr *restartError
	if err != nil && !errors.As(err, &restartErr) {
		_ = os.Remove(tmpPath)
		if finalDir != "" {
			_ = os.RemoveAll(finalDir)
		}
		cat := failure.CategoryOf(err)
		b.Status = metadata.BackupFailed
		if cat == failure.Cancelled {
			b.Status = metadata.BackupCanceled
		}
		b.ErrorCategory, b.ErrorMessage = string(cat), e.Redactor.RedactError(err)
		b.ArtifactPath, b.ManifestPath, b.PID = "", "", 0
		now := time.Now().UTC()
		b.CompletedAt = &now
		if uerr := e.Store.UpdateBackup(cctx, b); uerr != nil {
			e.Log.Error("record backup failure", "backup_id", b.ID, "error", uerr)
		}
		e.audit(cctx, "backup.finish", b.ID, "failed", "category="+string(cat))
		return b, err
	}
	e.audit(cctx, "backup.finish", b.ID, "success", fmt.Sprintf("size=%d sha256=%s", b.SizeBytes, b.SHA256))
	return b, err // err may be a restartError: backup is good, but containers were not restarted
}

// restartError reports containers that could not be started again. The
// backup itself is valid when this error is returned.
type restartError struct{ failed []string }

func (r *restartError) Error() string {
	return "backup created, but these containers could not be restarted: " + strings.Join(r.failed, ", ")
}

func (e *Engine) runVolume(ctx context.Context, cfg *config.Config, b *metadata.Backup, tmpPath string, finalDir *string) (retErr error) {
	src := cfg.Source
	dc, err := e.Docker()
	if err != nil {
		return failure.New(failure.DockerUnavailable, "Docker client", err)
	}
	info, err := dc.Negotiate(ctx)
	if err != nil {
		return failure.New(failure.DockerUnavailable, "Docker Engine is not reachable", err).
			WithHint("start the Docker daemon or fix DOCKER_HOST / socket permissions")
	}
	b.ToolVersion = "docker " + info.Version
	if _, err := dc.InspectVolume(ctx, src.Volume); err != nil {
		if errors.Is(err, docker.ErrNotFound) {
			return failure.New(failure.SourceNotFound, fmt.Sprintf("Docker volume %q does not exist", src.Volume), nil).
				WithHint("check source.volume (`unth discover docker` lists volumes)")
		}
		return failure.New(failure.DockerUnavailable, "inspect volume", err)
	}
	free, err := repo.FreeBytes(e.Repo.Root)
	if err == nil && free < cfg.Backup.MinFreeBytes {
		return failure.New(failure.InsufficientStorage,
			fmt.Sprintf("only %d bytes free in %s; backup.min_free_bytes requires %d", free, e.Repo.Root, cfg.Backup.MinFreeBytes), nil)
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Backup.Timeout.D())
	defer cancel()
	b.Status = metadata.BackupRunning
	if err := e.Store.UpdateBackup(ctx, b); err != nil {
		return err
	}

	users, err := dc.ContainersUsingVolume(ctx, src.Volume)
	if err != nil {
		return failure.New(failure.DockerUnavailable, "list containers using the volume", err)
	}
	var running []docker.ContainerSummary
	var warnings []string
	for _, c := range users {
		if c.Labels[docker.LabelManaged] == "true" {
			continue
		}
		name := strings.TrimPrefix(strings.Join(c.Names, ","), "/")
		if kind := docker.ClassifyImage(c.Image); kind != "generic" {
			warnings = append(warnings, fmt.Sprintf("volume is used by %s container %q: a file-level copy is only consistent while it is stopped; prefer a logical backup", kind, name))
		}
		if c.State == "running" || c.State == "restarting" || c.State == "paused" {
			running = append(running, c)
		}
	}
	if len(running) > 0 && src.Consistency == config.ConsistencyOffline {
		names := make([]string, 0, len(running))
		for _, c := range running {
			names = append(names, strings.TrimPrefix(strings.Join(c.Names, ","), "/"))
		}
		return failure.New(failure.ConsistencyUnsafe,
			fmt.Sprintf("volume %q is in use by running containers %v; copying files that are being written is not a consistent backup", src.Volume, names), nil).
			WithHint("stop them first, or set source.consistency: stop to let unth stop and restart them around the backup")
	}

	if err := restore.EnsureImage(ctx, dc, src.HelperImage, cfg.Verification.ImagePull); err != nil {
		return err
	}

	if len(running) > 0 { // consistency: stop
		var stopped []docker.ContainerSummary
		defer func() {
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
			defer rcancel()
			var failed []string
			for i := len(stopped) - 1; i >= 0; i-- {
				c := stopped[i]
				name := strings.TrimPrefix(strings.Join(c.Names, ","), "/")
				if err := dc.StartContainer(rctx, c.ID); err != nil {
					failed = append(failed, name)
					e.audit(rctx, "backup.restart_container", b.ID, "failed", "container="+name+" error="+err.Error())
				} else {
					e.audit(rctx, "backup.restart_container", b.ID, "success", "container="+name)
				}
			}
			if len(failed) > 0 && retErr == nil {
				retErr = &restartError{failed: failed}
			}
		}()
		for _, c := range running {
			name := strings.TrimPrefix(strings.Join(c.Names, ","), "/")
			e.audit(ctx, "backup.stop_container", b.ID, "started", "container="+name)
			if err := dc.StopContainer(ctx, c.ID, int(src.StopTimeout.D().Seconds())); err != nil {
				return failure.New(failure.EnvironmentFailed, "stop container "+name, err)
			}
			stopped = append(stopped, c)
		}
		warnings = append(warnings, fmt.Sprintf("%d container(s) were stopped during the backup (consistency: stop)", len(stopped)))
	}

	labels := restore.Labels(b.ID, b.ID, "volume-snapshot")
	hid, err := dc.CreateHelper(ctx, docker.HelperSpec{Name: "unth-snap-" + b.ID, Image: src.HelperImage,
		Labels: labels, Volume: src.Volume, ReadOnly: true})
	if err != nil {
		return failure.New(failure.EnvironmentFailed, "create snapshot helper container", err)
	}
	stream, err := dc.ArchiveFrom(ctx, hid, "/"+volume.Root)
	if err != nil {
		return failure.New(failure.BackupFailed, "read volume archive", err)
	}
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- internal path
	if err != nil {
		_ = stream.Close()
		return failure.New(failure.BackupFailed, "create temp artifact", err)
	}
	hasher := sha256.New()
	cw := &countingHashWriter{w: f, h: hasher}
	tee := io.TeeReader(io.LimitReader(stream, volume.DefaultLimits.MaxTotalSize+(1<<30)), cw)
	fm, scanErr := volume.Scan(tee, volume.DefaultLimits)
	if scanErr == nil {
		_, scanErr = io.Copy(io.Discard, tee) // trailing tar padding
	}
	_ = stream.Close()
	syncErr, closeErr := f.Sync(), f.Close()
	if cw.werr != nil {
		if failure.CategoryOf(cw.werr) == failure.InsufficientStorage {
			return failure.New(failure.InsufficientStorage, "writing backup artifact failed: disk full", cw.werr)
		}
		return failure.New(failure.BackupFailed, "write artifact", cw.werr)
	}
	if scanErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failure.New(failure.BackupFailed, "volume archive is not valid", scanErr)
	}
	if syncErr != nil || closeErr != nil {
		return failure.New(failure.InsufficientStorage, "flush artifact", errors.Join(syncErr, closeErr))
	}

	dir, err := e.Repo.BackupDir(b.ID)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return failure.New(failure.BackupFailed, "create backup directory", err)
	}
	*finalDir = dir
	artifactAbs := filepath.Join(dir, manifest.VolumeArtifactName)
	if err := os.Rename(tmpPath, artifactAbs); err != nil {
		return failure.New(failure.BackupFailed, "move artifact into repository", err)
	}
	now := time.Now().UTC()
	sum := hex.EncodeToString(hasher.Sum(nil))
	ver := cfg.Verification
	ver.RestoreImage = src.HelperImage // the verification sandbox uses the same helper image
	verJSON, _ := json.Marshal(ver)
	fmJSON, _ := json.Marshal(fm)
	m := &manifest.Manifest{
		FormatVersion: manifest.FormatVersion, Tool: "unth-recover", ToolVersion: buildinfo.Version,
		BackupID: b.ID, WorkloadID: b.WorkloadID, BackupType: TypeDockerVolume, CreatedAt: b.CreatedAt,
		SnapshotAt: now, Source: src.Redacted(), DumpToolVersion: b.ToolVersion,
		Artifact: manifest.Artifact{Path: manifest.VolumeArtifactName, Format: "docker-archive-tar",
			SizeBytes: cw.n, SHA256: sum, TOCItems: len(fm.Entries)},
		FingerprintDigest: fm.Digest(), VolumeFiles: fm, Verification: ver,
		Integrity: manifest.DefaultIntegrity(), Warnings: warnings,
	}
	manifestAbs := filepath.Join(dir, manifest.FileName)
	if err := manifest.Write(manifestAbs, m); err != nil {
		return failure.New(failure.BackupFailed, "write manifest", err)
	}
	syncDir(dir)
	relA, _ := e.Repo.Rel(artifactAbs)
	relM, _ := e.Repo.Rel(manifestAbs)
	b.Status, b.CompletedAt, b.SnapshotAt = metadata.BackupCreated, &now, &now
	b.ArtifactPath, b.ManifestPath, b.SizeBytes, b.SHA256 = relA, relM, cw.n, sum
	b.SourceFingerprint, b.FingerprintJSON, b.VerificationJSON = fm.Digest(), string(fmJSON), string(verJSON)
	b.PID = 0
	if len(warnings) > 0 {
		b.ErrorMessage = "warnings: " + strings.Join(warnings, "; ")
	}
	if err := e.Store.UpdateBackup(context.WithoutCancel(ctx), b); err != nil {
		return fmt.Errorf("record backup: %w", err)
	}
	return nil
}
