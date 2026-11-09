package verify

import (
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/report"
)

// Mandatory steps. A backup is VERIFIED only if EVERY one of these steps was
// actually executed in the current run and passed. Checksum success alone is
// never sufficient.
const (
	StepArtifact    = "artifact_accessible"
	StepManifest    = "manifest_valid"
	StepChecksum    = "checksum_sha256"
	StepArchive     = "archive_readable"
	StepEnvironment = "isolated_environment"
	StepRestore     = "restore"
	StepHealth      = "postgres_health"
	StepData        = "data_integrity"
	StepSchema      = "schema_and_constraints"
	StepQueries     = "assertion_queries"
)

// MandatorySteps lists, in order, the steps required for VERIFIED.
var MandatorySteps = []string{
	StepArtifact, StepManifest, StepChecksum, StepArchive, StepEnvironment,
	StepRestore, StepHealth, StepData, StepSchema, StepQueries,
}

// Decide computes the final status from executed steps. It is a pure
// function so that the core safety rule is unit-testable:
//
//   - cancelled runs are CANCELLED;
//   - any failed step, or any mandatory step that is missing or not run,
//     yields FAILED;
//   - only a complete set of passing mandatory steps yields VERIFIED.
func Decide(steps []report.Step, cancelled bool) string {
	if cancelled {
		return metadata.VerifyCanceled
	}
	byName := map[string]report.Step{}
	for _, s := range steps {
		if s.Status == report.StepFail {
			return metadata.VerifyFailed
		}
		if prev, ok := byName[s.Name]; ok && prev.Status != s.Status {
			return metadata.VerifyFailed // conflicting duplicate results are never trusted
		}
		byName[s.Name] = s
	}
	for _, name := range MandatorySteps {
		s, ok := byName[name]
		if !ok || s.Status != report.StepPass || !s.Mandatory {
			return metadata.VerifyFailed
		}
	}
	return metadata.VerifyVerified
}
