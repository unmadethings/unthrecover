// Package report defines the verification report model and its renderings.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/unmadethings/unthrecover/internal/postgres"
	"github.com/unmadethings/unthrecover/internal/volume"
)

// Step statuses.
const (
	StepPass    = "PASS"
	StepFail    = "FAIL"
	StepNotRun  = "NOT_RUN"
	StepWarning = "WARN"
)

// Step is one verification step.
type Step struct {
	Name       string `json:"name"`
	Mandatory  bool   `json:"mandatory"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Detail     string `json:"detail,omitempty"`
}

// Scope states exactly what was and was not verified.
type Scope struct {
	Covered    []string `json:"covered"`
	NotCovered []string `json:"not_covered"`
}

// Objectives separates observed test values from real-world RTO/RPO.
type Objectives struct {
	ObservedRestoreDurationMS int64     `json:"observed_test_restore_duration_ms"`
	ObservedTotalDurationMS   int64     `json:"observed_total_verification_duration_ms"`
	RTONote                   string    `json:"rto_note"`
	BackupSnapshotAt          time.Time `json:"backup_snapshot_at"`
	DataLossWindowSeconds     int64     `json:"estimated_data_loss_window_seconds_at_report_time"`
	RPONote                   string    `json:"rpo_note"`
}

// Environment describes the isolated restore environment.
type Environment struct {
	DockerVersion string  `json:"docker_version,omitempty"`
	RestoreImage  string  `json:"restore_image,omitempty"`
	Network       string  `json:"network,omitempty"`
	MemoryMB      int64   `json:"memory_mb,omitempty"`
	CPUs          float64 `json:"cpus,omitempty"`
	PidsLimit     int64   `json:"pids_limit,omitempty"`
	TargetVersion string  `json:"target_server_version,omitempty"`
}

// Cleanup records resource cleanup.
type Cleanup struct {
	Status  string   `json:"status"`
	Removed []string `json:"removed,omitempty"`
	Errors  []string `json:"errors,omitempty"`
}

// BackupInfo summarizes the backup under test.
type BackupInfo struct {
	CreatedAt     time.Time `json:"created_at"`
	SnapshotAt    time.Time `json:"snapshot_at"`
	SizeBytes     int64     `json:"size_bytes"`
	SHA256        string    `json:"sha256"`
	ServerVersion string    `json:"source_server_version"`
	Type          string    `json:"type"`
}

// Report is a verification report. It is produced for every run, including
// failed and cancelled ones.
type Report struct {
	ReportVersion    int                  `json:"report_version"`
	ToolVersion      string               `json:"tool_version"`
	RunID            string               `json:"run_id"`
	BackupID         string               `json:"backup_id"`
	WorkloadID       string               `json:"workload_id"`
	Source           map[string]any       `json:"source"`
	Backup           BackupInfo           `json:"backup"`
	FinalStatus      string               `json:"final_status"`
	ErrorCategory    string               `json:"error_category,omitempty"`
	ErrorMessage     string               `json:"error_message,omitempty"`
	Hint             string               `json:"hint,omitempty"`
	StartedAt        time.Time            `json:"started_at"`
	FinishedAt       time.Time            `json:"finished_at"`
	ChecksumResult   string               `json:"checksum_result"`
	RestoreResult    string               `json:"restore_test_result"`
	PostgresHealth   string               `json:"postgres_health"`
	ContainerHealth  string               `json:"container_health"`
	Steps            []Step               `json:"steps"`
	Scope            Scope                `json:"verification_scope"`
	Comparison       *postgres.Comparison `json:"comparison,omitempty"`
	FileComparison   *volume.Comparison   `json:"file_comparison,omitempty"`
	Objectives       Objectives           `json:"recovery_objectives"`
	Environment      Environment          `json:"environment"`
	Cleanup          Cleanup              `json:"cleanup"`
	DetectedFailures []string             `json:"detected_failures"`
}

// JSON writes the report as indented JSON.
func (r *Report) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Text writes a human-readable report.
func (r *Report) Text(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "UNTH RECOVER — RECOVERY VERIFICATION REPORT\n")
	fmt.Fprintf(&b, "============================================\n")
	fmt.Fprintf(&b, "Final status:      %s\n", r.FinalStatus)
	if r.ErrorCategory != "" {
		fmt.Fprintf(&b, "Failure category:  %s\n", r.ErrorCategory)
		fmt.Fprintf(&b, "Failure:           %s\n", r.ErrorMessage)
		if r.Hint != "" {
			fmt.Fprintf(&b, "Suggested fix:     %s\n", r.Hint)
		}
	}
	fmt.Fprintf(&b, "Run ID:            %s\n", r.RunID)
	fmt.Fprintf(&b, "Backup ID:         %s\n", r.BackupID)
	fmt.Fprintf(&b, "Workload:          %s\n", r.WorkloadID)
	if r.Source != nil {
		if r.Source["type"] == "docker-volume" {
			fmt.Fprintf(&b, "Source:            Docker volume %v (consistency: %v)\n", r.Source["volume"], r.Source["consistency"])
		} else {
			fmt.Fprintf(&b, "Source:            %v@%v:%v/%v\n", r.Source["user"], r.Source["host"], r.Source["port"], r.Source["database"])
		}
	}
	fmt.Fprintf(&b, "Backup size:       %d bytes\n", r.Backup.SizeBytes)
	fmt.Fprintf(&b, "Backup SHA-256:    %s\n", r.Backup.SHA256)
	if r.Backup.ServerVersion != "" {
		fmt.Fprintf(&b, "Source version:    %s\n", r.Backup.ServerVersion)
	}
	fmt.Fprintf(&b, "Snapshot taken at: %s\n", r.Backup.SnapshotAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "Tested at:         %s\n", r.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "\nChecksum:          %s\n", r.ChecksumResult)
	fmt.Fprintf(&b, "Restore test:      %s\n", r.RestoreResult)
	fmt.Fprintf(&b, "PostgreSQL health: %s\n", r.PostgresHealth)
	fmt.Fprintf(&b, "Container health:  %s\n", r.ContainerHealth)
	fmt.Fprintf(&b, "\nSteps:\n")
	tw := tabwriter.NewWriter(&b, 2, 4, 2, ' ', 0)
	for _, s := range r.Steps {
		m := ""
		if s.Mandatory {
			m = "*"
		}
		fmt.Fprintf(tw, "  %s%s\t%s\t%dms\t%s\n", s.Name, m, s.Status, s.DurationMS, oneLine(s.Detail, 100))
	}
	_ = tw.Flush()
	fmt.Fprintf(&b, "  (* mandatory for VERIFIED)\n")
	if c := r.Comparison; c != nil {
		fmt.Fprintf(&b, "\nData checks: %d tables (%d content-hashed), rows expected=%d actual=%d, %d assertion queries\n",
			c.TablesChecked, c.TablesHashed, c.RowsExpected, c.RowsActual, c.QueriesChecked)
		for _, m := range append(append(append([]postgres.Mismatch{}, c.DataMismatches...), c.SchemaMismatches...), c.QueryMismatches...) {
			fmt.Fprintf(&b, "  MISMATCH %s %s: expected=%s actual=%s\n", m.Check, m.Object, oneLine(m.Expected, 60), oneLine(m.Actual, 60))
		}
	}
	if c := r.FileComparison; c != nil {
		fmt.Fprintf(&b, "\nFile checks: %d entries expected, %d restored; %d bytes expected, %d restored; %d differences\n",
			c.EntriesExpected, c.EntriesActual, c.BytesExpected, c.BytesActual, c.MismatchCount)
		for _, m := range c.Mismatches {
			fmt.Fprintf(&b, "  MISMATCH %s %s: expected=%s actual=%s\n", m.Path, m.Field, oneLine(m.Expected, 60), oneLine(m.Actual, 60))
		}
	}
	fmt.Fprintf(&b, "\nVerification scope — covered:\n")
	for _, s := range r.Scope.Covered {
		fmt.Fprintf(&b, "  + %s\n", s)
	}
	fmt.Fprintf(&b, "Verification scope — NOT covered:\n")
	for _, s := range r.Scope.NotCovered {
		fmt.Fprintf(&b, "  - %s\n", s)
	}
	o := r.Objectives
	fmt.Fprintf(&b, "\nObserved test restore duration: %s\n", time.Duration(o.ObservedRestoreDurationMS)*time.Millisecond)
	fmt.Fprintf(&b, "Observed total verification:    %s\n", time.Duration(o.ObservedTotalDurationMS)*time.Millisecond)
	fmt.Fprintf(&b, "  note: %s\n", o.RTONote)
	fmt.Fprintf(&b, "Estimated data-loss window now: %s\n", time.Duration(o.DataLossWindowSeconds)*time.Second)
	fmt.Fprintf(&b, "  note: %s\n", o.RPONote)
	fmt.Fprintf(&b, "\nCleanup: %s", r.Cleanup.Status)
	if len(r.Cleanup.Removed) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(r.Cleanup.Removed, ", "))
	}
	fmt.Fprintln(&b)
	for _, e := range r.Cleanup.Errors {
		fmt.Fprintf(&b, "  cleanup error: %s\n", e)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-3] + "..."
	}
	return s
}
