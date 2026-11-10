// Package runner implements `unth run`: one scheduled job = backup, verify,
// prune (retention) and notify, with a per-workload lock so overlapping
// schedules can never run the same job twice at the same time.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/unmadethings/unthrecover/internal/backup"
	"github.com/unmadethings/unthrecover/internal/buildinfo"
	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/docker"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/notify"
	"github.com/unmadethings/unthrecover/internal/offsite"
	"github.com/unmadethings/unthrecover/internal/repo"
	"github.com/unmadethings/unthrecover/internal/report"
	"github.com/unmadethings/unthrecover/internal/retention"
	"github.com/unmadethings/unthrecover/internal/security"
	"github.com/unmadethings/unthrecover/internal/verify"
)

// Outcomes.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
)

// Summary is the result of one `unth run`.
type Summary struct {
	Workload           string            `json:"workload"`
	Outcome            string            `json:"outcome"`
	Stage              string            `json:"failed_stage,omitempty"` // lock|backup|verify|offsite|prune
	BackupID           string            `json:"backup_id,omitempty"`
	BackupStatus       string            `json:"backup_status,omitempty"`
	RunID              string            `json:"verification_run_id,omitempty"`
	VerificationStatus string            `json:"verification_status,omitempty"`
	ErrorCategory      string            `json:"error_category,omitempty"`
	ErrorMessage       string            `json:"error_message,omitempty"`
	Hint               string            `json:"hint,omitempty"`
	Offsite            *OffsiteSummary   `json:"offsite,omitempty"`
	Prune              *retention.Result `json:"prune,omitempty"`
	PruneSkipped       string            `json:"prune_skipped,omitempty"`
	Notifications      []notify.Result   `json:"notifications"`
	StartedAt          time.Time         `json:"started_at"`
	FinishedAt         time.Time         `json:"finished_at"`
	Cancelled          bool              `json:"cancelled,omitempty"`
}

// OffsiteSummary is the offsite part of a run.
type OffsiteSummary struct {
	Target    string   `json:"target"`
	Location  string   `json:"location,omitempty"`
	Status    string   `json:"status"`
	Encrypted bool     `json:"encrypted"`
	Signed    bool     `json:"signed"`
	Pruned    []string `json:"pruned_backup_ids,omitempty"`
}

// NotifyFailed reports whether any notification could not be delivered.
func (s *Summary) NotifyFailed() bool {
	for _, n := range s.Notifications {
		if n.Error != "" {
			return true
		}
	}
	return false
}

// Runner holds dependencies.
type Runner struct {
	Store    *metadata.Store
	Repo     *repo.Repo
	Docker   func() (*docker.Client, error)
	Redactor *security.Redactor
	Log      *slog.Logger
	Progress func(step report.Step)
	Sender   *notify.Sender
	Now      func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

// Run executes the whole job. It always returns a summary.
func (r *Runner) Run(ctx context.Context, cfg *config.Config) *Summary {
	sum := &Summary{Workload: cfg.Source.Name, StartedAt: r.now(), Notifications: []notify.Result{}}
	fail := func(stage string, err error) {
		sum.Outcome, sum.Stage = OutcomeFailure, stage
		sum.ErrorCategory = string(failure.CategoryOf(err))
		sum.ErrorMessage = r.Redactor.RedactError(err)
		sum.Hint = failure.HintOf(err)
		if errors.Is(err, context.Canceled) {
			sum.Cancelled = true
		}
	}

	unlock, err := r.lock(cfg.Source.Name)
	if err != nil {
		fail("lock", err)
		return r.finish(ctx, cfg, sum)
	}
	defer unlock()

	eng := &backup.Engine{Store: r.Store, Repo: r.Repo, Redactor: r.Redactor, Log: r.Log, Docker: r.Docker}
	b, err := eng.Run(ctx, cfg)
	if b != nil {
		sum.BackupID, sum.BackupStatus = b.ID, b.Status
	}
	if err != nil {
		fail("backup", err)
		return r.finish(ctx, cfg, sum)
	}

	ve := &verify.Engine{Store: r.Store, Repo: r.Repo, Docker: r.Docker, Redactor: r.Redactor, Log: r.Log, Progress: r.Progress}
	rep, err := ve.Run(ctx, b.ID, verify.Options{})
	if err != nil && rep == nil {
		fail("verify", err)
		return r.finish(ctx, cfg, sum)
	}
	sum.RunID, sum.VerificationStatus = rep.RunID, rep.FinalStatus
	if rep.FinalStatus != metadata.VerifyVerified {
		sum.Outcome, sum.Stage = OutcomeFailure, "verify"
		sum.ErrorCategory, sum.ErrorMessage, sum.Hint = rep.ErrorCategory, rep.ErrorMessage, rep.Hint
		sum.Cancelled = rep.FinalStatus == metadata.VerifyCanceled
		sum.PruneSkipped = "verification did not succeed; older backups are kept"
		return r.finish(ctx, cfg, sum)
	}

	// Offsite copy (and its proof) before any pruning: a failed copy keeps
	// every local backup.
	if cfg.Offsite.Enabled() {
		if err := r.offsite(ctx, cfg, b.ID, sum); err != nil {
			fail("offsite", err)
			sum.PruneSkipped = "offsite copy failed; nothing was pruned"
			return r.finish(ctx, cfg, sum)
		}
	}

	if cfg.Retention.Enabled() {
		pr, err := retention.Apply(ctx, r.Store, r.Repo, cfg.Source.Name, cfg.Retention, false, repo.Actor(), r.now())
		sum.Prune = pr
		if err != nil {
			fail("prune", err)
			return r.finish(ctx, cfg, sum)
		}
	} else {
		sum.PruneSkipped = "no retention rules configured"
	}
	sum.Outcome = OutcomeSuccess
	return r.finish(ctx, cfg, sum)
}

func (r *Runner) offsite(ctx context.Context, cfg *config.Config, backupID string, sum *Summary) error {
	octx, cancel := context.WithTimeout(ctx, cfg.Offsite.Timeout.D())
	defer cancel()
	cp, err := offsite.Open(octx, cfg, r.Store, r.Repo, repo.Actor())
	if err != nil {
		sum.Offsite = &OffsiteSummary{Status: metadata.CopyFailed}
		return err
	}
	sum.Offsite = &OffsiteSummary{Target: cp.Backend.Target(), Status: metadata.CopyFailed,
		Encrypted: cp.Keys.Identity != nil, Signed: cp.Keys.SignKey != nil}
	b, err := r.Store.GetBackup(octx, backupID) // re-read: verification updated it
	if err != nil {
		return err
	}
	rec, err := cp.Copy(octx, b)
	if rec != nil {
		sum.Offsite.Status, sum.Offsite.Location = rec.Status, rec.Location
	}
	if err != nil {
		if errors.Is(octx.Err(), context.DeadlineExceeded) {
			return failure.New(failure.Timeout, "offsite copy exceeded offsite.timeout", err)
		}
		return err
	}
	pr, err := cp.Prune(octx, cfg.Source.Name, cfg.Offsite.Retention, false)
	if pr != nil {
		sum.Offsite.Pruned = pr.Deleted
	}
	return err
}

func (r *Runner) finish(ctx context.Context, cfg *config.Config, sum *Summary) *Summary {
	sum.FinishedAt = r.now()
	if len(cfg.Notify) > 0 {
		host, _ := os.Hostname()
		ev := &notify.Event{
			Event: "unth.run.completed", SchemaVersion: 1, ToolVersion: buildinfo.Version, Host: host,
			Workload: sum.Workload, Outcome: sum.Outcome, BackupID: sum.BackupID, BackupStatus: sum.BackupStatus,
			RunID: sum.RunID, VerificationStatus: sum.VerificationStatus, ErrorCategory: sum.ErrorCategory,
			ErrorMessage: sum.ErrorMessage, Hint: sum.Hint, StartedAt: sum.StartedAt, FinishedAt: sum.FinishedAt,
			DurationMS: sum.FinishedAt.Sub(sum.StartedAt).Milliseconds(),
		}
		if sum.Prune != nil {
			ev.Pruned = sum.Prune.Expired
		}
		if sum.Offsite != nil {
			ev.OffsiteTarget, ev.OffsiteStatus = sum.Offsite.Target, sum.Offsite.Status
		}
		sender := r.Sender
		if sender == nil {
			sender = &notify.Sender{Redactor: r.Redactor}
		}
		// Notifications are sent even if the run was cancelled.
		nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		sum.Notifications = sender.SendAll(nctx, cfg.Notify, ev)
		cancel()
	}
	_ = r.Store.Audit(context.WithoutCancel(ctx), metadata.AuditEvent{Actor: repo.Actor(), Action: "run.finish",
		Target: sum.Workload, Outcome: sum.Outcome,
		Details: r.Redactor.Redact(fmt.Sprintf("backup=%s run=%s verification=%s stage=%s", sum.BackupID, sum.RunID, sum.VerificationStatus, sum.Stage))})
	return sum
}

// lock takes a non-blocking exclusive flock on <data-dir>/locks/<workload>.lock.
func (r *Runner) lock(workload string) (func(), error) {
	dir := filepath.Join(r.Repo.Root, "locks")
	if err := security.EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	p, err := security.SafeJoin(dir, workload+".lock")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- validated workload name
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- fd fits int
		_ = f.Close()
		return nil, failure.New(failure.Internal, fmt.Sprintf("another `unth run` for workload %q is in progress", workload), err).
			WithHint("wait for it to finish; schedules should not overlap")
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // #nosec G115
		_ = f.Close()
	}, nil
}
