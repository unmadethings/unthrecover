package offsite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/manifest"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/repo"
)

// mem is an in-memory Backend.
type mem struct {
	mu      sync.Mutex
	objs    map[string][]byte
	failPut string // fail Put for keys with this suffix
}

func newMem() *mem { return &mem{objs: map[string][]byte{}} }

func (m *mem) Target() string { return "mem://test" }
func (m *mem) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	if m.failPut != "" && strings.HasSuffix(key, m.failPut) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r, 100))
		return errors.New("simulated network failure")
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.objs[key] = b
	m.mu.Unlock()
	return nil
}
func (m *mem) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
}
func (m *mem) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}
func (m *mem) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.objs, key)
	m.mu.Unlock()
	return nil
}

const secretContent = "CUSTOMER-TABLE-ROW alice@example.com 4111-1111"

type env struct {
	store *metadata.Store
	repo  *repo.Repo
	keys  *Keys
	be    *mem
	c     *Copier
	dir   string
}

func newEnv(t *testing.T, encrypt, sign bool) *env {
	t.Helper()
	ctx := context.Background()
	r, err := repo.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := metadata.Open(ctx, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keyDir := filepath.Join(t.TempDir(), "keys")
	if _, err := GenerateKeys(keyDir); err != nil {
		t.Fatal(err)
	}
	var idf, skf string
	if encrypt {
		idf = filepath.Join(keyDir, AgeKeyFile)
	}
	if sign {
		skf = filepath.Join(keyDir, SignKeyFile)
	}
	k, err := LoadKeys(idf, nil, skf, "")
	if err != nil {
		t.Fatal(err)
	}
	be := newMem()
	return &env{store: st, repo: r, keys: k, be: be, dir: keyDir,
		c: &Copier{Backend: be, Keys: k, Prefix: "unth", Store: st, Repo: r, Actor: "test"}}
}

// addBackup creates a VERIFIED backup with a real artifact and manifest.
func (e *env) addBackup(t *testing.T, workload string, created time.Time, content string) *metadata.Backup {
	t.Helper()
	id := fmt.Sprintf("bk-%d-%s", created.UnixNano(), workload)
	dir, _ := e.repo.BackupDir(id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	art := filepath.Join(dir, manifest.ArtifactName)
	if err := os.WriteFile(art, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, n, _ := manifest.HashFile(art)
	m := &manifest.Manifest{FormatVersion: manifest.FormatVersion, Tool: "unth-recover", BackupID: id, WorkloadID: workload,
		BackupType: "postgres-logical", CreatedAt: created, SnapshotAt: created,
		Source:    map[string]any{"name": workload, "host": "db.internal", "database": "shop"},
		Artifact:  manifest.Artifact{Path: manifest.ArtifactName, Format: "pg_dump-custom", SizeBytes: n, SHA256: sum},
		Integrity: manifest.DefaultIntegrity(), ServerMajor: 16}
	if err := manifest.Write(filepath.Join(dir, manifest.FileName), m); err != nil {
		t.Fatal(err)
	}
	relA, _ := e.repo.Rel(art)
	relM, _ := e.repo.Rel(filepath.Join(dir, manifest.FileName))
	b := &metadata.Backup{ID: id, WorkloadID: workload, BackupType: "postgres-logical", Status: metadata.BackupCreated,
		VerificationStatus: metadata.VerifyVerified, CreatedAt: created, SourceJSON: "{}", ArtifactPath: relA,
		ManifestPath: relM, SizeBytes: n, SHA256: sum, ServerMajor: 16}
	if err := e.store.InsertBackup(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	return b
}

func category(err error) failure.Category { return failure.CategoryOf(err) }

func TestCopyEncryptedSignedRoundTrip(t *testing.T) {
	e := newEnv(t, true, true)
	ctx := context.Background()
	b := e.addBackup(t, "shop", time.Now().UTC(), secretContent)
	rec, err := e.c.Copy(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != metadata.CopyVerified || !rec.Encrypted || !rec.Signed || rec.CheckedAt == nil {
		t.Fatalf("record %+v", rec)
	}
	loc := path.Join("unth", "shop", b.ID)
	want := []string{"artifact.dump.age", DescriptorName, SignatureName, "manifest.json.age"}
	keys, _ := e.be.List(ctx, loc+"/")
	if len(keys) != len(want) {
		t.Fatalf("objects %v", keys)
	}
	for i, k := range keys {
		if path.Base(k) != want[i] {
			t.Fatalf("objects %v", keys)
		}
	}
	// Nothing readable leaves the server: no artifact content, no host/table names.
	for k, v := range e.be.objs {
		for _, s := range []string{"CUSTOMER-TABLE-ROW", "alice@example.com", "db.internal"} {
			if bytes.Contains(v, []byte(s)) {
				t.Fatalf("plaintext %q in %s", s, k)
			}
		}
	}
	if _, err := e.c.Check(ctx, b); err != nil {
		t.Fatalf("check: %v", err)
	}
	// Fetch into a brand-new installation (disaster recovery) with the same keys.
	e2 := newEnv(t, true, true)
	k2, err := LoadKeys(filepath.Join(e.dir, AgeKeyFile), nil, "", filepath.Join(e.dir, SignPubKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	c2 := &Copier{Backend: e.be, Keys: k2, Prefix: "unth", Store: e2.store, Repo: e2.repo, Actor: "test"}
	list, err := c2.List(ctx, "")
	if err != nil || len(list) != 1 || list[0].BackupID != b.ID || list[0].Error != "" || !list[0].Encrypted {
		t.Fatalf("list %v %+v", err, list)
	}
	fb, res, err := c2.Fetch(ctx, "shop", b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SignatureChecked || fb.SHA256 != b.SHA256 || fb.VerificationStatus != metadata.VerifyNone || fb.Status != metadata.BackupCreated {
		t.Fatalf("fetched %+v %+v", fb, res)
	}
	// The stored record (what `unth verify` reads) must be complete, not just the returned struct.
	stored, err := e2.store.GetBackup(ctx, b.ID)
	if err != nil || stored.ArtifactPath == "" || stored.ManifestPath == "" || stored.SHA256 != b.SHA256 ||
		stored.SizeBytes != b.SizeBytes || stored.SnapshotAt == nil || stored.Status != metadata.BackupCreated ||
		stored.VerificationStatus != metadata.VerifyNone || stored.ServerMajor != 16 {
		t.Fatalf("stored record %v %+v", err, stored)
	}
	if cps, _ := e2.store.ListCopies(ctx, b.ID); len(cps) != 1 || cps[0].Status != metadata.CopyVerified {
		t.Fatalf("copy record after fetch: %+v", cps)
	}
	p, _ := e2.repo.Resolve(fb.ArtifactPath)
	got, _ := os.ReadFile(p)
	fi, _ := os.Stat(p)
	di, _ := os.Stat(filepath.Dir(p))
	if string(got) != secretContent || fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("fetched file %q %v %v", got, fi.Mode(), di.Mode())
	}
	if _, _, err := c2.Fetch(ctx, "shop", b.ID); err == nil {
		t.Fatal("second fetch over an existing backup must be refused")
	}
	if m, err := manifest.Read(filepath.Join(filepath.Dir(p), manifest.FileName)); err != nil || m.BackupID != b.ID {
		t.Fatalf("manifest %v", err)
	}
}

func TestTamperedCopiesAreNeverAccepted(t *testing.T) {
	ctx := context.Background()
	type mutate func(e *env, loc string)
	flip := func(name string, at int) mutate {
		return func(e *env, loc string) {
			k := path.Join(loc, name)
			v := e.be.objs[k]
			if at < 0 {
				at = len(v) + at
			}
			v[at] ^= 0x01
		}
	}
	cases := map[string]struct {
		m   mutate
		cat failure.Category
	}{
		"artifact body byte": {flip("artifact.dump.age", -20), failure.DecryptionFailed},
		"artifact header":    {flip("artifact.dump.age", 40), failure.DecryptionFailed},
		"manifest body byte": {flip("manifest.json.age", -5), failure.DecryptionFailed},
		"copy.json edited":   {func(e *env, loc string) { e.be.objs[path.Join(loc, DescriptorName)][20] ^= 1 }, failure.SignatureInvalid},
		"signature removed":  {func(e *env, loc string) { delete(e.be.objs, path.Join(loc, SignatureName)) }, failure.SignatureInvalid},
		"artifact removed":   {func(e *env, loc string) { delete(e.be.objs, path.Join(loc, "artifact.dump.age")) }, failure.ArtifactMissing},
		"descriptor removed": {func(e *env, loc string) { delete(e.be.objs, path.Join(loc, DescriptorName)) }, failure.ArtifactMissing},
		"artifact truncated": {func(e *env, loc string) {
			k := path.Join(loc, "artifact.dump.age")
			e.be.objs[k] = e.be.objs[k][:len(e.be.objs[k])-10]
		}, failure.DecryptionFailed},
		"bytes appended": {func(e *env, loc string) {
			k := path.Join(loc, "artifact.dump.age")
			e.be.objs[k] = append(e.be.objs[k], 'x') // age itself rejects trailing data
		}, failure.DecryptionFailed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, true, true)
			b := e.addBackup(t, "shop", time.Now().UTC(), secretContent)
			if _, err := e.c.Copy(ctx, b); err != nil {
				t.Fatal(err)
			}
			loc := path.Join("unth", "shop", b.ID)
			tc.m(e, loc)
			_, err := e.c.Check(ctx, b)
			if err == nil || category(err) != tc.cat {
				t.Fatalf("got %v (%s), want %s", err, category(err), tc.cat)
			}
			rec, _ := e.store.GetCopy(ctx, b.ID, e.be.Target())
			if rec.Status != metadata.CopyFailed {
				t.Fatalf("status %s after failed check", rec.Status)
			}
			e2 := newEnv(t, true, true)
			c2 := &Copier{Backend: e.be, Keys: e.keys, Prefix: "unth", Store: e2.store, Repo: e2.repo}
			if _, _, err := c2.Fetch(ctx, "shop", b.ID); err == nil {
				t.Fatal("fetch of a tampered copy succeeded")
			}
			if bs, _ := e2.store.ListBackups(ctx, 10); len(bs) != 0 {
				t.Fatal("tampered copy imported")
			}
			if ents, _ := os.ReadDir(filepath.Join(e2.repo.Root, "tmp")); len(ents) != 0 {
				t.Fatalf("staging left behind: %v", ents)
			}
		})
	}
}

func TestWrongKeysAndSwappedCopies(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, true, true)
	b1 := e.addBackup(t, "shop", time.Now().UTC(), secretContent)
	b2 := e.addBackup(t, "shop", time.Now().UTC().Add(time.Second), "other content")
	for _, b := range []*metadata.Backup{b1, b2} {
		if _, err := e.c.Copy(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	// Different age identity.
	other := newEnv(t, true, true)
	c := &Copier{Backend: e.be, Keys: &Keys{Identity: other.keys.Identity, VerifyKey: e.keys.VerifyKey}, Prefix: "unth", Store: e.store, Repo: e.repo}
	if _, err := c.Check(ctx, b1); category(err) != failure.DecryptionFailed {
		t.Fatalf("wrong identity: %v", err)
	}
	// Different signing key.
	c = &Copier{Backend: e.be, Keys: &Keys{Identity: e.keys.Identity, VerifyKey: other.keys.VerifyKey}, Prefix: "unth", Store: e.store, Repo: e.repo}
	if _, err := c.Check(ctx, b1); category(err) != failure.SignatureInvalid {
		t.Fatalf("wrong signing key: %v", err)
	}
	// No identity for an encrypted copy.
	c = &Copier{Backend: e.be, Keys: &Keys{VerifyKey: e.keys.VerifyKey}, Prefix: "unth", Store: e.store, Repo: e.repo}
	if _, err := c.Check(ctx, b1); category(err) != failure.DecryptionFailed {
		t.Fatalf("missing identity: %v", err)
	}
	// Copy of b2 moved to b1's location (all objects, valid signature).
	l1, l2 := path.Join("unth", "shop", b1.ID), path.Join("unth", "shop", b2.ID)
	for _, n := range []string{"artifact.dump.age", "manifest.json.age", DescriptorName, SignatureName} {
		e.be.objs[path.Join(l1, n)] = e.be.objs[path.Join(l2, n)]
	}
	if _, err := e.c.Check(ctx, b1); category(err) != failure.OffsiteMismatch {
		t.Fatalf("swapped copy: %v", err)
	}
}

func TestUnencryptedCopyRejectedWhenEncryptionConfigured(t *testing.T) {
	ctx := context.Background()
	plain := newEnv(t, false, false)
	b := plain.addBackup(t, "shop", time.Now().UTC(), secretContent)
	rec, err := plain.c.Copy(ctx, b)
	if err != nil || rec.Encrypted {
		t.Fatalf("plain copy: %v %+v", err, rec)
	}
	if !bytes.Contains(plain.be.objs[path.Join("unth", "shop", b.ID, "artifact.dump")], []byte("CUSTOMER")) {
		t.Fatal("unencrypted copy should contain the plaintext artifact")
	}
	enc := newEnv(t, true, false)
	c := &Copier{Backend: plain.be, Keys: enc.keys, Prefix: "unth", Store: plain.store, Repo: plain.repo}
	if _, err := c.Check(ctx, b); category(err) != failure.OffsiteMismatch {
		t.Fatalf("got %v", err)
	}
}

func TestCorruptLocalArtifactIsNotCopied(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, true, true)
	b := e.addBackup(t, "shop", time.Now().UTC(), secretContent)
	p, _ := e.repo.Resolve(b.ArtifactPath)
	_ = os.WriteFile(p, []byte(strings.ToUpper(secretContent)), 0o600)
	rec, err := e.c.Copy(ctx, b)
	if category(err) != failure.ChecksumMismatch || rec.Status != metadata.CopyFailed {
		t.Fatalf("got %v %+v", err, rec)
	}
	if len(e.be.objs) != 0 {
		t.Fatalf("partial upload left behind: %d objects", len(e.be.objs))
	}
}

func TestInterruptedUploadIsCleanedUp(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, true, true)
	b := e.addBackup(t, "shop", time.Now().UTC(), secretContent)
	e.be.failPut = "manifest.json.age"
	rec, err := e.c.Copy(ctx, b)
	if err == nil || rec.Status != metadata.CopyFailed || rec.ErrorCategory != string(failure.OffsiteFailed) {
		t.Fatalf("got %v %+v", err, rec)
	}
	if len(e.be.objs) != 0 {
		t.Fatalf("partial upload left behind: %v", e.be.objs)
	}
	if list, _ := e.c.List(ctx, "shop"); len(list) != 0 {
		t.Fatal("incomplete copy listed")
	}
}

func TestOnlyVerifiedBackupsAreCopied(t *testing.T) {
	e := newEnv(t, true, false)
	b := e.addBackup(t, "shop", time.Now().UTC(), secretContent)
	b.VerificationStatus = metadata.VerifyFailed
	if _, err := e.c.Copy(context.Background(), b); err == nil {
		t.Fatal("unverified backup copied")
	}
	if len(e.be.objs) != 0 {
		t.Fatal("objects written")
	}
}

func TestOffsitePrune(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, true, true)
	base := time.Now().UTC().Add(-72 * time.Hour)
	var bs []*metadata.Backup
	for i := 0; i < 4; i++ {
		b := e.addBackup(t, "shop", base.Add(time.Duration(i)*time.Hour), fmt.Sprintf("content %d", i))
		if _, err := e.c.Copy(ctx, b); err != nil {
			t.Fatal(err)
		}
		bs = append(bs, b)
	}
	other := e.addBackup(t, "billing", base, "billing")
	if _, err := e.c.Copy(ctx, other); err != nil {
		t.Fatal(err)
	}
	dry, err := e.c.Prune(ctx, "shop", config.Retention{KeepLast: 2}, true)
	if err != nil || len(dry.Deleted) != 2 || len(e.be.objs) != 5*4 {
		t.Fatalf("dry run %v %+v (%d objects)", err, dry, len(e.be.objs))
	}
	res, err := e.c.Prune(ctx, "shop", config.Retention{KeepLast: 2}, false)
	if err != nil || len(res.Deleted) != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	for i, b := range bs {
		rec, _ := e.store.GetCopy(ctx, b.ID, e.be.Target())
		keys, _ := e.be.List(ctx, path.Join("unth", "shop", b.ID)+"/")
		deleted := i < 2
		if deleted != (rec.Status == metadata.CopyDeleted) || deleted != (len(keys) == 0) {
			t.Fatalf("backup %d: status %s, %d objects", i, rec.Status, len(keys))
		}
	}
	if keys, _ := e.be.List(ctx, "unth/billing/"); len(keys) != 4 {
		t.Fatal("prune touched another workload")
	}
	// Local retention never limits offsite retention: an EXPIRED local backup keeps its copy.
	bs[3].Status = metadata.BackupExpired
	_ = e.store.UpdateBackup(ctx, bs[3])
	if res, _ := e.c.Prune(ctx, "shop", config.Retention{KeepLast: 2}, false); len(res.Deleted) != 0 {
		t.Fatalf("deleted %v", res.Deleted)
	}
}

func TestGenerateKeysNeverOverwrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "k")
	g, err := GenerateKeys(dir)
	if err != nil || !strings.HasPrefix(g.AgeRecipient, "age1") || !strings.HasPrefix(g.SignPublic, "ed25519:") {
		t.Fatalf("%v %+v", err, g)
	}
	for _, n := range []string{AgeKeyFile, SignKeyFile} {
		fi, _ := os.Stat(filepath.Join(dir, n))
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", n, fi.Mode())
		}
	}
	before, _ := os.ReadFile(filepath.Join(dir, AgeKeyFile))
	if _, err := GenerateKeys(dir); err == nil {
		t.Fatal("overwrote existing keys")
	}
	after, _ := os.ReadFile(filepath.Join(dir, AgeKeyFile))
	if !bytes.Equal(before, after) {
		t.Fatal("key changed")
	}
	// A readable secret key is refused.
	_ = os.Chmod(filepath.Join(dir, AgeKeyFile), 0o644)
	if _, err := LoadKeys(filepath.Join(dir, AgeKeyFile), nil, "", ""); err == nil {
		t.Fatal("world-readable identity accepted")
	}
	_ = os.Chmod(filepath.Join(dir, AgeKeyFile), 0o600)
	if _, err := LoadKeys(filepath.Join(dir, AgeKeyFile), []string{"age1notakey"}, "", ""); err == nil {
		t.Fatal("invalid recipient accepted")
	}
	k, err := LoadKeys(filepath.Join(dir, AgeKeyFile), []string{g.AgeRecipient}, filepath.Join(dir, SignKeyFile), "")
	if err != nil || len(k.Recipients) != 2 || k.SignKey == nil || Fingerprint(k.VerifyKey) != g.SignPublic {
		t.Fatalf("%v %+v", err, k)
	}
}

// An offline recovery key (extra recipient) can decrypt a copy without the server's identity.
func TestRecoveryRecipientCanFetch(t *testing.T) {
	ctx := context.Background()
	recovery := newEnv(t, true, false) // stands for a key kept offline
	e := newEnv(t, true, false)
	k, err := LoadKeys(filepath.Join(e.dir, AgeKeyFile), []string{recovery.keys.Identity.Recipient().String()}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	e.c.Keys = k
	b := e.addBackup(t, "shop", time.Now().UTC(), secretContent)
	if _, err := e.c.Copy(ctx, b); err != nil {
		t.Fatal(err)
	}
	c := &Copier{Backend: e.be, Keys: recovery.keys, Prefix: "unth", Store: recovery.store, Repo: recovery.repo}
	if fb, _, err := c.Fetch(ctx, "shop", b.ID); err != nil || fb.SHA256 != b.SHA256 {
		t.Fatalf("recovery fetch: %v", err)
	}
}
