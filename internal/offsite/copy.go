package offsite

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/unmadethings/unthrecover/internal/buildinfo"
	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/manifest"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/repo"
	"github.com/unmadethings/unthrecover/internal/retention"
)

// Object names inside one copy.
const (
	DescriptorName = "copy.json"
	SignatureName  = "copy.json.sig"
	encSuffix      = ".age"
	// DescriptorFormat is the copy.json schema version.
	DescriptorFormat = 1
	maxDescriptor    = 1 << 20
)

// Object describes one stored object of a copy.
type Object struct {
	Role        string `json:"role"` // artifact | manifest
	Name        string `json:"name"` // object name inside the copy
	Size        int64  `json:"size"` // stored (encrypted) size
	SHA256      string `json:"sha256"`
	PlainSize   int64  `json:"plaintext_size"`
	PlainSHA256 string `json:"plaintext_sha256"`
}

// Descriptor is copy.json: what was stored, how, and by whom.
type Descriptor struct {
	Format      int       `json:"format"`
	Tool        string    `json:"tool"`
	ToolVersion string    `json:"tool_version"`
	BackupID    string    `json:"backup_id"`
	WorkloadID  string    `json:"workload_id"`
	BackupType  string    `json:"backup_type"`
	CreatedAt   time.Time `json:"backup_created_at"`
	CopiedAt    time.Time `json:"copied_at"`
	Encrypted   bool      `json:"encrypted"`
	Recipients  []string  `json:"recipients,omitempty"`
	Signed      bool      `json:"signed"`
	SignedBy    string    `json:"signed_by,omitempty"`
	Objects     []Object  `json:"objects"`
}

func (d *Descriptor) object(role string) (*Object, bool) {
	for i := range d.Objects {
		if d.Objects[i].Role == role {
			return &d.Objects[i], true
		}
	}
	return nil, false
}

// Copier performs offsite operations for one job.
type Copier struct {
	Backend Backend
	Keys    *Keys
	Prefix  string
	Store   *metadata.Store
	Repo    *repo.Repo
	Log     *slog.Logger
	Actor   string
	Now     func() time.Time
	// TZ names the dated copy folders (default: the system time zone).
	TZ *time.Location
}

func (c *Copier) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

var (
	idPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	monthPattern = regexp.MustCompile(`^\d{4}-\d{2}$`)
	datePattern  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}\.\d{2}(-\d{1,3})?$`)
)

// Location returns where copies were stored before v0.5:
// <prefix>/<workload>/<backup id>. Such copies stay readable.
func (c *Copier) Location(workload, backupID string) (string, error) {
	if !idPattern.MatchString(workload) || !idPattern.MatchString(backupID) {
		return "", fmt.Errorf("invalid workload or backup id")
	}
	return path.Join(c.Prefix, workload, backupID), nil
}

// newLocation picks a free, readable folder for a new copy:
// <prefix>/<workload>/<YYYY-MM>/<YYYY-MM-DD HH.MM>, in the copier's time zone,
// with -2, -3, ... when two backups fall in the same minute. What a copy is
// comes from its signed copy.json, never from the folder name.
func (c *Copier) newLocation(ctx context.Context, b *metadata.Backup) (string, error) {
	if !idPattern.MatchString(b.WorkloadID) || !idPattern.MatchString(b.ID) {
		return "", fmt.Errorf("invalid workload or backup id")
	}
	tz := c.TZ
	if tz == nil {
		tz = time.Local
	}
	t := b.CreatedAt.In(tz)
	dir := path.Join(c.Prefix, b.WorkloadID, t.Format("2006-01"))
	name := t.Format("2006-01-02 15.04")
	for i := 1; i <= 999; i++ {
		loc := path.Join(dir, name)
		if i > 1 {
			loc = path.Join(dir, fmt.Sprintf("%s-%d", name, i))
		}
		keys, err := c.Backend.List(ctx, loc+"/")
		if err != nil {
			return "", failure.New(failure.OffsiteFailed, "list "+c.Backend.Target(), err)
		}
		if len(keys) == 0 {
			return loc, nil
		}
	}
	return "", failure.New(failure.OffsiteFailed, "no free copy folder in "+dir, nil)
}

// locationOf returns where this backup's copy is: the recorded location, or
// the pre-v0.5 one.
func (c *Copier) locationOf(ctx context.Context, b *metadata.Backup) (string, error) {
	if rec, err := c.Store.GetCopy(ctx, b.ID, c.Backend.Target()); err == nil && rec.Location != "" {
		return rec.Location, nil
	}
	return c.Location(b.WorkloadID, b.ID)
}

// placedRight reports whether loc is a valid folder for the copy d: the old
// <workload>/<backup id> or the dated <workload>/<YYYY-MM>/<date> layout.
func placedRight(loc string, d *Descriptor) bool {
	if path.Base(loc) == d.BackupID && path.Base(path.Dir(loc)) == d.WorkloadID {
		return true
	}
	month := path.Dir(loc)
	return datePattern.MatchString(path.Base(loc)) && monthPattern.MatchString(path.Base(month)) &&
		strings.HasPrefix(path.Base(loc), path.Base(month)+"-") && path.Base(path.Dir(month)) == d.WorkloadID
}

func (c *Copier) audit(ctx context.Context, action, target, outcome, details string) {
	if c.Store != nil {
		_ = c.Store.Audit(context.WithoutCancel(ctx), metadata.AuditEvent{Actor: c.Actor, Action: action,
			Target: target, Outcome: outcome, Details: details})
	}
}

// Copy uploads a VERIFIED backup and then proves the copy with Check. The
// returned record reflects the final state; on error it is FAILED.
func (c *Copier) Copy(ctx context.Context, b *metadata.Backup) (*metadata.OffsiteCopy, error) {
	if b.Status != metadata.BackupCreated || b.VerificationStatus != metadata.VerifyVerified {
		return nil, failure.New(failure.OffsiteFailed, fmt.Sprintf("backup %s is %s/%s; only VERIFIED backups are copied offsite",
			b.ID, b.Status, b.VerificationStatus), nil)
	}
	var loc string
	var err error
	if old, gerr := c.Store.GetCopy(ctx, b.ID, c.Backend.Target()); gerr == nil && old.Location != "" && old.Status != metadata.CopyDeleted {
		loc = old.Location // a retry overwrites the same folder
	} else if loc, err = c.newLocation(ctx, b); err != nil {
		return nil, err
	}
	rec := &metadata.OffsiteCopy{BackupID: b.ID, Target: c.Backend.Target(), Location: loc, Status: metadata.CopyUploading,
		Encrypted: c.Keys.Identity != nil, Signed: c.Keys.SignKey != nil, StartedAt: c.now()}
	if err := c.Store.UpsertCopy(ctx, rec); err != nil {
		return nil, err
	}
	c.audit(ctx, "offsite.copy.start", b.ID, "started", rec.Target+"/"+loc)
	fail := func(err error) (*metadata.OffsiteCopy, error) {
		rec.Status = metadata.CopyFailed
		rec.ErrorCategory = string(failure.CategoryOf(err))
		if rec.ErrorCategory == string(failure.Internal) {
			rec.ErrorCategory = string(failure.OffsiteFailed)
		}
		rec.ErrorMessage = err.Error()
		_ = c.Store.UpsertCopy(context.WithoutCancel(ctx), rec)
		c.audit(ctx, "offsite.copy.finish", b.ID, "failed", rec.ErrorCategory)
		return rec, err
	}

	if err := c.upload(ctx, b, loc, rec); err != nil {
		// Remove whatever was uploaded; a copy without copy.json is never used anyway.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		_ = c.deleteAll(dctx, loc)
		cancel()
		return fail(err)
	}
	up := c.now()
	rec.Status, rec.UploadedAt = metadata.CopyUploaded, &up
	if err := c.Store.UpsertCopy(ctx, rec); err != nil {
		return fail(err)
	}
	if _, err := c.check(ctx, b, loc, io.Discard, nil); err != nil {
		return fail(err)
	}
	ck := c.now()
	rec.Status, rec.CheckedAt = metadata.CopyVerified, &ck
	rec.ErrorCategory, rec.ErrorMessage = "", ""
	if err := c.Store.UpsertCopy(ctx, rec); err != nil {
		return fail(err)
	}
	c.audit(ctx, "offsite.copy.finish", b.ID, "verified", fmt.Sprintf("%s/%s encrypted=%v signed=%v", rec.Target, loc, rec.Encrypted, rec.Signed))
	return rec, nil
}

func (c *Copier) upload(ctx context.Context, b *metadata.Backup, loc string, rec *metadata.OffsiteCopy) error {
	artAbs, err := c.Repo.Resolve(b.ArtifactPath)
	if err != nil {
		return failure.New(failure.ArtifactMissing, "resolve artifact", err)
	}
	manAbs, err := c.Repo.Resolve(b.ManifestPath)
	if err != nil {
		return failure.New(failure.ArtifactMissing, "resolve manifest", err)
	}
	d := &Descriptor{Format: DescriptorFormat, Tool: "unth-recover", ToolVersion: buildinfo.Version, BackupID: b.ID,
		WorkloadID: b.WorkloadID, BackupType: b.BackupType, CreatedAt: b.CreatedAt, CopiedAt: c.now(),
		Encrypted: c.Keys.Identity != nil, Recipients: c.Keys.RecipientS, Signed: c.Keys.SignKey != nil}
	if d.Signed {
		d.SignedBy = Fingerprint(c.Keys.VerifyKey)
	}
	for _, f := range []struct{ role, abs string }{{"artifact", artAbs}, {"manifest", manAbs}} {
		name := filepath.Base(f.abs)
		if d.Encrypted {
			name += encSuffix
		}
		obj, err := c.putFile(ctx, path.Join(loc, name), f.abs)
		if err != nil {
			return err
		}
		obj.Role, obj.Name = f.role, name
		d.Objects = append(d.Objects, *obj)
	}
	art, _ := d.object("artifact")
	if art.PlainSHA256 != b.SHA256 {
		return failure.New(failure.ChecksumMismatch, "local artifact no longer matches its recorded SHA-256; not copying a corrupted backup", nil)
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := c.Backend.Put(ctx, path.Join(loc, DescriptorName), bytes.NewReader(raw), int64(len(raw))); err != nil {
		return failure.New(failure.OffsiteFailed, "upload "+DescriptorName, err)
	}
	if d.Signed {
		sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(c.Keys.SignKey, raw)) + "\n")
		if err := c.Backend.Put(ctx, path.Join(loc, SignatureName), bytes.NewReader(sig), int64(len(sig))); err != nil {
			return failure.New(failure.OffsiteFailed, "upload "+SignatureName, err)
		}
	}
	rec.SizeBytes, rec.ObjectSHA256 = art.Size, art.SHA256
	return nil
}

type countingHash struct {
	h hash.Hash
	n int64
}

func newCountingHash() *countingHash { return &countingHash{h: sha256.New()} }
func (c *countingHash) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return c.h.Write(p)
}
func (c *countingHash) sum() string { return hex.EncodeToString(c.h.Sum(nil)) }

// putFile streams a local file to key, encrypting on the fly when keys are
// set. No plaintext temporary copy is created.
func (c *Copier) putFile(ctx context.Context, key, abs string) (*Object, error) {
	f, err := os.Open(abs) // #nosec G304 -- path resolved inside the repository
	if err != nil {
		return nil, failure.New(failure.ArtifactMissing, "open "+filepath.Base(abs), err)
	}
	defer f.Close()
	plain, stored := newCountingHash(), newCountingHash()
	src := io.TeeReader(f, plain)
	if c.Keys.Identity == nil {
		fi, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if err := c.Backend.Put(ctx, key, io.TeeReader(src, stored), fi.Size()); err != nil {
			return nil, failure.New(failure.OffsiteFailed, "upload "+path.Base(key), err)
		}
	} else {
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			w, err := age.Encrypt(pw, c.Keys.Recipients...)
			if err == nil {
				_, err = io.Copy(w, src)
			}
			if err == nil {
				err = w.Close()
			}
			_ = pw.CloseWithError(err)
			done <- err
		}()
		perr := c.Backend.Put(ctx, key, io.TeeReader(pr, stored), -1)
		_ = pr.CloseWithError(errors.New("upload ended"))
		eerr := <-done
		if perr != nil {
			return nil, failure.New(failure.OffsiteFailed, "upload "+path.Base(key), perr)
		}
		if eerr != nil {
			return nil, failure.New(failure.OffsiteFailed, "encrypt "+path.Base(key), eerr)
		}
	}
	return &Object{Size: stored.n, SHA256: stored.sum(), PlainSize: plain.n, PlainSHA256: plain.sum()}, nil
}

// CheckResult is the outcome of checking a remote copy.
type CheckResult struct {
	BackupID         string      `json:"backup_id"`
	Target           string      `json:"target"`
	Location         string      `json:"location"`
	Encrypted        bool        `json:"encrypted"`
	SignatureChecked bool        `json:"signature_checked"`
	SignedBy         string      `json:"signed_by,omitempty"`
	Descriptor       *Descriptor `json:"descriptor"`
}

// Check downloads a copy again and proves it: signature (when a key is
// configured), stored hashes, decryption, plaintext hashes, and that the
// artifact equals the locally recorded SHA-256 of the backup.
func (c *Copier) Check(ctx context.Context, b *metadata.Backup) (*CheckResult, error) {
	loc, err := c.locationOf(ctx, b)
	if err != nil {
		return nil, err
	}
	res, err := c.check(ctx, b, loc, io.Discard, nil)
	rec, gerr := c.Store.GetCopy(ctx, b.ID, c.Backend.Target())
	if gerr == nil {
		t := c.now()
		if err != nil {
			rec.Status, rec.ErrorCategory, rec.ErrorMessage = metadata.CopyFailed, string(failure.CategoryOf(err)), err.Error()
		} else {
			rec.Status, rec.CheckedAt, rec.ErrorCategory, rec.ErrorMessage = metadata.CopyVerified, &t, "", ""
		}
		_ = c.Store.UpsertCopy(context.WithoutCancel(ctx), rec)
	}
	outcome := "verified"
	if err != nil {
		outcome = "failed"
	}
	c.audit(ctx, "offsite.check", b.ID, outcome, c.Backend.Target()+"/"+loc)
	return res, err
}

// check verifies the copy at loc. sinks (artifact, manifest) receive the
// verified plaintext; b may be nil when no local record exists (fetch).
func (c *Copier) check(ctx context.Context, b *metadata.Backup, loc string, artifactSink io.Writer, manifestSink io.Writer) (*CheckResult, error) {
	d, sigChecked, err := c.readDescriptor(ctx, loc)
	if err != nil {
		return nil, err
	}
	res := &CheckResult{BackupID: d.BackupID, Target: c.Backend.Target(), Location: loc, Encrypted: d.Encrypted,
		SignatureChecked: sigChecked, SignedBy: d.SignedBy, Descriptor: d}
	if !placedRight(loc, d) {
		return nil, failure.New(failure.OffsiteMismatch, "copy.json names a different backup than its location", nil)
	}
	if b != nil && (d.BackupID != b.ID || d.WorkloadID != b.WorkloadID) {
		return nil, failure.New(failure.OffsiteMismatch, "copy.json does not belong to this backup", nil)
	}
	if c.Keys.Identity != nil && !d.Encrypted {
		return nil, failure.New(failure.OffsiteMismatch, "encryption is configured but the remote copy is not encrypted", nil)
	}
	if d.Encrypted && c.Keys.Identity == nil {
		return nil, failure.New(failure.DecryptionFailed, "the copy is encrypted; set encryption.identity_file", nil)
	}
	art, ok1 := d.object("artifact")
	man, ok2 := d.object("manifest")
	if !ok1 || !ok2 || len(d.Objects) != 2 {
		return nil, failure.New(failure.OffsiteMismatch, "copy.json must list exactly one artifact and one manifest", nil)
	}
	if b != nil && art.PlainSHA256 != b.SHA256 {
		return nil, failure.New(failure.OffsiteMismatch, "remote artifact hash differs from the backup's recorded SHA-256", nil)
	}
	if manifestSink == nil {
		manifestSink = io.Discard
	}
	if err := c.getObject(ctx, loc, art, d.Encrypted, artifactSink); err != nil {
		return nil, err
	}
	var mbuf bytes.Buffer
	if err := c.getObject(ctx, loc, man, d.Encrypted, io.MultiWriter(&mbuf, manifestSink)); err != nil {
		return nil, err
	}
	var m manifest.Manifest
	if err := json.Unmarshal(mbuf.Bytes(), &m); err != nil {
		return nil, failure.New(failure.ManifestInvalid, "remote manifest", err)
	}
	if m.BackupID != d.BackupID || m.WorkloadID != d.WorkloadID || m.Artifact.SHA256 != art.PlainSHA256 ||
		m.Artifact.SizeBytes != art.PlainSize || m.FormatVersion != manifest.FormatVersion {
		return nil, failure.New(failure.OffsiteMismatch, "remote manifest does not match copy.json", nil)
	}
	return res, nil
}

func (c *Copier) readDescriptor(ctx context.Context, loc string) (*Descriptor, bool, error) {
	raw, err := c.readSmall(ctx, path.Join(loc, DescriptorName))
	if err != nil {
		if errors.Is(err, ErrNotExist) {
			return nil, false, failure.New(failure.ArtifactMissing, "no complete copy at "+loc+" (copy.json missing)", err)
		}
		return nil, false, failure.New(failure.OffsiteFailed, "download copy.json", err)
	}
	sigChecked := false
	if c.Keys.VerifyKey != nil {
		sigRaw, err := c.readSmall(ctx, path.Join(loc, SignatureName))
		if err != nil {
			return nil, false, failure.New(failure.SignatureInvalid, "signing is configured but the copy has no valid signature", err)
		}
		sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigRaw)))
		if err != nil || !ed25519.Verify(c.Keys.VerifyKey, raw, sig) {
			return nil, false, failure.New(failure.SignatureInvalid, "copy.json signature does not verify with the configured key", nil)
		}
		sigChecked = true
	}
	var d Descriptor
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, false, failure.New(failure.ManifestInvalid, "parse copy.json", err)
	}
	if d.Format != DescriptorFormat {
		return nil, false, failure.New(failure.ManifestInvalid, fmt.Sprintf("unsupported copy.json format %d", d.Format), nil)
	}
	if !idPattern.MatchString(d.BackupID) || !idPattern.MatchString(d.WorkloadID) {
		return nil, false, failure.New(failure.ManifestInvalid, "copy.json has invalid ids", nil)
	}
	for _, o := range d.Objects {
		if o.Name == "" || path.Base(o.Name) != o.Name || o.Name == DescriptorName || o.Name == SignatureName ||
			strings.HasPrefix(o.Name, ".") {
			return nil, false, failure.New(failure.ManifestInvalid, "copy.json has an invalid object name", nil)
		}
	}
	if sigChecked && d.SignedBy != Fingerprint(c.Keys.VerifyKey) {
		return nil, false, failure.New(failure.SignatureInvalid, "copy.json signed_by does not match the configured key", nil)
	}
	return &d, sigChecked, nil
}

func (c *Copier) readSmall(ctx context.Context, key string) ([]byte, error) {
	rc, err := c.Backend.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, maxDescriptor+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxDescriptor {
		return nil, fmt.Errorf("%s is too large", path.Base(key))
	}
	return raw, nil
}

// getObject downloads one object, checks its stored hash, decrypts it when
// needed and checks the plaintext hash; plaintext goes to sink.
func (c *Copier) getObject(ctx context.Context, loc string, o *Object, encrypted bool, sink io.Writer) error {
	rc, err := c.Backend.Get(ctx, path.Join(loc, o.Name))
	if err != nil {
		if errors.Is(err, ErrNotExist) {
			return failure.New(failure.ArtifactMissing, "remote object "+o.Name+" is missing", err)
		}
		return failure.New(failure.OffsiteFailed, "download "+o.Name, err)
	}
	defer rc.Close()
	stored, plain := newCountingHash(), newCountingHash()
	var r io.Reader = io.TeeReader(rc, stored)
	if encrypted {
		dr, err := age.Decrypt(r, c.Keys.Identity)
		if err != nil {
			return failure.New(failure.DecryptionFailed, "decrypt "+o.Name+" (wrong key or modified object)", err)
		}
		r = dr
	}
	if _, err := io.Copy(io.MultiWriter(sink, plain), r); err != nil {
		if encrypted {
			return failure.New(failure.DecryptionFailed, "decrypt "+o.Name+" (object modified or truncated)", err)
		}
		return failure.New(failure.OffsiteFailed, "download "+o.Name, err)
	}
	// Drain anything after the age stream so the stored hash covers the whole object.
	if _, err := io.Copy(io.Discard, io.TeeReader(rc, stored)); err != nil {
		return failure.New(failure.OffsiteFailed, "download "+o.Name, err)
	}
	if stored.n != o.Size || stored.sum() != o.SHA256 {
		return failure.New(failure.OffsiteMismatch, "remote object "+o.Name+" differs from copy.json (stored hash)", nil)
	}
	if plain.n != o.PlainSize || plain.sum() != o.PlainSHA256 {
		return failure.New(failure.OffsiteMismatch, "remote object "+o.Name+" differs from copy.json (content hash)", nil)
	}
	return nil
}

// RemoteCopy is one complete copy found on the target.
type RemoteCopy struct {
	WorkloadID string    `json:"workload_id"`
	BackupID   string    `json:"backup_id"`
	CreatedAt  time.Time `json:"backup_created_at"`
	CopiedAt   time.Time `json:"copied_at"`
	SizeBytes  int64     `json:"size_bytes"`
	Encrypted  bool      `json:"encrypted"`
	Signed     bool      `json:"signed"`
	Location   string    `json:"location"`
	Error      string    `json:"error,omitempty"`
}

// List returns the complete copies on the target (optionally one workload),
// newest first. Signatures are checked when a key is configured.
func (c *Copier) List(ctx context.Context, workload string) ([]RemoteCopy, error) {
	base := c.Prefix
	if workload != "" {
		if !idPattern.MatchString(workload) {
			return nil, fmt.Errorf("invalid workload")
		}
		base = path.Join(c.Prefix, workload)
	}
	listPrefix := base
	if listPrefix != "" {
		listPrefix += "/"
	}
	keys, err := c.Backend.List(ctx, listPrefix)
	if err != nil {
		return nil, failure.New(failure.OffsiteFailed, "list "+c.Backend.Target(), err)
	}
	var out []RemoteCopy
	for _, k := range keys {
		if path.Base(k) != DescriptorName {
			continue
		}
		loc := path.Dir(k)
		rc := RemoteCopy{Location: loc, BackupID: path.Base(loc), WorkloadID: path.Base(path.Dir(loc))}
		if datePattern.MatchString(rc.BackupID) {
			rc.BackupID, rc.WorkloadID = "", path.Base(path.Dir(path.Dir(loc)))
		}
		d, _, err := c.readDescriptor(ctx, loc)
		switch {
		case err != nil:
			rc.Error = err.Error()
		case !placedRight(loc, d):
			rc.Error = "copy.json names a different backup than its location"
		default:
			rc.BackupID, rc.WorkloadID = d.BackupID, d.WorkloadID
			rc.CreatedAt, rc.CopiedAt, rc.Encrypted, rc.Signed = d.CreatedAt, d.CopiedAt, d.Encrypted, d.Signed
			if a, ok := d.object("artifact"); ok {
				rc.SizeBytes = a.Size
			}
		}
		out = append(out, rc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (c *Copier) deleteAll(ctx context.Context, loc string) error {
	keys, err := c.Backend.List(ctx, loc+"/")
	if err != nil {
		return err
	}
	// copy.json first: from then on the copy is "incomplete", never "corrupt".
	sort.SliceStable(keys, func(i, j int) bool { return path.Base(keys[i]) == DescriptorName })
	for _, k := range keys {
		if path.Dir(k) != loc {
			continue // only objects of exactly this copy
		}
		if err := c.Backend.Delete(ctx, k); err != nil {
			return err
		}
	}
	// Folder-based storage (Google Drive) keeps empty folders: remove the
	// copy's folder and, when that empties it, the month folder.
	if p, ok := c.Backend.(interface {
		RemoveEmptyDir(ctx context.Context, dir string) (bool, error)
	}); ok {
		if gone, err := p.RemoveEmptyDir(ctx, loc); err == nil && gone && monthPattern.MatchString(path.Base(path.Dir(loc))) {
			_, _ = p.RemoveEmptyDir(ctx, path.Dir(loc))
		}
	}
	return nil
}

// PruneResult summarizes offsite retention.
type PruneResult struct {
	Target    string               `json:"target"`
	Decisions []retention.Decision `json:"decisions"`
	Deleted   []string             `json:"deleted"`
	Errors    []string             `json:"errors,omitempty"`
}

// Prune applies offsite retention to the REMOTE_VERIFIED copies of a
// workload. The rules work like local retention; the newest copy is always kept.
func (c *Copier) Prune(ctx context.Context, workload string, p config.Retention, dryRun bool) (*PruneResult, error) {
	res := &PruneResult{Target: c.Backend.Target(), Deleted: []string{}}
	if !p.Enabled() {
		return res, nil
	}
	copies, err := c.Store.ListCopiesByWorkload(ctx, workload, c.Backend.Target())
	if err != nil {
		return nil, err
	}
	var pseudo []*metadata.Backup
	byID := map[string]*metadata.OffsiteCopy{}
	for _, cp := range copies {
		if cp.Status != metadata.CopyVerified {
			continue
		}
		b, err := c.Store.GetBackup(ctx, cp.BackupID)
		if err != nil {
			continue
		}
		// The copy itself was proven, whatever happened to the local artifact.
		pb := *b
		pb.Status, pb.VerificationStatus = metadata.BackupCreated, metadata.VerifyVerified
		pseudo = append(pseudo, &pb)
		byID[cp.BackupID] = cp
	}
	res.Decisions = retention.Plan(pseudo, p, c.now())
	for _, d := range res.Decisions {
		if d.Keep {
			continue
		}
		if dryRun {
			res.Deleted = append(res.Deleted, d.BackupID)
			continue
		}
		cp := byID[d.BackupID]
		if err := c.deleteAll(ctx, cp.Location); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", d.BackupID, err))
			continue
		}
		t := c.now()
		cp.Status, cp.DeletedAt = metadata.CopyDeleted, &t
		if err := c.Store.UpsertCopy(ctx, cp); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", d.BackupID, err))
			continue
		}
		c.audit(ctx, "offsite.prune", d.BackupID, "deleted", fmt.Sprintf("%s/%s reasons=%v", cp.Target, cp.Location, d.Reasons))
		res.Deleted = append(res.Deleted, d.BackupID)
	}
	if len(res.Errors) > 0 {
		return res, fmt.Errorf("offsite prune incomplete: %v", res.Errors)
	}
	return res, nil
}

// Fetch downloads and proves a copy and imports it into the local
// repository as a NOT_VERIFIED backup (run `unth verify` next). It is the
// disaster-recovery path on a new machine.
func (c *Copier) Fetch(ctx context.Context, workload, backupID string) (*metadata.Backup, *CheckResult, error) {
	loc, err := c.findCopy(ctx, workload, backupID)
	if err != nil {
		return nil, nil, err
	}
	if b, err := c.Store.GetBackup(ctx, backupID); err == nil && b.Status != metadata.BackupExpired {
		return nil, nil, fmt.Errorf("backup %s already exists locally (%s)", backupID, b.Status)
	}
	d, _, err := c.readDescriptor(ctx, loc)
	if err != nil {
		return nil, nil, err
	}
	art, ok := d.object("artifact")
	man, ok2 := d.object("manifest")
	if !ok || !ok2 {
		return nil, nil, failure.New(failure.OffsiteMismatch, "copy.json must list an artifact and a manifest", nil)
	}
	if free, err := repo.FreeBytes(c.Repo.Root); err == nil && free < art.PlainSize+(64<<20) {
		return nil, nil, failure.New(failure.InsufficientStorage, fmt.Sprintf("need %d bytes free in the data directory", art.PlainSize), nil)
	}
	stage, err := os.MkdirTemp(filepath.Join(c.Repo.Root, "tmp"), backupID+".fetch-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(stage)
	artName := strings.TrimSuffix(art.Name, encSuffix)
	manName := strings.TrimSuffix(man.Name, encSuffix)
	if artName != manifest.ArtifactName && artName != manifest.VolumeArtifactName || manName != manifest.FileName {
		return nil, nil, failure.New(failure.ManifestInvalid, "unexpected object names in copy.json", nil)
	}
	af, err := os.OpenFile(filepath.Join(stage, artName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- private staging dir
	if err != nil {
		return nil, nil, err
	}
	mf, err := os.OpenFile(filepath.Join(stage, manName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- private staging dir
	if err != nil {
		_ = af.Close()
		return nil, nil, err
	}
	res, cerr := c.check(ctx, nil, loc, af, mf)
	e1, e2 := af.Sync(), mf.Sync()
	_ = af.Close()
	_ = mf.Close()
	if cerr != nil {
		c.audit(ctx, "offsite.fetch", backupID, "failed", c.Backend.Target()+"/"+loc)
		return nil, nil, cerr
	}
	if e1 != nil || e2 != nil {
		return nil, nil, fmt.Errorf("write fetched files: %v %v", e1, e2)
	}
	m, err := manifest.Read(filepath.Join(stage, manName))
	if err != nil {
		return nil, nil, failure.New(failure.ManifestInvalid, "fetched manifest", err)
	}
	if m.Artifact.Path != artName {
		return nil, nil, failure.New(failure.ManifestInvalid, "manifest artifact name differs from the fetched object", nil)
	}
	dir, err := c.Repo.BackupDir(backupID)
	if err != nil {
		return nil, nil, err
	}
	_ = os.RemoveAll(dir) // only an EXPIRED backup's (empty) directory can exist here
	if err := os.Rename(stage, dir); err != nil {
		return nil, nil, err
	}
	_ = os.Chmod(dir, 0o700) // #nosec G302 -- a directory needs the x bit; owner-only like every backup dir
	b, err := c.importRecord(ctx, m, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	t := c.now()
	_ = c.Store.UpsertCopy(ctx, &metadata.OffsiteCopy{BackupID: b.ID, Target: c.Backend.Target(), Location: loc,
		Status: metadata.CopyVerified, Encrypted: d.Encrypted, Signed: d.Signed, SizeBytes: art.Size,
		ObjectSHA256: art.SHA256, StartedAt: d.CopiedAt, UploadedAt: &d.CopiedAt, CheckedAt: &t})
	c.audit(ctx, "offsite.fetch", b.ID, "imported", fmt.Sprintf("%s/%s signature_checked=%v", c.Backend.Target(), loc, res.SignatureChecked))
	return b, res, nil
}

// findCopy locates a backup's copy on the target: the pre-v0.5 folder, or
// the dated folder whose copy.json names this backup.
func (c *Copier) findCopy(ctx context.Context, workload, backupID string) (string, error) {
	legacy, err := c.Location(workload, backupID)
	if err != nil {
		return "", err
	}
	if keys, err := c.Backend.List(ctx, legacy+"/"); err == nil && len(keys) > 0 {
		return legacy, nil
	}
	list, err := c.List(ctx, workload)
	if err != nil {
		return "", err
	}
	for _, rc := range list {
		if rc.BackupID == backupID && rc.WorkloadID == workload {
			return rc.Location, nil
		}
	}
	unreadable := 0
	for _, rc := range list {
		if rc.Error != "" {
			unreadable++
		}
	}
	msg := fmt.Sprintf("no copy of %s on %s", backupID, c.Backend.Target())
	if unreadable > 0 {
		msg += fmt.Sprintf(" (%d copies could not be read; see `unth offsite list`)", unreadable)
	}
	return "", failure.New(failure.ArtifactMissing, msg, nil)
}

func (c *Copier) importRecord(ctx context.Context, m *manifest.Manifest, dir string) (*metadata.Backup, error) {
	srcJSON, _ := json.Marshal(m.Source)
	verJSON, _ := json.Marshal(m.Verification)
	var fpJSON []byte
	switch {
	case m.Fingerprint != nil:
		fpJSON, _ = json.Marshal(m.Fingerprint)
	case m.VolumeFiles != nil:
		fpJSON, _ = json.Marshal(m.VolumeFiles)
	}
	relA, _ := c.Repo.Rel(filepath.Join(dir, m.Artifact.Path))
	relM, _ := c.Repo.Rel(filepath.Join(dir, manifest.FileName))
	now := c.now()
	snap := m.SnapshotAt
	b := &metadata.Backup{ID: m.BackupID, WorkloadID: m.WorkloadID, BackupType: m.BackupType,
		Status: metadata.BackupCreated, VerificationStatus: metadata.VerifyNone, CreatedAt: m.CreatedAt,
		CompletedAt: &now, SnapshotAt: &snap, SourceJSON: string(srcJSON), SourceFingerprint: m.FingerprintDigest,
		FingerprintJSON: string(fpJSON), VerificationJSON: string(verJSON), ArtifactPath: relA, ManifestPath: relM,
		SizeBytes: m.Artifact.SizeBytes, SHA256: m.Artifact.SHA256, ServerVersion: m.ServerVersion,
		ServerMajor: m.ServerMajor, ToolVersion: m.DumpToolVersion}
	if _, err := c.Store.GetBackup(ctx, b.ID); err != nil {
		// InsertBackup records the identity only; all other fields are written by UpdateBackup.
		if err := c.Store.InsertBackup(ctx, b); err != nil {
			return nil, err
		}
	}
	return b, c.Store.UpdateBackup(ctx, b)
}
