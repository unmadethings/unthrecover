// Package config loads and validates Unth Recover job configuration files.
//
// Secrets are never stored in the configuration file itself: the PostgreSQL
// password is read from an environment variable (password_env) or from a
// file with 0600 permissions (password_file). A literal "password" key is
// rejected by the strict decoder.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/unmadethings/unthrecover/internal/security"
)

// Duration is a time.Duration that unmarshals from strings like "30s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("line %d: duration must be a string like \"30s\"", node.Line)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", node.Line, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// D returns the value as time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is the root of a job configuration file.
type Config struct {
	Version      int          `yaml:"version"`
	Source       Source       `yaml:"source"`
	Backup       Backup       `yaml:"backup"`
	Verification Verification `yaml:"verification"`
}

// Source describes the PostgreSQL database to protect.
type Source struct {
	Name           string   `yaml:"name"`
	Type           string   `yaml:"type"`
	Host           string   `yaml:"host"`
	Port           int      `yaml:"port"`
	Database       string   `yaml:"database"`
	User           string   `yaml:"user"`
	PasswordEnv    string   `yaml:"password_env"`
	PasswordFile   string   `yaml:"password_file"`
	SSLMode        string   `yaml:"sslmode"`
	ConnectTimeout Duration `yaml:"connect_timeout"`
}

// Backup holds backup execution settings.
type Backup struct {
	Timeout      Duration `yaml:"timeout"`
	MinFreeBytes int64    `yaml:"min_free_bytes"`
}

// Verification holds the isolated restore test settings. They are stored
// with each backup so that `unth verify <id>` needs no configuration file.
type Verification struct {
	RestoreImage     string   `yaml:"restore_image" json:"restore_image"`
	ImagePull        string   `yaml:"image_pull" json:"image_pull"`
	Timeout          Duration `yaml:"timeout" json:"timeout"`
	MemoryMB         int64    `yaml:"memory_mb" json:"memory_mb"`
	CPUs             float64  `yaml:"cpus" json:"cpus"`
	PidsLimit        int64    `yaml:"pids_limit" json:"pids_limit"`
	TableHashMaxRows int64    `yaml:"table_hash_max_rows" json:"table_hash_max_rows"`
	Queries          []Query  `yaml:"queries" json:"queries"`
}

// Query is an application-specific, read-only assertion. It runs against the
// source inside the backup snapshot and against the restored database; the
// results must be identical.
type Query struct {
	Name string `yaml:"name" json:"name"`
	SQL  string `yaml:"sql" json:"sql"`
}

// Image pull policies.
const (
	PullIfMissing = "if-missing"
	PullAlways    = "always"
	PullNever     = "never"
)

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	envPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// imagePattern is intentionally conservative: registry/repo:tag or @digest.
	imagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*(@sha256:[a-f0-9]{64})?$`)
	validSSL     = map[string]bool{"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true}
)

// Load reads, strictly decodes, defaults and validates a config file.
func Load(path string) (*Config, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if fi.Size() > 1<<20 {
		return nil, fmt.Errorf("config %q is larger than 1 MiB", path)
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied config path; size bounded above
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse decodes configuration bytes. Unknown keys are errors.
func Parse(raw []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		if strings.Contains(err.Error(), "field password ") {
			return nil, errors.New("invalid config: plaintext 'password' is not allowed; use source.password_env or source.password_file")
		}
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ApplyDefaults fills unset optional fields.
func (c *Config) ApplyDefaults() {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Source.Type == "" {
		c.Source.Type = "postgres"
	}
	if c.Source.Port == 0 {
		c.Source.Port = 5432
	}
	if c.Source.SSLMode == "" {
		c.Source.SSLMode = "prefer"
	}
	if c.Source.ConnectTimeout == 0 {
		c.Source.ConnectTimeout = Duration(10 * time.Second)
	}
	if c.Backup.Timeout == 0 {
		c.Backup.Timeout = Duration(time.Hour)
	}
	if c.Backup.MinFreeBytes == 0 {
		c.Backup.MinFreeBytes = 256 << 20
	}
	c.Verification.ApplyDefaults()
}

// ApplyDefaults fills unset verification fields.
func (v *Verification) ApplyDefaults() {
	if v.ImagePull == "" {
		v.ImagePull = PullIfMissing
	}
	if v.Timeout == 0 {
		v.Timeout = Duration(30 * time.Minute)
	}
	if v.MemoryMB == 0 {
		v.MemoryMB = 1024
	}
	if v.CPUs == 0 {
		v.CPUs = 1
	}
	if v.PidsLimit == 0 {
		v.PidsLimit = 512
	}
	if v.TableHashMaxRows == 0 {
		v.TableHashMaxRows = 100000
	}
}

// Validate returns a descriptive error for the first invalid field.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if c.Version != 1 {
		add("version: unsupported config version %d (expected 1)", c.Version)
	}
	s := c.Source
	if !namePattern.MatchString(s.Name) {
		add("source.name: required, letters/digits/._- only, max 63 chars")
	}
	if s.Type != "postgres" {
		add("source.type: only \"postgres\" is supported in this release")
	}
	if s.Host == "" || strings.ContainsAny(s.Host, " \t\n/") && !strings.HasPrefix(s.Host, "/") {
		add("source.host: required")
	}
	if s.Port < 1 || s.Port > 65535 {
		add("source.port: must be 1-65535")
	}
	if s.Database == "" || strings.ContainsAny(s.Database, "\x00\n") {
		add("source.database: required")
	}
	if s.User == "" || strings.ContainsAny(s.User, "\x00\n") {
		add("source.user: required")
	}
	if s.PasswordEnv != "" && s.PasswordFile != "" {
		add("source: set only one of password_env or password_file")
	}
	if s.PasswordEnv != "" && !envPattern.MatchString(s.PasswordEnv) {
		add("source.password_env: invalid environment variable name")
	}
	if !validSSL[s.SSLMode] {
		add("source.sslmode: invalid value %q", s.SSLMode)
	}
	if c.Backup.Timeout.D() < time.Second {
		add("backup.timeout: must be at least 1s")
	}
	if c.Backup.MinFreeBytes < 0 {
		add("backup.min_free_bytes: must not be negative")
	}
	if err := c.Verification.Validate(); err != nil {
		add("%s", err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// Validate checks verification settings.
func (v *Verification) Validate() error {
	var errs []string
	if v.RestoreImage != "" && !imagePattern.MatchString(v.RestoreImage) {
		errs = append(errs, "verification.restore_image: invalid image reference")
	}
	switch v.ImagePull {
	case PullIfMissing, PullAlways, PullNever:
	default:
		errs = append(errs, "verification.image_pull: must be if-missing, always or never")
	}
	if v.Timeout.D() < time.Second {
		errs = append(errs, "verification.timeout: must be at least 1s")
	}
	if v.MemoryMB < 128 {
		errs = append(errs, "verification.memory_mb: must be at least 128")
	}
	if v.CPUs <= 0 || v.CPUs > 64 {
		errs = append(errs, "verification.cpus: must be in (0, 64]")
	}
	if v.PidsLimit < 64 {
		errs = append(errs, "verification.pids_limit: must be at least 64")
	}
	if v.TableHashMaxRows < 0 {
		errs = append(errs, "verification.table_hash_max_rows: must not be negative")
	}
	seen := map[string]bool{}
	for i, q := range v.Queries {
		if !namePattern.MatchString(q.Name) {
			errs = append(errs, fmt.Sprintf("verification.queries[%d].name: invalid", i))
		}
		if seen[q.Name] {
			errs = append(errs, fmt.Sprintf("verification.queries[%d].name: duplicate %q", i, q.Name))
		}
		seen[q.Name] = true
		if strings.TrimSpace(q.SQL) == "" {
			errs = append(errs, fmt.Sprintf("verification.queries[%d].sql: required", i))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n  - "))
	}
	return nil
}

// ResolvePassword returns the source password from the configured secret
// source. An empty password is allowed only when neither source is set
// (e.g. trust/peer authentication or a user-managed ~/.pgpass).
func (s *Source) ResolvePassword() (string, error) {
	switch {
	case s.PasswordEnv != "":
		v, ok := os.LookupEnv(s.PasswordEnv)
		if !ok {
			return "", fmt.Errorf("environment variable %s (source.password_env) is not set", s.PasswordEnv)
		}
		return v, nil
	case s.PasswordFile != "":
		return security.ReadSecretFile(s.PasswordFile)
	default:
		return "", nil
	}
}

// Redacted returns a copy of the source safe to persist (no secret values;
// only the name of the secret source is kept).
func (s Source) Redacted() map[string]any {
	return map[string]any{
		"name": s.Name, "type": s.Type, "host": s.Host, "port": s.Port,
		"database": s.Database, "user": s.User, "sslmode": s.SSLMode,
		"password_env": s.PasswordEnv, "password_file": s.PasswordFile,
	}
}
