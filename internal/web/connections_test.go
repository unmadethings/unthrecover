package web

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unmadethings/unthrecover/internal/connections"
	"github.com/unmadethings/unthrecover/internal/fakegoogle"
)

func fakeClient(fg *fakegoogle.Server) *connections.Google {
	b := fg.URL
	return &connections.Google{ClientID: fg.ClientID, ClientSecret: fg.ClientSecret, AuthURL: b + "/auth", TokenURL: b + "/token",
		UserinfoURL: b + "/userinfo", RevokeURL: b + "/revoke", DriveURL: b + "/drive/v3", UploadURL: b + "/upload/drive/v3",
		HTTP: fg.Client()}
}

var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func TestConnectGoogleDriveFlow(t *testing.T) {
	fg := fakegoogle.New()
	defer fg.Close()
	srv, ts, _ := newTestServer(t, false)
	srv.Google = fakeClient(fg)
	sid := login(t, ts)

	start := func() string {
		r := do(t, ts, "POST", "/api/connections/gdrive", "{}", sid, true)
		var v map[string]string
		_ = json.NewDecoder(r.Body).Decode(&v)
		if r.StatusCode != 200 || !strings.HasPrefix(v["auth_url"], fg.URL+"/auth?") {
			t.Fatalf("start: %d %v", r.StatusCode, v)
		}
		if !strings.Contains(v["auth_url"], "drive.file") || !strings.Contains(v["auth_url"], "code_challenge_method=S256") {
			t.Fatalf("auth url %s", v["auth_url"])
		}
		return v["auth_url"]
	}
	callback := func(loc string) string {
		r, err := noFollow.Get(loc) // the browser returns WITHOUT the session cookie (SameSite=Strict)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusFound {
			t.Fatalf("callback status %d", r.StatusCode)
		}
		return r.Header.Get("Location")
	}

	loc, err := fg.Connect(start())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loc, ts.URL+"/oauth/google/callback?") {
		t.Fatalf("redirect %s", loc)
	}
	if got := callback(loc); got != "/#/settings?gdrive=connected%3Agdrive" {
		t.Fatalf("result %s", got)
	}
	// Replaying the same callback (same state) is rejected.
	if got := callback(loc); !strings.Contains(got, "gdrive=expired") {
		t.Fatalf("replay: %s", got)
	}
	if got := callback(ts.URL + "/oauth/google/callback?state=forged&code=x"); !strings.Contains(got, "gdrive=expired") {
		t.Fatalf("forged state: %s", got)
	}
	// The connection is listed without secrets; the file is private.
	r := do(t, ts, "GET", "/api/connections", "", sid, false)
	body, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(body), `"account":"user@example.com"`) || !strings.Contains(string(body), `"google_configured":true`) {
		t.Fatalf("list %s", body)
	}
	c, err := connections.NewStore(srv.Repo.Root).Load("gdrive")
	if err != nil || c.RefreshToken == "" || c.FolderID == "" {
		t.Fatalf("stored %v %+v", err, c)
	}
	if strings.Contains(string(body), c.RefreshToken) {
		t.Fatal("refresh token returned by the API")
	}
	fi, _ := os.Stat(filepath.Join(srv.Repo.Root, "connections", "gdrive.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("connection file mode %v", fi.Mode())
	}
	// A second account gets its own name.
	loc, _ = fg.Connect(start())
	if got := callback(loc); got != "/#/settings?gdrive=connected%3Agdrive-2" {
		t.Fatalf("second: %s", got)
	}
	// The user clicks Cancel / unticks the Drive permission.
	fg.Deny = true
	loc, _ = fg.Connect(start())
	if got := callback(loc); !strings.Contains(got, "gdrive=denied") {
		t.Fatalf("deny: %s", got)
	}
	fg.Deny, fg.OmitDriveScope = false, true
	loc, _ = fg.Connect(start())
	if got := callback(loc); !strings.Contains(got, "gdrive=failed") {
		t.Fatalf("no drive scope: %s", got)
	}
	// Disconnect revokes the token at Google.
	if r := do(t, ts, "DELETE", "/api/connections/gdrive", "", sid, true); r.StatusCode != 200 {
		t.Fatalf("delete %d", r.StatusCode)
	}
	if fg.RefreshValid(c.RefreshToken) {
		t.Fatal("token not revoked")
	}
	if _, err := connections.NewStore(srv.Repo.Root).Load("gdrive"); err == nil {
		t.Fatal("connection not removed")
	}
	if r := do(t, ts, "DELETE", "/api/connections/nope", "", sid, true); r.StatusCode != 404 {
		t.Fatal("unknown connection")
	}
}

func TestConnectGoogleDriveGuards(t *testing.T) {
	fg := fakegoogle.New()
	defer fg.Close()
	srv, ts, _ := newTestServer(t, false)
	srv.Google = fakeClient(fg)
	sid := login(t, ts)
	if r := do(t, ts, "POST", "/api/connections/gdrive", "{}", "", true); r.StatusCode != 401 {
		t.Fatal("start without session")
	}
	if r := do(t, ts, "POST", "/api/connections/gdrive", "{}", sid, false); r.StatusCode != 403 {
		t.Fatal("start without CSRF header")
	}
	if r := do(t, ts, "GET", "/api/connections", "", "", false); r.StatusCode != 401 {
		t.Fatal("list without session")
	}
	// Panel reached through a domain: the Desktop client cannot return there.
	req, _ := http.NewRequest("POST", ts.URL+"/api/connections/gdrive", strings.NewReader("{}"))
	req.Host = "backup.example.com"
	req.Header.Set("Cookie", cookieName+"="+sid)
	req.Header.Set(csrfHeader, "1")
	r, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 400 || !strings.Contains(string(b), "SSH tunnel") {
		t.Fatalf("non-loopback: %d %s", r.StatusCode, b)
	}
	// No OAuth client in this build.
	srv.Google = &connections.Google{}
	r = do(t, ts, "POST", "/api/connections/gdrive", "{}", sid, true)
	if r.StatusCode != 409 {
		t.Fatalf("unconfigured: %d", r.StatusCode)
	}
}
