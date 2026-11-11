package config

import (
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Offsite copies every VERIFIED backup to storage outside this server:
// S3-compatible object storage (AWS S3, Google Cloud Storage, Cloudflare R2,
// Backblaze B2, Wasabi, Hetzner, MinIO, ...) or any rclone remote (Google
// Drive, OneDrive, Dropbox, SFTP, ...). Copies are encrypted with age before
// they leave the server unless allow_unencrypted is set, and each copy is
// downloaded again and checked after the upload.
type Offsite struct {
	Type string `yaml:"type"` // s3 | rclone | gdrive

	// type: gdrive — a Google Drive connected in the panel (Settings → Connections)
	Connection string `yaml:"connection"`

	// type: s3
	Endpoint     string `yaml:"endpoint"`       // e.g. https://storage.googleapis.com (default: AWS S3)
	Region       string `yaml:"region"`         // e.g. us-east-1, auto
	Bucket       string `yaml:"bucket"`         // must exist
	AccessKeyEnv string `yaml:"access_key_env"` // env var with the access key ID
	SecretKeyEnv string `yaml:"secret_key_env"` // env var with the secret key
	PathStyle    bool   `yaml:"path_style"`     // path-style URLs (MinIO, SeaweedFS, Garage)
	AllowHTTP    bool   `yaml:"allow_http"`     // plain http to a non-loopback endpoint

	// type: rclone
	Remote       string `yaml:"remote"`        // e.g. gdrive:unth-backups
	RcloneConfig string `yaml:"rclone_config"` // path to rclone.conf (default: rclone's own)
	RcloneBinary string `yaml:"rclone_binary"` // default: rclone from PATH

	Prefix           string    `yaml:"prefix"`            // key prefix inside the bucket/remote
	Timeout          Duration  `yaml:"timeout"`           // per copy (upload + check)
	AllowUnencrypted bool      `yaml:"allow_unencrypted"` // explicitly allow plaintext copies
	Retention        Retention `yaml:"retention"`         // default: same as the job's retention
	// TimeZone names the dated copy folders (<job>/2026-11/2026-11-11 17.30),
	// e.g. Europe/Istanbul. Default: the server's time zone.
	TimeZone string `yaml:"time_zone"`
}

// Location returns the time zone for copy folder names (nil: the server's).
func (o Offsite) Location() (*time.Location, error) {
	if o.TimeZone == "" {
		return nil, nil
	}
	return time.LoadLocation(o.TimeZone)
}

// Offsite types.
const (
	OffsiteS3     = "s3"
	OffsiteRclone = "rclone"
	OffsiteGDrive = "gdrive"
)

// Enabled reports whether offsite copies are configured.
func (o Offsite) Enabled() bool { return o.Type != "" }

// Encryption: copies are encrypted to the age identity in identity_file (its
// public key is derived) plus any extra recipients, e.g. an offline recovery
// key kept away from this server.
type Encryption struct {
	IdentityFile string   `yaml:"identity_file"` // age secret key, 0600 (unth keys generate)
	Recipients   []string `yaml:"recipients"`    // extra age public keys (age1...)
}

// Enabled reports whether encryption is configured.
func (e Encryption) Enabled() bool { return e.IdentityFile != "" }

// Signing: copy descriptors are signed with an Ed25519 key so a restore can
// prove the copy was made by this installation and not modified.
type Signing struct {
	KeyFile       string `yaml:"key_file"`        // Ed25519 private key, 0600 (unth keys generate)
	PublicKeyFile string `yaml:"public_key_file"` // to check copies on a machine without the private key
}

// Enabled reports whether signing or signature checking is configured.
func (s Signing) Enabled() bool { return s.KeyFile != "" || s.PublicKeyFile != "" }

var (
	bucketPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,61}[a-z0-9]$`)
	prefixPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	remotePattern  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_. -]*:[^\x00-\x1f]*$`)
	agePublicKey   = regexp.MustCompile(`^age1[02-9ac-hj-np-z]{58}$`)
	regionPattern  = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	connPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	endpointSchema = map[string]bool{"http": true, "https": true}
)

func (o *Offsite) applyDefaults(job Retention) {
	if !o.Enabled() {
		return
	}
	if o.Timeout == 0 {
		o.Timeout = Duration(6 * time.Hour)
	}
	if o.Type == OffsiteS3 && o.Endpoint == "" {
		o.Endpoint = "https://s3.amazonaws.com"
	}
	if !o.Retention.Enabled() {
		o.Retention = job
	}
	o.Prefix = strings.Trim(o.Prefix, "/")
}

func (c *Config) validateOffsite(add func(string, ...any)) {
	o := c.Offsite
	if c.Encryption.Enabled() || len(c.Encryption.Recipients) > 0 {
		if c.Encryption.IdentityFile == "" {
			add("encryption.identity_file: required (unth keys generate)")
		}
		for i, r := range c.Encryption.Recipients {
			if !agePublicKey.MatchString(r) {
				add("encryption.recipients[%d]: must be an age public key (age1...)", i)
			}
		}
	}
	if c.Signing.KeyFile != "" && c.Signing.PublicKeyFile != "" {
		add("signing: set key_file (to sign) or public_key_file (to check only), not both")
	}
	if !o.Enabled() {
		return
	}
	if o.Prefix != "" && (!prefixPattern.MatchString(o.Prefix) || strings.Contains(o.Prefix, "..") || strings.Contains(o.Prefix, "//")) {
		add("offsite.prefix: letters, digits and ._-/ only, no '..'")
	}
	if !c.Encryption.Enabled() && !o.AllowUnencrypted {
		add("offsite: copies leave this server, so encryption.identity_file is required (or set offsite.allow_unencrypted: true)")
	}
	if o.Timeout.D() < time.Minute || o.Timeout.D() > 48*time.Hour {
		add("offsite.timeout: must be between 1m and 48h")
	}
	if o.Retention.MaxAge > 0 && o.Retention.MaxAge.D() < time.Hour {
		add("offsite.retention.max_age: must be at least 1h")
	}
	switch o.Type {
	case OffsiteS3:
		u, err := url.Parse(o.Endpoint)
		if err != nil || !endpointSchema[u.Scheme] || u.Host == "" || (u.Path != "" && u.Path != "/") || u.User != nil || u.RawQuery != "" {
			add("offsite.endpoint: must be an http(s) URL without path, e.g. https://storage.googleapis.com")
		} else if u.Scheme == "http" && !o.AllowHTTP && !isLoopbackHost(u.Hostname()) {
			add("offsite.endpoint: https required (set offsite.allow_http: true only for a trusted internal network)")
		}
		if !bucketPattern.MatchString(o.Bucket) {
			add("offsite.bucket: required, a valid bucket name")
		}
		if o.Region != "" && !regionPattern.MatchString(o.Region) {
			add("offsite.region: invalid")
		}
		if !envPattern.MatchString(o.AccessKeyEnv) || !envPattern.MatchString(o.SecretKeyEnv) {
			add("offsite.access_key_env and offsite.secret_key_env: required environment variable names (keys are secrets)")
		}
		if o.Remote != "" || o.RcloneConfig != "" || o.RcloneBinary != "" || o.Connection != "" {
			add("offsite: remote/rclone_*/connection are not used by type s3")
		}
	case OffsiteRclone:
		if !remotePattern.MatchString(o.Remote) || strings.HasPrefix(o.Remote, "-") || strings.Contains(o.Remote, "..") {
			add("offsite.remote: required rclone remote, e.g. gdrive:unth-backups")
		}
		if o.Bucket != "" || o.AccessKeyEnv != "" || o.SecretKeyEnv != "" || o.PathStyle || o.AllowHTTP {
			add("offsite: bucket/access_key_env/secret_key_env/path_style/allow_http are only used by type s3")
		}
		if o.Endpoint != "" {
			add("offsite.endpoint: only used by type s3")
		}
	case OffsiteGDrive:
		if !connPattern.MatchString(o.Connection) {
			add("offsite.connection: required, the name of a Google Drive connection made in the panel (e.g. gdrive)")
		}
		if o.Bucket != "" || o.AccessKeyEnv != "" || o.SecretKeyEnv != "" || o.Endpoint != "" || o.Remote != "" || o.RcloneConfig != "" {
			add("offsite: only connection (and prefix, retention, timeout) are used by type gdrive")
		}
	default:
		add("offsite.type: must be %q, %q or %q", OffsiteS3, OffsiteRclone, OffsiteGDrive)
	}
	if _, err := o.Location(); err != nil || strings.ContainsAny(o.TimeZone, " \\") {
		add("offsite.time_zone: unknown time zone %q (use a name like Europe/Istanbul or UTC)", o.TimeZone)
	}
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
