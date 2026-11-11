package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/unmadethings/unthrecover/internal/connections"
	"github.com/unmadethings/unthrecover/internal/offsite"
)

const (
	oauthCallbackPath = "/oauth/google/callback"
	oauthTTL          = 10 * time.Minute
)

type pendingOAuth struct {
	verifier, redirect string
	created            time.Time
}

func (s *Server) conns() *connections.Store { return connections.NewStore(s.Repo.Root) }

func (s *Server) google() *connections.Google {
	if s.Google != nil {
		return s.Google
	}
	return connections.DefaultGoogle()
}

// redirectURI is where Google sends the browser back. With the project's
// "Desktop app" OAuth client Google accepts loopback addresses on any port,
// so the panel must be opened at 127.0.0.1/localhost (directly or through an
// SSH tunnel). UNTH_GOOGLE_REDIRECT_URL overrides it for an own Web client.
func (s *Server) redirectURI(r *http.Request) (string, error) {
	if v := os.Getenv("UNTH_GOOGLE_REDIRECT_URL"); v != "" {
		return v, nil
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", errors.New("to connect Google Drive, open the panel at http://127.0.0.1:<port> on the server (for a remote server use an SSH tunnel: ssh -L 8484:127.0.0.1:8484 server), then click Connect again")
	}
	if s.TLS {
		return "", errors.New("google's sign-in returns to http://127.0.0.1; run the panel without TLS on loopback to connect Google Drive")
	}
	return "http://" + r.Host + oauthCallbackPath, nil
}

func (s *Server) listConnections(w http.ResponseWriter, _ *http.Request) {
	list, err := s.conns().List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot read connections")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"google_configured": s.google().Configured(),
		"picker_configured": s.google().PickerConfigured(), "connections": list})
}

// openDrive loads a Google Drive connection for the folder endpoints.
func (s *Server) openDrive(w http.ResponseWriter, r *http.Request) (*offsite.GDrive, bool) {
	d, err := offsite.NewGDrive(s.google(), s.conns(), r.PathValue("name"))
	if errors.Is(err, connections.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "connection not found")
		return nil, false
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, s.Redactor.Redact(err.Error()))
		return nil, false
	}
	return d, true
}

// pickerToken gives the panel what Google's folder picker needs: a
// short-lived access token (scope drive.file: only files and folders shared
// with the app), the public API key and the project number. The refresh
// token never leaves the server.
func (s *Server) pickerToken(w http.ResponseWriter, r *http.Request) {
	g := s.google()
	if !g.PickerConfigured() {
		writeErr(w, http.StatusConflict, "this build has no Google API key for the folder picker; set UNTH_GOOGLE_API_KEY (docs/OFFSITE.md)")
		return
	}
	d, ok := s.openDrive(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	tok, err := d.AccessToken(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, s.Redactor.Redact(err.Error()))
		return
	}
	s.Redactor.Add(tok)
	s.auditEvent(r.Context(), "connection.gdrive.picker", r.PathValue("name"), "opened", "")
	writeJSON(w, http.StatusOK, map[string]string{"access_token": tok, "api_key": g.APIKey, "app_id": g.AppID()})
}

func (s *Server) setFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	d, ok := s.openDrive(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	name, err := d.UseFolder(ctx, body.ID)
	if err != nil {
		s.auditEvent(r.Context(), "connection.gdrive.folder", r.PathValue("name"), "failed", s.Redactor.Redact(err.Error()))
		writeErr(w, http.StatusBadRequest, s.Redactor.Redact(err.Error()))
		return
	}
	s.auditEvent(r.Context(), "connection.gdrive.folder", r.PathValue("name"), "picked", name)
	writeJSON(w, http.StatusOK, map[string]string{"folder": name})
}

func (s *Server) resetFolder(w http.ResponseWriter, r *http.Request) {
	d, ok := s.openDrive(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := d.UseDefaultFolder(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, s.Redactor.Redact(err.Error()))
		return
	}
	s.auditEvent(r.Context(), "connection.gdrive.folder", r.PathValue("name"), "default", connections.DefaultFolderName)
	writeJSON(w, http.StatusOK, map[string]string{"folder": connections.DefaultFolderName})
}

func (s *Server) startGDrive(w http.ResponseWriter, r *http.Request) {
	g := s.google()
	if !g.Configured() {
		writeErr(w, http.StatusConflict, "this build has no Google OAuth client; set UNTH_GOOGLE_CLIENT_ID and UNTH_GOOGLE_CLIENT_SECRET (docs/OFFSITE.md)")
		return
	}
	ru, err := s.redirectURI(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	state, verifier := connections.RandomToken(), connections.RandomToken()
	s.mu.Lock()
	now := s.now()
	for k, p := range s.oauth { // drop expired attempts
		if now.Sub(p.created) > oauthTTL {
			delete(s.oauth, k)
		}
	}
	if len(s.oauth) >= 20 {
		s.mu.Unlock()
		writeErr(w, http.StatusTooManyRequests, "too many pending Google sign-ins; try again in a few minutes")
		return
	}
	s.oauth[state] = &pendingOAuth{verifier: verifier, redirect: ru, created: now}
	s.mu.Unlock()
	s.auditEvent(r.Context(), "connection.gdrive.start", "gdrive", "started", "")
	writeJSON(w, http.StatusOK, map[string]string{"auth_url": g.AuthCodeURL(state, verifier, ru)})
}

// googleCallback is reached by a top-level redirect from Google, which does
// not carry the SameSite=Strict session cookie. It is protected by the
// random, single-use, 10-minute state created for an authenticated user, and
// by PKCE: the code is useless without the verifier that never left the server.
func (s *Server) googleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	back := func(result string) {
		http.Redirect(w, r, "/#/settings?gdrive="+url.QueryEscape(result), http.StatusFound)
	}
	s.mu.Lock()
	p := s.oauth[q.Get("state")]
	delete(s.oauth, q.Get("state"))
	s.mu.Unlock()
	if q.Get("state") == "" || p == nil || s.now().Sub(p.created) > oauthTTL {
		s.auditEvent(r.Context(), "connection.gdrive.finish", "gdrive", "rejected", "unknown or expired state")
		back("expired")
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		s.auditEvent(r.Context(), "connection.gdrive.finish", "gdrive", "denied", "")
		back("denied")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	g := s.google()
	tok, err := g.Exchange(ctx, q.Get("code"), p.verifier, p.redirect)
	if err != nil {
		s.Log.Warn("google drive connection failed", "error", s.Redactor.RedactError(err))
		s.auditEvent(r.Context(), "connection.gdrive.finish", "gdrive", "failed", s.Redactor.Redact(err.Error()))
		back("failed")
		return
	}
	s.Redactor.Add(tok.RefreshToken)
	s.Redactor.Add(tok.AccessToken)
	email, err := g.Email(ctx, tok.AccessToken)
	if err != nil {
		email = "unknown account"
	}
	store := s.conns()
	c := &connections.Connection{Name: store.NextName("gdrive"), Provider: "gdrive", Account: email,
		ClientID: g.ClientID, RefreshToken: tok.RefreshToken, CreatedAt: s.now().UTC()}
	if err := store.Save(c); err != nil {
		back("failed")
		return
	}
	// Create the "Unth Recover Backups" folder now, so the user sees it works.
	if d, err := offsite.NewGDrive(g, store, c.Name); err == nil {
		if err := d.Check(ctx); err != nil {
			s.Log.Warn("google drive folder", "error", s.Redactor.RedactError(err))
		}
	}
	s.auditEvent(r.Context(), "connection.gdrive.finish", c.Name, "connected", email)
	back("connected:" + c.Name)
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	store := s.conns()
	c, err := store.Load(name)
	if errors.Is(err, connections.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "connection not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid connection")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	revoked := s.google().Revoke(ctx, c.RefreshToken) == nil
	if err := store.Delete(name); err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot remove connection")
		return
	}
	s.auditEvent(r.Context(), "connection.delete", name, "deleted", map[bool]string{true: "access revoked at Google", false: "revoke failed; remove access in the Google account"}[revoked])
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "revoked": revoked})
}
