// Package manifest defines the backup manifest written next to every
// artifact, plus SHA-256 helpers.
//
// SECURITY NOTE: the SHA-256 checksum provides integrity against accidental
// corruption. It is NOT an authenticity proof: anyone able to modify the
// artifact can also rewrite the manifest. Unth Recover additionally compares
// against the checksum stored in the separate metadata database. Offsite copies
// are encrypted and signed (internal/offsite). See docs/BACKUP_SECURITY.md.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/postgres"
	"github.com/unmadethings/unthrecover/internal/volume"
)

// FormatVersion is the manifest schema version.
const FormatVersion = 1

// MaxManifestBytes bounds manifest parsing.
const MaxManifestBytes = 32 << 20

// FileName is the manifest file name inside a backup directory.
const FileName = "manifest.json"

// ArtifactName is the artifact file name inside a backup directory.
const ArtifactName = "artifact.dump"

// VolumeArtifactName is the artifact name of docker-volume backups.
const VolumeArtifactName = "volume.tar"

// Artifact describes the backup payload.
type Artifact struct {
	Path      string `json:"path"` // relative to the backup directory
	Format    string `json:"format"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	TOCItems  int    `json:"toc_items"`
}

// Integrity documents what the checksum does and does not prove.
type Integrity struct {
	Algorithm string `json:"algorithm"`
	Signed    bool   `json:"signed"`
	Encrypted bool   `json:"encrypted"`
	Note      string `json:"note"`
}

// Manifest is the on-disk description of a backup.
type Manifest struct {
	FormatVersion     int                   `json:"format_version"`
	Tool              string                `json:"tool"`
	ToolVersion       string                `json:"tool_version"`
	BackupID          string                `json:"backup_id"`
	WorkloadID        string                `json:"workload_id"`
	BackupType        string                `json:"backup_type"`
	CreatedAt         time.Time             `json:"created_at"`
	SnapshotAt        time.Time             `json:"snapshot_at"`
	Source            map[string]any        `json:"source"`
	ServerVersion     string                `json:"server_version"`
	ServerMajor       int                   `json:"server_major"`
	DumpToolVersion   string                `json:"dump_tool_version"`
	Artifact          Artifact              `json:"artifact"`
	FingerprintDigest string                `json:"fingerprint_digest"`
	Fingerprint       *postgres.Fingerprint `json:"fingerprint,omitempty"`
	VolumeFiles       *volume.FileManifest  `json:"volume_files,omitempty"`
	Warnings          []string              `json:"warnings,omitempty"`
	Verification      config.Verification   `json:"verification_settings"`
	Integrity         Integrity             `json:"integrity"`
}

// DefaultIntegrity is the integrity statement for M0 backups.
func DefaultIntegrity() Integrity {
	return Integrity{
		Algorithm: "sha256",
		Signed:    false,
		Encrypted: false,
		Note:      "SHA-256 detects accidental corruption only; it is not proof of authenticity. Artifacts are not encrypted.",
	}
}

// Write atomically writes the manifest to path with 0600 permissions.
func Write(path string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- path built from validated backup ID
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Read parses a manifest, enforcing a size bound and the format version.
func Read(path string) (*Manifest, error) {
	f, err := os.Open(path) // #nosec G304 -- path validated by caller
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("manifest is not a regular file")
	}
	if fi.Size() > MaxManifestBytes {
		return nil, fmt.Errorf("manifest exceeds %d bytes", MaxManifestBytes)
	}
	var m Manifest
	dec := json.NewDecoder(io.LimitReader(f, MaxManifestBytes))
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("unsupported manifest format_version %d", m.FormatVersion)
	}
	if filepath.Base(m.Artifact.Path) != m.Artifact.Path {
		return nil, fmt.Errorf("manifest artifact path %q must be a plain file name", m.Artifact.Path)
	}
	return &m, nil
}

// HashFile returns the SHA-256 (hex) and size of a file.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path) // #nosec G304 -- path validated by caller
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
