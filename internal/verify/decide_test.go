package verify

import (
	"testing"

	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/report"
)

func allPass() []report.Step {
	var s []report.Step
	for _, n := range MandatorySteps {
		s = append(s, report.Step{Name: n, Mandatory: true, Status: report.StepPass})
	}
	return s
}

func TestDecideOnlyFullPassIsVerified(t *testing.T) {
	if got := Decide(allPass(), false); got != metadata.VerifyVerified {
		t.Fatalf("all mandatory steps passed: got %s", got)
	}
}

func TestDecideNeverVerifiesIncompleteRuns(t *testing.T) {
	// The core product rule: a checksum alone must never produce VERIFIED.
	checksumOnly := []report.Step{
		{Name: StepArtifact, Mandatory: true, Status: report.StepPass},
		{Name: StepManifest, Mandatory: true, Status: report.StepPass},
		{Name: StepChecksum, Mandatory: true, Status: report.StepPass},
		{Name: StepArchive, Mandatory: true, Status: report.StepPass},
	}
	if got := Decide(checksumOnly, false); got != metadata.VerifyFailed {
		t.Fatalf("checksum-only run: got %s", got)
	}

	for _, missing := range MandatorySteps {
		var steps []report.Step
		for _, s := range allPass() {
			if s.Name != missing {
				steps = append(steps, s)
			}
		}
		if got := Decide(steps, false); got != metadata.VerifyFailed {
			t.Errorf("missing %s: got %s", missing, got)
		}
	}

	for i := range MandatorySteps {
		for _, st := range []string{report.StepFail, report.StepNotRun, report.StepWarning, ""} {
			steps := allPass()
			steps[i].Status = st
			if got := Decide(steps, false); got != metadata.VerifyFailed {
				t.Errorf("step %s=%q: got %s", steps[i].Name, st, got)
			}
		}
	}

	steps := allPass()
	steps[5].Mandatory = false
	if got := Decide(steps, false); got != metadata.VerifyFailed {
		t.Errorf("mandatory flag tampered: got %s", got)
	}

	steps = append(allPass(), report.Step{Name: "extra", Status: report.StepFail})
	if got := Decide(steps, false); got != metadata.VerifyFailed {
		t.Errorf("failed optional step: got %s", got)
	}

	steps = append(allPass(), report.Step{Name: StepRestore, Mandatory: true, Status: report.StepNotRun})
	if got := Decide(steps, false); got != metadata.VerifyFailed {
		t.Errorf("conflicting duplicate: got %s", got)
	}

	if got := Decide(nil, false); got != metadata.VerifyFailed {
		t.Errorf("no steps: got %s", got)
	}
}

func TestDecideCancelled(t *testing.T) {
	if got := Decide(allPass(), true); got != metadata.VerifyCanceled {
		t.Fatalf("cancelled: got %s", got)
	}
}

func TestDecideForVolume(t *testing.T) {
	var steps []report.Step
	for _, n := range VolumeMandatorySteps {
		steps = append(steps, report.Step{Name: n, Mandatory: true, Status: report.StepPass})
	}
	if got := DecideFor(steps, false, MandatoryFor(BackupTypeVolume)); got != metadata.VerifyVerified {
		t.Fatalf("volume all pass: %s", got)
	}
	// A volume run can never satisfy the PostgreSQL list (and vice versa).
	if got := DecideFor(steps, false, MandatoryFor("postgres-logical")); got != metadata.VerifyFailed {
		t.Fatalf("volume steps verified a postgres backup: %s", got)
	}
	if got := DecideFor(steps[:len(steps)-1], false, VolumeMandatorySteps); got != metadata.VerifyFailed {
		t.Fatalf("missing file_integrity: %s", got)
	}
	if got := DecideFor(steps, false, nil); got != metadata.VerifyFailed {
		t.Fatalf("empty mandatory list must never verify: %s", got)
	}
}
