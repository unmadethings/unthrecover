package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/unmadethings/unthrecover/internal/docker"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/manifest"
	"github.com/unmadethings/unthrecover/internal/report"
	"github.com/unmadethings/unthrecover/internal/restore"
	"github.com/unmadethings/unthrecover/internal/volume"
)

// executeVolume verifies a docker-volume backup:
//
//	artifact → manifest → checksum → archive_readable (safe entries, manifest
//	matches) → isolated_environment (new labeled volume + never-started helper)
//	→ restore (Docker extracts the archive into the new volume) →
//	file_integrity (the restored volume is archived again and every entry's
//	type/mode/owner/size/SHA-256/link compared with the backup manifest).
func (e *Engine) executeVolume(ctx context.Context, st *runState) {
	b := st.backup
	e.stepArtifact(ctx, st)

	e.step(ctx, st, StepManifest, func(ctx context.Context) (string, error) {
		p, err := st.backupPath(e.Repo, b.ManifestPath)
		if err != nil {
			return "", err
		}
		m, err := manifest.Read(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", failure.New(failure.ArtifactMissing, "manifest not found", err)
			}
			return "", failure.New(failure.ManifestInvalid, "manifest unreadable", err)
		}
		switch {
		case m.BackupID != b.ID:
			return "", failure.New(failure.ManifestInvalid, "manifest belongs to backup "+m.BackupID, nil)
		case m.Artifact.SHA256 != b.SHA256:
			return "", failure.New(failure.ChecksumMismatch, "manifest checksum differs from metadata record", nil)
		case m.Artifact.SizeBytes != b.SizeBytes:
			return "", failure.New(failure.ManifestInvalid, "manifest size differs from metadata record", nil)
		case m.VolumeFiles == nil || m.FingerprintDigest != b.SourceFingerprint || m.VolumeFiles.Digest() != b.SourceFingerprint:
			return "", failure.New(failure.ManifestInvalid, "manifest file list does not match metadata record", nil)
		}
		var fm volume.FileManifest
		if err := json.Unmarshal([]byte(b.FingerprintJSON), &fm); err != nil || fm.Digest() != b.SourceFingerprint {
			return "", failure.New(failure.ManifestInvalid, "stored file manifest is corrupt", err)
		}
		st.files, st.manifest = &fm, m
		return fmt.Sprintf("format v%d, %d entries (%d files, %d dirs, %d symlinks)", m.FormatVersion, len(fm.Entries), fm.Files, fm.Dirs, fm.Symlinks), nil
	})

	e.stepChecksum(ctx, st)

	e.step(ctx, st, StepArchive, func(ctx context.Context) (string, error) {
		f, err := os.Open(st.artifact) // #nosec G304 -- validated artifact path
		if err != nil {
			return "", failure.New(failure.ArtifactMissing, "open artifact", err)
		}
		defer f.Close()
		fm, err := volume.Scan(f, volume.DefaultLimits)
		if err != nil {
			return "", failure.New(failure.ArtifactInvalid, "archive is not a safe, readable volume archive", err)
		}
		if fm.Digest() != st.files.Digest() {
			c := volume.Compare(st.files, fm)
			return "", failure.New(failure.ArtifactInvalid, fmt.Sprintf("archive content differs from its manifest (%d differences)", c.MismatchCount), nil)
		}
		return fmt.Sprintf("%d entries, all paths inside the volume root", len(fm.Entries)), nil
	})
	if st.failErr == nil {
		e.setRunStatus(ctx, st, "INTEGRITY_CHECKED")
	}

	var dc *docker.Client
	var helper string
	e.step(ctx, st, StepEnvironment, func(ctx context.Context) (string, error) {
		c, err := e.Docker()
		if err != nil {
			return "", failure.New(failure.DockerUnavailable, "Docker client", err)
		}
		e.setRunStatus(ctx, st, "RESTORE_TESTING")
		info, err := c.Negotiate(ctx)
		if err != nil {
			st.rep.ContainerHealth = report.StepFail
			return "", failure.New(failure.DockerUnavailable, "Docker Engine is not reachable", err).
				WithHint("start the Docker daemon or fix DOCKER_HOST / socket permissions")
		}
		dc = c
		image := st.settings.RestoreImage
		if image == "" {
			image = "busybox:1.36.1"
		}
		st.rep.Environment = report.Environment{DockerVersion: info.Version, RestoreImage: image,
			Network: "none (helper container is never started)"}
		if err := restore.EnsureImage(ctx, c, image, st.settings.ImagePull); err != nil {
			st.rep.ContainerHealth = report.StepFail
			return "", err
		}
		vol := "unth-vol-" + st.run.ID
		labels := restore.Labels(st.run.ID, b.ID, "volume-restore")
		if err := c.CreateVolume(ctx, vol, labels); err != nil {
			return "", failure.New(failure.EnvironmentFailed, "create isolated restore volume", err)
		}
		id, err := c.CreateHelper(ctx, docker.HelperSpec{Name: "unth-vr-" + st.run.ID, Image: image, Labels: labels, Volume: vol})
		if err != nil {
			st.rep.ContainerHealth = report.StepFail
			return "", failure.New(failure.EnvironmentFailed, "create restore helper container", err)
		}
		helper = id
		st.rep.ContainerHealth = report.StepPass
		return fmt.Sprintf("new volume %s, helper %s (never started, no network)", vol, id[:12]), nil
	})

	ok := e.step(ctx, st, StepRestore, func(ctx context.Context) (string, error) {
		f, err := os.Open(st.artifact) // #nosec G304 -- validated artifact path
		if err != nil {
			return "", failure.New(failure.ArtifactMissing, "open artifact", err)
		}
		defer f.Close()
		start := time.Now()
		err = dc.ArchiveTo(ctx, helper, "/", f)
		st.restoreDur = time.Since(start)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", failure.New(failure.RestoreFailed, "Docker could not extract the archive into the restore volume", err)
		}
		return fmt.Sprintf("archive extracted by the Docker daemon in %s", st.restoreDur.Round(time.Millisecond)), nil
	})
	st.rep.RestoreResult = resultOf(ok, st)

	e.step(ctx, st, StepFiles, func(ctx context.Context) (string, error) {
		stream, err := dc.ArchiveFrom(ctx, helper, "/"+volume.Root)
		if err != nil {
			return "", failure.New(failure.RestoreFailed, "read restored volume", err)
		}
		defer stream.Close()
		got, err := volume.Scan(stream, volume.DefaultLimits)
		if err != nil {
			return "", failure.New(failure.RestoreFailed, "restored volume archive unreadable", err)
		}
		c := volume.Compare(st.files, got)
		st.rep.FileComparison = c
		if !c.OK() {
			first := c.Mismatches[0]
			return "", failure.New(failure.FileMismatch, fmt.Sprintf("%d differences between backup and restored volume (first: %s %s expected=%s actual=%s)",
				c.MismatchCount, first.Path, first.Field, first.Expected, first.Actual), nil)
		}
		return fmt.Sprintf("%d entries, %d bytes: type, mode, owner, size, SHA-256 and link targets identical", c.EntriesActual, c.BytesActual), nil
	})
	st.rep.PostgresHealth = "N/A"
}

func buildVolumeScope(st *runState) report.Scope {
	s := report.Scope{}
	passed := map[string]bool{}
	for _, step := range st.rep.Steps {
		passed[step.Name] = step.Status == report.StepPass
	}
	if passed[StepChecksum] {
		s.Covered = append(s.Covered, "artifact SHA-256 matches metadata record and manifest")
	} else {
		s.NotCovered = append(s.NotCovered, "artifact checksum (step did not pass)")
	}
	if passed[StepArchive] {
		s.Covered = append(s.Covered, "archive is well-formed and every entry stays inside the volume root")
	}
	if passed[StepRestore] {
		s.Covered = append(s.Covered, "full restore into a new isolated Docker volume, extracted by the Docker daemon")
	} else {
		s.NotCovered = append(s.NotCovered, "isolated restore (step did not pass)")
	}
	if c := st.rep.FileComparison; c != nil && passed[StepFiles] {
		s.Covered = append(s.Covered, fmt.Sprintf("all %d entries (%d bytes): type, permissions, owner uid/gid, size, SHA-256 content and symlink targets", c.EntriesActual, c.BytesActual))
	} else {
		s.NotCovered = append(s.NotCovered, "file-level comparison (step did not pass)")
	}
	s.NotCovered = append(s.NotCovered,
		"application-level consistency beyond the chosen policy (offline: no running writers; stop: writers stopped) — files written by a live process are never claimed consistent",
		"application health (no application containers are started from the restored volume)",
		"extended attributes, ACLs, hard-link identity and modification times",
		"authenticity of the artifact (checksums are not signatures)")
	return s
}
