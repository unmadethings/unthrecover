// Package connections stores the accounts users connect from the panel (today:
// Google Drive) and implements the Google OAuth flow for them.
//
// Each user connects their OWN account; the project's OAuth client only
// identifies the app on Google's consent screen. Tokens stay on the user's
// server (data dir, 0600) and never pass through any Unmade Things service.
package connections

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/unmadethings/unthrecover/internal/buildinfo"
)

// Scopes requested from Google. drive.file: unth sees only the files it
// created itself — never the rest of the user's Drive.
const Scopes = "https://www.googleapis.com/auth/drive.file openid email"

// Google holds the OAuth client and endpoints.
type Google struct {
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	UserinfoURL  string
	RevokeURL    string
	DriveURL     string // https://www.googleapis.com/drive/v3
	UploadURL    string // https://www.googleapis.com/upload/drive/v3
	HTTP         *http.Client
}

// DefaultGoogle returns the configured client: UNTH_GOOGLE_CLIENT_ID/_SECRET
// override the client embedded in release builds. UNTH_GOOGLE_TEST_ENDPOINT
// (loopback URLs only) points all endpoints at a fake server for tests.
func DefaultGoogle() *Google {
	g := &Google{ // #nosec G101 -- public endpoint URLs; the client of an installed app is not confidential
		ClientID: buildinfo.GoogleClientID, ClientSecret: buildinfo.GoogleClientSecret,
		AuthURL:     "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:    "https://oauth2.googleapis.com/token",
		UserinfoURL: "https://openidconnect.googleapis.com/v1/userinfo",
		RevokeURL:   "https://oauth2.googleapis.com/revoke",
		DriveURL:    "https://www.googleapis.com/drive/v3",
		UploadURL:   "https://www.googleapis.com/upload/drive/v3",
		HTTP:        &http.Client{Timeout: 5 * time.Minute},
	}
	if id := os.Getenv("UNTH_GOOGLE_CLIENT_ID"); id != "" {
		g.ClientID, g.ClientSecret = id, os.Getenv("UNTH_GOOGLE_CLIENT_SECRET")
	}
	if base := os.Getenv("UNTH_GOOGLE_TEST_ENDPOINT"); base != "" && isLoopbackURL(base) {
		base = strings.TrimSuffix(base, "/")
		g.AuthURL, g.TokenURL, g.UserinfoURL, g.RevokeURL = base+"/auth", base+"/token", base+"/userinfo", base+"/revoke"
		g.DriveURL, g.UploadURL = base+"/drive/v3", base+"/upload/drive/v3"
	}
	return g
}

// Configured reports whether an OAuth client is available.
func (g *Google) Configured() bool { return g.ClientID != "" && g.ClientSecret != "" }

func isLoopbackURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "http" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
}

// RandomToken returns a URL-safe random string with 256 bits of entropy.
func RandomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Challenge returns the PKCE S256 challenge of a verifier.
func Challenge(verifier string) string {
	s := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

// AuthCodeURL is where the browser is sent to sign in and allow access.
func (g *Google) AuthCodeURL(state, verifier, redirectURI string) string {
	q := url.Values{
		"client_id": {g.ClientID}, "redirect_uri": {redirectURI}, "response_type": {"code"},
		"scope": {Scopes}, "state": {state}, "code_challenge": {Challenge(verifier)},
		"code_challenge_method": {"S256"}, "access_type": {"offline"}, "prompt": {"consent"},
		"include_granted_scopes": {"false"},
	}
	return g.AuthURL + "?" + q.Encode()
}

// Token is the result of a code exchange.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"-"`
	Scope        string    `json:"scope"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func (g *Google) postForm(ctx context.Context, u string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&tr); err != nil {
		return nil, fmt.Errorf("google token endpoint: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || tr.Error != "" {
		return nil, fmt.Errorf("google token endpoint: %s %s", tr.Error, tr.ErrorDesc)
	}
	return &tr, nil
}

// Exchange trades the authorization code (with the PKCE verifier) for tokens.
func (g *Google) Exchange(ctx context.Context, code, verifier, redirectURI string) (*Token, error) {
	tr, err := g.postForm(ctx, g.TokenURL, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
		"redirect_uri": {redirectURI}, "client_id": {g.ClientID}, "client_secret": {g.ClientSecret},
	})
	if err != nil {
		return nil, err
	}
	if tr.RefreshToken == "" {
		return nil, errors.New("google did not return a refresh token")
	}
	if !strings.Contains(" "+tr.Scope+" ", " https://www.googleapis.com/auth/drive.file ") {
		return nil, errors.New("access to Google Drive was not granted (the drive.file permission was unticked)")
	}
	return &Token{AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken, Scope: tr.Scope,
		Expiry: time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)}, nil
}

// Refresh returns a new access token for a refresh token.
func (g *Google) Refresh(ctx context.Context, refreshToken string) (string, time.Time, error) {
	tr, err := g.postForm(ctx, g.TokenURL, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken},
		"client_id": {g.ClientID}, "client_secret": {g.ClientSecret},
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return tr.AccessToken, time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second), nil
}

// Email returns the account's e-mail address (shown in the panel).
func (g *Google) Email(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.UserinfoURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		Email string `json:"email"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v) != nil || v.Email == "" {
		return "", fmt.Errorf("google userinfo: HTTP %d", resp.StatusCode)
	}
	return v.Email, nil
}

// Revoke invalidates a token at Google (used when disconnecting).
func (g *Google) Revoke(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.RevokeURL, strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest { // 400: already invalid
		return fmt.Errorf("google revoke: HTTP %d", resp.StatusCode)
	}
	return nil
}
