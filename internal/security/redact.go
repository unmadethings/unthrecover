// Package security contains small, dependency-free helpers that enforce the
// project's security rules: secret redaction, safe path handling and
// private credential files for PostgreSQL client tools.
package security

import (
	"sort"
	"strings"
	"sync"
)

// RedactedPlaceholder replaces every secret value found in redacted text.
const RedactedPlaceholder = "[REDACTED]"

// minSecretLen avoids redacting trivially short values (e.g. "a") that would
// mangle unrelated text. Such secrets are rejected by config validation anyway.
const minSecretLen = 4

// Redactor removes registered secret values from arbitrary text. It is safe
// for concurrent use. Every string that may contain process output, error
// messages or connection details must pass through a Redactor before it is
// logged, stored in metadata or written to a report.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

// NewRedactor returns an empty Redactor.
func NewRedactor() *Redactor { return &Redactor{} }

// Add registers secret values. Empty and very short values are ignored.
func (r *Redactor) Add(secrets ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range secrets {
		if len(s) < minSecretLen {
			continue
		}
		dup := false
		for _, existing := range r.secrets {
			if existing == s {
				dup = true
				break
			}
		}
		if !dup {
			r.secrets = append(r.secrets, s)
		}
	}
	// Replace longest secrets first so that a secret containing another
	// secret is fully removed.
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

// Redact returns s with every registered secret replaced.
func (r *Redactor) Redact(s string) string {
	if r == nil || s == "" {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, secret := range r.secrets {
		if strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, RedactedPlaceholder)
		}
	}
	return s
}

// RedactError returns the redacted message of err ("" when err is nil).
func (r *Redactor) RedactError(err error) string {
	if err == nil {
		return ""
	}
	return r.Redact(err.Error())
}
