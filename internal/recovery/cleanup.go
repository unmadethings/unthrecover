// Package recovery handles interrupted-operation recovery: operations whose
// process died (SIGKILL, power loss, OOM) leave records in a non-final state
// and possibly labeled Docker resources. Cleanup reconciles both, touching
// only resources that Unth Recover created and labeled.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/unmadethings/unthrecover/internal/docker"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/repo"
	"github.com/unmadethings/unthrecover/internal/restore"
)

// Action is one cleanup action (performed or planned).
type Action struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Detail string `json:"detail"`
	Done   bool   `json:"done"`
	Error  string `json:"error,omitempty"`
}

// processAlive reports whether pid is a running process other than us.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return true
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Options controls Cleanup.
type Options struct {
	DryRun bool
	// DockerOptional skips Docker reconciliation when the daemon is unreachable.
	DockerOptional bool
}

// Cleanup marks interrupted operations as FAILED/INTERRUPTED, removes their
// partial files, and removes Docker resources of runs that are not active.
func Cleanup(ctx context.Context, store *metadata.Store, r *repo.Repo, dcFn func() (*docker.Client, error), opts Options) ([]Action, error) {
	var actions []Action
	active := map[string]bool{}

	runs, err := store.UnfinishedRuns(ctx)
	if err != nil {
		return nil, err
	}
	for _, run := range runs {
		if processAlive(run.PID) {
			active[run.ID] = true
			continue
		}
		a := Action{Kind: "verification_run", Target: run.ID, Detail: fmt.Sprintf("status %s, owner pid %d not running → FAILED (INTERRUPTED)", run.Status, run.PID)}
		if !opts.DryRun {
			now := time.Now().UTC()
			run.Status = metadata.VerifyFailed
			run.FinishedAt = &now
			run.ErrorCategory = string(failure.Interrupted)
			run.ErrorMessage = "verification process terminated before completion; marked by `unth cleanup`"
			if err := store.UpdateRun(ctx, run); err != nil {
				a.Error = err.Error()
			} else if b, err := store.GetBackup(ctx, run.BackupID); err == nil && b.LastVerificationID == run.ID {
				b.VerificationStatus = metadata.VerifyFailed
				_ = store.UpdateBackup(ctx, b)
			}
			a.Done = a.Error == ""
		}
		actions = append(actions, a)
	}

	backups, err := store.InterruptedBackups(ctx)
	if err != nil {
		return actions, err
	}
	for _, b := range backups {
		if processAlive(b.PID) {
			continue
		}
		a := Action{Kind: "backup", Target: b.ID, Detail: fmt.Sprintf("status %s, owner pid %d not running → FAILED (INTERRUPTED), partial files removed", b.Status, b.PID)}
		if !opts.DryRun {
			if tmp, err := r.TmpFile(b.ID); err == nil {
				_ = os.Remove(tmp)
			}
			if dir, err := r.BackupDir(b.ID); err == nil {
				_ = os.RemoveAll(dir)
			}
			now := time.Now().UTC()
			b.Status = metadata.BackupFailed
			b.CompletedAt = &now
			b.ErrorCategory = string(failure.Interrupted)
			b.ErrorMessage = "backup process terminated before completion; marked by `unth cleanup`"
			b.ArtifactPath, b.ManifestPath, b.PID = "", "", 0
			if err := store.UpdateBackup(ctx, b); err != nil {
				a.Error = err.Error()
			}
			a.Done = a.Error == ""
		}
		actions = append(actions, a)
	}

	dc, err := dcFn()
	if err == nil {
		var ids map[string]bool
		ids, err = restore.ManagedRunIDs(ctx, dc)
		if err == nil {
			for id := range ids {
				if active[id] {
					continue
				}
				a := Action{Kind: "docker_resources", Target: id, Detail: "orphaned verification resources (labeled " + docker.LabelRun + "=" + id + ")"}
				if !opts.DryRun {
					res, cerr := restore.Cleanup(ctx, dc, id)
					if res != nil {
						a.Detail += fmt.Sprintf(": removed %v", res.Removed)
					}
					if cerr != nil {
						a.Error = cerr.Error()
					}
					a.Done = cerr == nil
				}
				actions = append(actions, a)
			}
		}
	}
	if err != nil {
		if opts.DockerOptional {
			actions = append(actions, Action{Kind: "docker_resources", Target: "*", Detail: "skipped: " + err.Error()})
			return actions, nil
		}
		return actions, failure.New(failure.DockerUnavailable, "cannot reconcile Docker resources", err)
	}
	return actions, nil
}
