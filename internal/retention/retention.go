// Package retention decides which backups of a workload to expire and
// applies that decision.
//
// Rules (a backup is kept if ANY rule keeps it):
//   - keep_last N:   the N newest backups;
//   - keep_daily N:  the newest backup of each of the N most recent UTC days that have backups;
//   - keep_weekly N: the newest backup of each of the N most recent ISO weeks that have backups;
//   - max_age D:     when keep_* rules are absent, backups younger than D are kept;
//     when combined with keep_* rules, backups older than D are expired even if
//     a keep_* rule matched them.
//
// Safety rules that override everything:
//   - the newest VERIFIED backup of the workload is never expired;
//   - a backup whose verification is in progress is never expired;
//   - only BACKUP_CREATED backups are considered (failed backups have no artifact).
package retention

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/repo"
)

// Decision is the verdict for one backup.
type Decision struct {
	BackupID  string    `json:"backup_id"`
	CreatedAt time.Time `json:"created_at"`
	Keep      bool      `json:"keep"`
	Reasons   []string  `json:"reasons"`
	Verified  string    `json:"verification_status"`
}

var inProgress = map[string]bool{
	metadata.VerifyPending: true, metadata.VerifyRunning: true,
	metadata.VerifyIntegrityChecked: true, metadata.VerifyRestoreTesting: true,
}

// Plan computes decisions for the backups of ONE workload. It is pure.
func Plan(backups []*metadata.Backup, p config.Retention, now time.Time) []Decision {
	var cands []*metadata.Backup
	for _, b := range backups {
		if b.Status == metadata.BackupCreated {
			cands = append(cands, b)
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].CreatedAt.After(cands[j].CreatedAt) })

	reasons := make(map[string][]string, len(cands))
	add := func(id, r string) { reasons[id] = append(reasons[id], r) }

	hasKeep := p.KeepLast > 0 || p.KeepDaily > 0 || p.KeepWeekly > 0
	for i, b := range cands {
		if i < p.KeepLast {
			add(b.ID, fmt.Sprintf("keep_last %d", p.KeepLast))
		}
	}
	bucket := func(n int, label string, key func(time.Time) string) {
		if n <= 0 {
			return
		}
		seen := map[string]bool{}
		for _, b := range cands {
			k := key(b.CreatedAt.UTC())
			if seen[k] {
				continue
			}
			if len(seen) >= n {
				break
			}
			seen[k] = true
			add(b.ID, fmt.Sprintf("%s %d (%s)", label, n, k))
		}
	}
	bucket(p.KeepDaily, "keep_daily", func(t time.Time) string { return t.Format("2006-01-02") })
	bucket(p.KeepWeekly, "keep_weekly", func(t time.Time) string {
		y, w := t.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w)
	})

	tooOld := func(b *metadata.Backup) bool {
		return p.MaxAge > 0 && now.Sub(b.CreatedAt) > p.MaxAge.D()
	}
	if !hasKeep && p.MaxAge > 0 {
		for _, b := range cands {
			if !tooOld(b) {
				add(b.ID, "younger than max_age "+p.MaxAge.D().String())
			}
		}
	}

	newestVerified := ""
	for _, b := range cands {
		if b.VerificationStatus == metadata.VerifyVerified {
			newestVerified = b.ID
			break
		}
	}

	out := make([]Decision, 0, len(cands))
	for _, b := range cands {
		d := Decision{BackupID: b.ID, CreatedAt: b.CreatedAt, Verified: b.VerificationStatus, Reasons: reasons[b.ID]}
		d.Keep = len(d.Reasons) > 0
		if d.Keep && hasKeep && tooOld(b) {
			d.Keep = false
			d.Reasons = []string{"older than max_age " + p.MaxAge.D().String()}
		}
		switch {
		case b.ID == newestVerified:
			if !d.Keep {
				d.Reasons = nil
			}
			d.Keep = true
			d.Reasons = append(d.Reasons, "newest VERIFIED backup (always kept)")
		case inProgress[b.VerificationStatus]:
			d.Keep = true
			d.Reasons = append(d.Reasons, "verification in progress (always kept)")
		}
		if !d.Keep && len(d.Reasons) == 0 {
			d.Reasons = []string{"not matched by any keep rule"}
		}
		out = append(out, d)
	}
	return out
}

// Result summarizes an applied plan.
type Result struct {
	Workload  string     `json:"workload"`
	DryRun    bool       `json:"dry_run"`
	Decisions []Decision `json:"decisions"`
	Expired   []string   `json:"expired"`
	Errors    []string   `json:"errors,omitempty"`
}

// Apply plans and (unless dryRun) expires backups: the artifact directory is
// deleted and the record becomes EXPIRED. Metadata, verification runs and
// reports are kept as history.
func Apply(ctx context.Context, store *metadata.Store, r *repo.Repo, workload string, p config.Retention,
	dryRun bool, actor string, now time.Time) (*Result, error) {
	if !p.Enabled() {
		return nil, fmt.Errorf("no retention rules configured (set retention.keep_last/keep_daily/keep_weekly/max_age)")
	}
	bs, err := store.ListBackupsByWorkload(ctx, workload)
	if err != nil {
		return nil, err
	}
	res := &Result{Workload: workload, DryRun: dryRun, Decisions: Plan(bs, p, now), Expired: []string{}}
	byID := map[string]*metadata.Backup{}
	for _, b := range bs {
		byID[b.ID] = b
	}
	for _, d := range res.Decisions {
		if d.Keep {
			continue
		}
		if dryRun {
			res.Expired = append(res.Expired, d.BackupID)
			continue
		}
		b := byID[d.BackupID]
		dir, err := r.BackupDir(b.ID)
		if err == nil {
			err = os.RemoveAll(dir)
		}
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", b.ID, err))
			continue
		}
		b.Status = metadata.BackupExpired
		b.ArtifactPath, b.ManifestPath = "", ""
		if err := store.UpdateBackup(ctx, b); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", b.ID, err))
			continue
		}
		_ = store.Audit(ctx, metadata.AuditEvent{Actor: actor, Action: "prune.expire", Target: b.ID,
			Outcome: "expired", Details: fmt.Sprintf("workload=%s reasons=%v", workload, d.Reasons)})
		res.Expired = append(res.Expired, b.ID)
	}
	if len(res.Errors) > 0 {
		return res, fmt.Errorf("prune incomplete: %v", res.Errors)
	}
	return res, nil
}
