package offsite

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/connections"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/pkg/fakegoogle"
)

func fakeGoogleClient(fg *fakegoogle.Server) *connections.Google {
	b := fg.URL
	return &connections.Google{ClientID: fg.ClientID, ClientSecret: fg.ClientSecret, AuthURL: b + "/auth", TokenURL: b + "/token",
		UserinfoURL: b + "/userinfo", RevokeURL: b + "/revoke", DriveURL: b + "/drive/v3", UploadURL: b + "/upload/drive/v3",
		HTTP: fg.Client()}
}

// connectFake runs the OAuth flow against the fake and stores a connection.
func connectFake(t *testing.T, fg *fakegoogle.Server, g *connections.Google, store *connections.Store) string {
	t.Helper()
	ru := "http://127.0.0.1:8484/oauth/google/callback"
	state, verifier := connections.RandomToken(), connections.RandomToken()
	loc, err := fg.Connect(g.AuthCodeURL(state, verifier, ru))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(loc)
	if u.Query().Get("state") != state {
		t.Fatal("state not returned")
	}
	tok, err := g.Exchange(context.Background(), u.Query().Get("code"), verifier, ru)
	if err != nil {
		t.Fatal(err)
	}
	email, _ := g.Email(context.Background(), tok.AccessToken)
	c := &connections.Connection{Name: "gdrive", Provider: "gdrive", Account: email, ClientID: g.ClientID, RefreshToken: tok.RefreshToken, CreatedAt: time.Now()}
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	return tok.RefreshToken
}

func TestGDriveBackend(t *testing.T) {
	ctx := context.Background()
	fg := fakegoogle.New()
	defer fg.Close()
	fg.AddUserFile("tax-return-2025.pdf", []byte("private")) // must stay invisible to unth
	g := fakeGoogleClient(fg)
	e := newEnv(t, true, true)
	store := connections.NewStore(e.repo.Root)
	connectFake(t, fg, g, store)
	d, err := NewGDrive(g, store, "gdrive")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if c, _ := store.Load("gdrive"); c.FolderID == "" {
		t.Fatal("root folder id not saved")
	}
	if len(fg.FindByName(RootFolderName)) != 1 {
		t.Fatal("root folder not created")
	}
	e.c.Backend = d
	// A >8 MiB artifact exercises multi-chunk resumable uploads.
	big := make([]byte, 19<<20)
	_, _ = rand.Read(big)
	var bs []*metadata.Backup
	for i := 0; i < 3; i++ {
		content := secretContent
		if i == 0 {
			content = string(big)
		}
		b := e.addBackup(t, "shop", time.Now().UTC().Add(time.Duration(i)*time.Minute), content)
		rec, err := e.c.Copy(ctx, b)
		if err != nil || rec.Status != metadata.CopyVerified || rec.Target != "gdrive:user@example.com" {
			t.Fatalf("copy %d: %v %+v", i, err, rec)
		}
		bs = append(bs, b)
	}
	// Only unth's own files are visible (drive.file).
	keys, err := d.List(ctx, "")
	if err != nil || len(keys) != 3*4 {
		t.Fatalf("list %v %d %v", err, len(keys), keys)
	}
	for _, k := range keys {
		if strings.Contains(k, "tax-return") {
			t.Fatal("user file visible")
		}
	}
	// Access token expiry -> transparent refresh.
	fg.ExpireAccess()
	if _, err := e.c.Check(ctx, bs[1]); err != nil {
		t.Fatalf("check after access expiry: %v", err)
	}
	// Tamper with a stored object.
	arts := fg.FindByName("artifact.dump.age")
	if len(arts) != 3 {
		t.Fatalf("%d artifacts", len(arts))
	}
	for _, f := range arts {
		if strings.Contains(string(f.Data), "CUSTOMER") {
			t.Fatal("plaintext on Drive")
		}
	}
	var target *fakegoogle.File // the copy of bs[0] (FindByName order is random)
	for _, f := range arts {
		if fg.ParentName(f) == path.Base(e.loc(t, bs[0])) {
			target = f
		}
	}
	if target == nil {
		t.Fatal("artifact of the first backup not found")
	}
	fg.Mutate(target, func(f *fakegoogle.File) { f.Data[len(f.Data)/2] ^= 1 })
	failed := 0
	for _, b := range bs {
		if _, err := e.c.Check(ctx, b); err != nil {
			if c := failure.CategoryOf(err); c != failure.DecryptionFailed {
				t.Fatalf("tamper category %s", c)
			}
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("expected exactly one tampered copy, got %d", failed)
	}
	if _, err := e.c.Check(ctx, bs[0]); failure.CategoryOf(err) != failure.DecryptionFailed {
		t.Fatalf("tampered copy of bs[0] not rejected: %v", err)
	}
	// Disaster recovery on a new machine with the same Drive connection.
	e2 := newEnv(t, true, true)
	store2 := connections.NewStore(e2.repo.Root)
	connectFake(t, fg, g, store2)
	d2, _ := NewGDrive(g, store2, "gdrive")
	c2 := &Copier{Backend: d2, Keys: e.keys, Prefix: "unth", Store: e2.store, Repo: e2.repo}
	if list, err := c2.List(ctx, "shop"); err != nil || len(list) != 3 {
		t.Fatalf("list on new machine: %v %d", err, len(list))
	}
	if n := len(fg.FindByName(RootFolderName)); n != 1 {
		t.Fatalf("a new machine must reuse the existing backup folder, found %d folders", n)
	}
	if fb, _, err := c2.Fetch(ctx, "shop", bs[2].ID); err != nil || fb.SHA256 != bs[2].SHA256 {
		t.Fatalf("fetch: %v", err)
	}
	// Retention deletes copies on Drive.
	before := fg.Count()
	res, err := e.c.Prune(ctx, "shop", config.Retention{KeepLast: 1}, false)
	if err != nil || len(res.Deleted) == 0 || fg.Count() >= before {
		t.Fatalf("prune %v %+v %d->%d", err, res, before, fg.Count())
	}
	// No empty dated folders are left behind on Drive.
	for _, id := range res.Deleted {
		for _, b := range bs {
			if b.ID == id {
				if n := len(fg.FindByName(path.Base(e.loc(t, b)))); n != 0 {
					t.Fatalf("empty folder of pruned copy %s left on Drive", e.loc(t, b))
				}
			}
		}
	}
	// The user removes access in the Google account -> clear error.
	fg.RevokeAll()
	d3, _ := NewGDrive(g, store, "gdrive")
	if err := d3.Check(ctx); err == nil || !strings.Contains(err.Error(), "connect again") {
		t.Fatalf("revoked access: %v", err)
	}
}

func TestGDriveConnectionChecks(t *testing.T) {
	fg := fakegoogle.New()
	defer fg.Close()
	g := fakeGoogleClient(fg)
	store := connections.NewStore(t.TempDir())
	if _, err := NewGDrive(g, store, "gdrive"); err == nil {
		t.Fatal("missing connection accepted")
	}
	connectFake(t, fg, g, store)
	other := *g
	other.ClientID = "another-client"
	if _, err := NewGDrive(&other, store, "gdrive"); err == nil {
		t.Fatal("token of another OAuth client accepted")
	}
	unconf := *g
	unconf.ClientID, unconf.ClientSecret = "", ""
	if _, err := NewGDrive(&unconf, store, "gdrive"); err == nil {
		t.Fatal("unconfigured client accepted")
	}
	// PKCE: a code cannot be redeemed with a different verifier, nor twice.
	ru := "http://127.0.0.1:1/cb"
	state, verifier := connections.RandomToken(), connections.RandomToken()
	loc, _ := fg.Connect(g.AuthCodeURL(state, verifier, ru))
	u, _ := url.Parse(loc)
	code := u.Query().Get("code")
	if _, err := g.Exchange(context.Background(), code, connections.RandomToken(), ru); err == nil {
		t.Fatal("wrong PKCE verifier accepted")
	}
	loc, _ = fg.Connect(g.AuthCodeURL(state, verifier, ru))
	u, _ = url.Parse(loc)
	if _, err := g.Exchange(context.Background(), u.Query().Get("code"), verifier, ru); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Exchange(context.Background(), u.Query().Get("code"), verifier, ru); err == nil {
		t.Fatal("code redeemed twice")
	}
	// The user unticks the Drive permission on the consent screen.
	fg.OmitDriveScope = true
	loc, _ = fg.Connect(g.AuthCodeURL(state, verifier, ru))
	u, _ = url.Parse(loc)
	if _, err := g.Exchange(context.Background(), u.Query().Get("code"), verifier, ru); err == nil || !strings.Contains(err.Error(), "drive.file") {
		t.Fatalf("missing scope: %v", err)
	}
}

func TestGDriveSharedDriveDeleteFallsBackToTrash(t *testing.T) {
	fg := fakegoogle.New()
	defer fg.Close()
	g := fakeGoogleClient(fg)
	store := connections.NewStore(t.TempDir())
	connectFake(t, fg, g, store)
	d, err := NewGDrive(g, store, "gdrive")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := d.Put(ctx, "shop/2026-11/2026-11-11 17.30/copy.json", strings.NewReader("{}"), 2); err != nil {
		t.Fatal(err)
	}
	f := fg.FindByName("copy.json")[0]
	fg.Mutate(f, func(f *fakegoogle.File) { f.NoDelete = true }) // shared drive member
	if err := d.Delete(ctx, "shop/2026-11/2026-11-11 17.30/copy.json"); err != nil {
		t.Fatal(err)
	}
	if !f.Trashed {
		t.Fatal("not moved to the trash")
	}
	if keys, _ := d.List(ctx, "shop/"); len(keys) != 0 {
		t.Fatalf("trashed file still listed: %v", keys)
	}
}

// A Drive connected in a control plane: the agent only ever holds
// short-lived access tokens; the folder is fixed by the control plane.
func TestBrokeredGDrive(t *testing.T) {
	fg := fakegoogle.New()
	defer fg.Close()
	g := fakeGoogleClient(fg)
	cloudStore := connections.NewStore(t.TempDir()) // stands in for the control plane
	rt := connectFake(t, fg, g, cloudStore)
	folder := fg.AddUserFolder("Acme backups")
	fg.Grant(folder)
	calls := 0
	broker := func(ctx context.Context, name string) (*BrokeredToken, error) {
		calls++
		if name != "drive-acme" {
			return nil, errors.New("unknown connection")
		}
		at, exp, err := g.Refresh(ctx, rt)
		return &BrokeredToken{AccessToken: at, Expiry: exp, Account: "user@example.com", FolderID: folder, FolderName: "Acme backups"}, err
	}
	e := newEnv(t, true, true)
	ctx := WithDriveBroker(context.Background(), broker)
	cfg := &config.Config{Offsite: config.Offsite{Type: config.OffsiteGDrive, Connection: "drive-acme", Prefix: "web1"},
		Encryption: config.Encryption{IdentityFile: filepath.Join(e.dir, AgeKeyFile)}, Signing: config.Signing{KeyFile: filepath.Join(e.dir, SignKeyFile)}}
	t.Setenv("UNTH_GOOGLE_TEST_ENDPOINT", fg.URL)
	cp, err := Open(ctx, cfg, e.store, e.repo, "agent")
	if err != nil {
		t.Fatal(err)
	}
	cp.Backend.(*GDrive).g.HTTP = fg.Client()
	b := e.addBackup(t, "shop", time.Now().UTC(), secretContent)
	if rec, err := cp.Copy(ctx, b); err != nil || rec.Target != "gdrive:user@example.com" {
		t.Fatalf("copy: %v %+v", err, rec)
	}
	if f := fg.FindByName("web1"); len(f) != 1 || fg.ParentName(f[0]) != "Acme backups" {
		t.Fatal("copy not in the control plane's folder")
	}
	if n := len(fg.FindByName(RootFolderName)); n != 0 {
		t.Fatal("a brokered connection must never create its own folder")
	}
	// Expired access -> a new token from the broker, transparently.
	fg.ExpireAccess()
	if _, err := cp.Check(ctx, b); err != nil {
		t.Fatalf("check after expiry: %v", err)
	}
	if calls < 2 {
		t.Fatalf("broker calls %d", calls)
	}
	// The folder is removed: fail loudly, no fallback.
	fg.Mutate(fg.FindByName("Acme backups")[0], func(f *fakegoogle.File) { f.Trashed = true })
	if _, err := Open(ctx, cfg, e.store, e.repo, "agent"); err == nil || !strings.Contains(err.Error(), "choose a folder again") {
		t.Fatalf("trashed folder: %v", err)
	}
	// Unknown connection at the control plane.
	cfg.Offsite.Connection = "drive-other"
	if _, err := Open(ctx, cfg, e.store, e.repo, "agent"); failure.CategoryOf(err) != failure.OffsiteFailed {
		t.Fatalf("unknown connection: %v", err)
	}
}
