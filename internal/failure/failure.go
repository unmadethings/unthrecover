// Package failure defines categorized errors. Every failed backup or
// verification is recorded with one of these categories so that operators
// (and automation) can tell "the backup is bad" from "the environment is bad".
package failure

import (
	"context"
	"errors"
	"fmt"
	"syscall"
)

// Category classifies a failure.
type Category string

// Failure categories.
const (
	ConfigInvalid       Category = "CONFIG_INVALID"
	ConnectionFailed    Category = "CONNECTION_FAILED"
	AuthenticationFail  Category = "AUTHENTICATION_FAILED"
	UnsupportedVersion  Category = "UNSUPPORTED_VERSION"
	ToolMissing         Category = "TOOL_MISSING"
	BackupFailed        Category = "BACKUP_FAILED"
	InsufficientStorage Category = "INSUFFICIENT_STORAGE"
	ArtifactMissing     Category = "ARTIFACT_MISSING"
	ArtifactInvalid     Category = "ARTIFACT_INVALID"
	ChecksumMismatch    Category = "CHECKSUM_MISMATCH"
	ManifestInvalid     Category = "MANIFEST_INVALID"
	DockerUnavailable   Category = "DOCKER_UNAVAILABLE"
	EnvironmentFailed   Category = "ENVIRONMENT_FAILED"
	RestoreFailed       Category = "RESTORE_FAILED"
	DataMismatch        Category = "DATA_MISMATCH"
	SchemaMismatch      Category = "SCHEMA_MISMATCH"
	AssertionFailed     Category = "ASSERTION_FAILED"
	Timeout             Category = "TIMEOUT"
	Cancelled           Category = "CANCELLED"
	Interrupted         Category = "INTERRUPTED"
	SourceNotFound      Category = "SOURCE_NOT_FOUND"
	ConsistencyUnsafe   Category = "CONSISTENCY_UNSAFE"
	FileMismatch        Category = "FILE_MISMATCH"
	OffsiteFailed       Category = "OFFSITE_FAILED"
	OffsiteMismatch     Category = "OFFSITE_MISMATCH"
	SignatureInvalid    Category = "SIGNATURE_INVALID"
	DecryptionFailed    Category = "DECRYPTION_FAILED"
	Internal            Category = "INTERNAL_ERROR"
)

// Error is an error with a category and an operator-facing remediation hint.
type Error struct {
	Category Category
	Message  string
	Hint     string
	Err      error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Category, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Category, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// New creates a categorized error.
func New(cat Category, msg string, err error) *Error {
	return &Error{Category: cat, Message: msg, Err: err}
}

// WithHint returns e with a remediation hint.
func (e *Error) WithHint(h string) *Error { e.Hint = h; return e }

// CategoryOf extracts the category of err, mapping context errors and ENOSPC.
func CategoryOf(err error) Category {
	if err == nil {
		return ""
	}
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Category
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout
	case errors.Is(err, context.Canceled):
		return Cancelled
	case errors.Is(err, syscall.ENOSPC):
		return InsufficientStorage
	}
	return Internal
}

// HintOf returns the remediation hint of err, if any.
func HintOf(err error) string {
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Hint
	}
	return ""
}
