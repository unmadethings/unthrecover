// Package fakegoogle is an in-process stand-in for Google's OAuth endpoints
// and the subset of the Drive v3 API unth uses. It is used by tests only; it
// enforces what the real services enforce for this flow (client id/secret,
// redirect URI, single-use codes, PKCE S256, bearer tokens, revocation,
// resumable-upload Content-Range rules, drive.file visibility).
package fakegoogle

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// File is a stored Drive file or folder.
type File struct {
	ID, Name, MimeType, Parent string
	Data                       []byte
	ByApp                      bool // created through the API (drive.file visibility)
	Granted                    bool // picked by the user in Google's picker (visible to the app)
	Trashed                    bool
	NoDelete                   bool // shared drive member: delete is refused, trash works
}

func (f *File) visible() bool { return f.ByApp || f.Granted }

type code struct {
	redirect, challenge string
	used                bool
}

// Server is the fake.
type Server struct {
	*httptest.Server
	ClientID, ClientSecret, Email string
	Deny                          bool          // the user clicks "Cancel" on the consent screen
	OmitDriveScope                bool          // the user unticks the Drive permission
	AccessTTL                     time.Duration // default 1h

	mu       sync.Mutex
	codes    map[string]*code
	access   map[string]time.Time // token -> expiry
	refresh  map[string]bool      // token -> valid
	files    map[string]*File
	uploads  map[string]*upload
	seq      int
	Requests int
}

type upload struct {
	name, parent string
	data         []byte
}

// New starts the fake server.
func New() *Server {
	s := &Server{ClientID: "fake-client.apps.googleusercontent.com", ClientSecret: "fake-secret", Email: "user@example.com",
		AccessTTL: time.Hour, codes: map[string]*code{}, access: map[string]time.Time{}, refresh: map[string]bool{},
		files: map[string]*File{}, uploads: map[string]*upload{}}
	s.files["root"] = &File{ID: "root", Name: "My Drive", MimeType: "application/vnd.google-apps.folder"}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", s.auth)
	mux.HandleFunc("/token", s.token)
	mux.HandleFunc("/userinfo", s.userinfo)
	mux.HandleFunc("/revoke", s.revoke)
	mux.HandleFunc("/drive/v3/files", s.authed(s.filesCollection))
	mux.HandleFunc("/drive/v3/files/", s.authed(s.fileItem))
	mux.HandleFunc("/upload/drive/v3/files", s.authed(s.startUpload))
	mux.HandleFunc("/upload/session/", s.authed(s.uploadChunk))
	s.Server = httptest.NewServer(mux)
	return s
}

func (s *Server) id() string {
	s.seq++
	return fmt.Sprintf("f%06d", s.seq)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

// auth simulates the browser consent page: it validates the request and
// immediately redirects back, as if the user clicked "Allow".
func (s *Server) auth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != s.ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("access_type") != "offline" {
		http.Error(w, "invalid_request", http.StatusBadRequest)
		return
	}
	ru, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || ru.Scheme != "http" || (ru.Hostname() != "127.0.0.1" && ru.Hostname() != "localhost" && ru.Hostname() != "::1") {
		http.Error(w, "redirect_uri_mismatch (desktop clients allow loopback only)", http.StatusBadRequest)
		return
	}
	v := url.Values{"state": {q.Get("state")}}
	if s.Deny {
		v.Set("error", "access_denied")
	} else {
		c := RandomCode()
		s.mu.Lock()
		s.codes[c] = &code{redirect: q.Get("redirect_uri"), challenge: q.Get("code_challenge")}
		s.mu.Unlock()
		v.Set("code", c)
		v.Set("scope", "openid email https://www.googleapis.com/auth/drive.file")
	}
	ru.RawQuery = v.Encode()
	http.Redirect(w, r, ru.String(), http.StatusFound) // #nosec G710 -- test fake; loopback redirect URIs only (checked above)
}

// RandomCode returns a random-looking code.
func RandomCode() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "x" + strconv.Itoa(int(time.Now().UnixNano()%9973))
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	tokErr := func(e string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": e})
	}
	if f.Get("client_id") != s.ClientID || f.Get("client_secret") != s.ClientSecret {
		tokErr("invalid_client")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	scope := "openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/drive.file"
	if s.OmitDriveScope {
		scope = "openid https://www.googleapis.com/auth/userinfo.email"
	}
	resp := map[string]any{"token_type": "Bearer", "expires_in": int(s.AccessTTL.Seconds()), "scope": scope}
	switch f.Get("grant_type") {
	case "authorization_code":
		c := s.codes[f.Get("code")]
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if c == nil || c.used || c.redirect != f.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			tokErr("invalid_grant")
			return
		}
		c.used = true
		rt := "rt-" + RandomCode()
		s.refresh[rt] = true
		resp["refresh_token"] = rt
	case "refresh_token":
		if !s.refresh[f.Get("refresh_token")] {
			tokErr("invalid_grant")
			return
		}
	default:
		tokErr("unsupported_grant_type")
		return
	}
	at := "at-" + RandomCode()
	s.access[at] = time.Now().Add(s.AccessTTL)
	resp["access_token"] = at
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) bearer(r *http.Request) bool {
	t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.access[t]
	return ok && time.Now().Before(exp)
}

func (s *Server) userinfo(w http.ResponseWriter, r *http.Request) {
	if !s.bearer(r) {
		jsonErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"email": s.Email, "email_verified": true})
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	t := r.PostForm.Get("token")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refresh[t] {
		s.refresh[t] = false
		return
	}
	if _, ok := s.access[t]; ok {
		delete(s.access, t)
		return
	}
	w.WriteHeader(http.StatusBadRequest)
}

func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.Requests++
		s.mu.Unlock()
		if !s.bearer(r) {
			jsonErr(w, http.StatusUnauthorized, "Invalid Credentials")
			return
		}
		h(w, r)
	}
}

var (
	qParent = regexp.MustCompile(`^'([^']*)' in parents and trashed = false$`)
	qName   = regexp.MustCompile(`^'([^']*)' in parents and name = '((?:[^'\\]|\\.)*)' and trashed = false$`)
)

func (s *Server) filesCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query().Get("q")
		var parent, name string
		if m := qName.FindStringSubmatch(q); m != nil {
			parent, name = m[1], strings.NewReplacer(`\'`, `'`, `\\`, `\`).Replace(m[2])
		} else if m := qParent.FindStringSubmatch(q); m != nil {
			parent = m[1]
		} else {
			jsonErr(w, http.StatusBadRequest, "unsupported query in fake: "+q)
			return
		}
		s.mu.Lock()
		var out []map[string]string
		for _, f := range s.files {
			if f.Parent == parent && f.visible() && !f.Trashed && (name == "" || f.Name == name) {
				out = append(out, map[string]string{"id": f.ID, "name": f.Name, "mimeType": f.MimeType})
			}
		}
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"files": out})
	case http.MethodPost:
		var meta struct {
			Name     string   `json:"name"`
			MimeType string   `json:"mimeType"`
			Parents  []string `json:"parents"`
		}
		if json.NewDecoder(r.Body).Decode(&meta) != nil || meta.Name == "" || len(meta.Parents) != 1 {
			jsonErr(w, http.StatusBadRequest, "bad metadata")
			return
		}
		s.mu.Lock()
		if _, ok := s.files[meta.Parents[0]]; !ok {
			s.mu.Unlock()
			jsonErr(w, http.StatusNotFound, "parent not found")
			return
		}
		f := &File{ID: s.id(), Name: meta.Name, MimeType: meta.MimeType, Parent: meta.Parents[0], ByApp: true}
		s.files[f.ID] = f
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": f.ID})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) fileItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
	s.mu.Lock()
	f, ok := s.files[id]
	if !ok || !f.visible() {
		s.mu.Unlock()
		jsonErr(w, http.StatusNotFound, "File not found: "+id)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Trashed bool `json:"trashed"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.Trashed = body.Trashed
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": f.ID})
	case http.MethodGet:
		data := append([]byte(nil), f.Data...)
		meta := map[string]any{"id": f.ID, "name": f.Name, "mimeType": f.MimeType, "trashed": f.Trashed}
		s.mu.Unlock()
		if r.URL.Query().Get("alt") == "media" {
			_, _ = w.Write(data)
			return
		}
		_ = json.NewEncoder(w).Encode(meta)
	case http.MethodDelete:
		if f.NoDelete {
			s.mu.Unlock()
			jsonErr(w, http.StatusForbidden, "The user does not have sufficient permissions for this file.")
			return
		}
		var del func(id string)
		del = func(id string) {
			for _, c := range s.files {
				if c.Parent == id {
					del(c.ID)
				}
			}
			delete(s.files, id)
		}
		del(id)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s.mu.Unlock()
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) startUpload(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("uploadType") != "resumable" {
		jsonErr(w, http.StatusBadRequest, "only resumable uploads in fake")
		return
	}
	var meta struct {
		Name    string   `json:"name"`
		Parents []string `json:"parents"`
	}
	if json.NewDecoder(r.Body).Decode(&meta) != nil || meta.Name == "" || len(meta.Parents) != 1 {
		jsonErr(w, http.StatusBadRequest, "bad metadata")
		return
	}
	s.mu.Lock()
	sid := s.id()
	s.uploads[sid] = &upload{name: meta.Name, parent: meta.Parents[0]}
	s.mu.Unlock()
	w.Header().Set("Location", s.URL+"/upload/session/"+sid)
	w.WriteHeader(http.StatusOK)
}

var crPattern = regexp.MustCompile(`^bytes (?:(\d+)-(\d+)|\*)/(\d+|\*)$`)

func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request) {
	sid := strings.TrimPrefix(r.URL.Path, "/upload/session/")
	body, _ := io.ReadAll(r.Body)
	m := crPattern.FindStringSubmatch(r.Header.Get("Content-Range"))
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[sid]
	if u == nil || m == nil {
		jsonErr(w, http.StatusBadRequest, "bad upload session or Content-Range")
		return
	}
	if m[1] != "" {
		start, _ := strconv.Atoi(m[1])
		end, _ := strconv.Atoi(m[2])
		if start != len(u.data) || end-start+1 != len(body) {
			jsonErr(w, http.StatusBadRequest, "Content-Range does not match the data")
			return
		}
		if m[3] == "*" && len(body)%(256<<10) != 0 {
			jsonErr(w, http.StatusBadRequest, "non-final chunks must be multiples of 256 KiB")
			return
		}
		u.data = append(u.data, body...)
	}
	if m[3] == "*" {
		w.WriteHeader(308)
		return
	}
	total, _ := strconv.Atoi(m[3])
	if total != len(u.data) {
		jsonErr(w, http.StatusBadRequest, "total size mismatch")
		return
	}
	f := &File{ID: s.id(), Name: u.name, Parent: u.parent, Data: u.data, ByApp: true, MimeType: "application/octet-stream"}
	s.files[f.ID] = f
	delete(s.uploads, sid)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": f.ID})
}

// AddUserFile adds a file the user created outside unth (invisible with drive.file).
func (s *Server) AddUserFile(name string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.id()
	s.files[id] = &File{ID: id, Name: name, Parent: "root", Data: data, MimeType: "text/plain"}
}

// AddUserFolder adds a folder the user created outside unth; the app cannot
// see it until Grant (the user picks it in Google's picker).
func (s *Server) AddUserFolder(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.id()
	s.files[id] = &File{ID: id, Name: name, Parent: "root", MimeType: "application/vnd.google-apps.folder"}
	return id
}

// Grant shares one file or folder with the app, as Google's picker does.
func (s *Server) Grant(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.files[id]; ok {
		f.Granted = true
	}
}

// FindByName returns the files with a name (for tampering in tests).
func (s *Server) FindByName(name string) []*File {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*File
	for _, f := range s.files {
		if f.Name == name {
			out = append(out, f)
		}
	}
	return out
}

// Mutate changes a stored file under the lock.
func (s *Server) Mutate(f *File, fn func(*File)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(f)
}

// Count returns the number of stored files and folders created by the app.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, f := range s.files {
		if f.ByApp {
			n++
		}
	}
	return n
}

// RevokeAll invalidates every refresh and access token (user removed access).
func (s *Server) RevokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.refresh {
		s.refresh[k] = false
	}
	s.access = map[string]time.Time{}
}

// RefreshValid reports whether a refresh token is still valid.
func (s *Server) RefreshValid(t string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refresh[t]
}

// ExpireAccess invalidates all access tokens (refresh tokens stay valid), as
// when an hour has passed.
func (s *Server) ExpireAccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.access = map[string]time.Time{}
}

// Connect performs the browser part of the flow without a browser: it calls
// the auth endpoint and returns the redirect target (callback URL with code).
func (s *Server) Connect(authURL string) (string, error) {
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Get(authURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("auth: HTTP %d %s", resp.StatusCode, b)
	}
	return resp.Header.Get("Location"), nil
}

// ParentName returns the name of a file's parent folder.
func (s *Server) ParentName(f *File) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.files[f.Parent]; ok {
		return p.Name
	}
	return ""
}
