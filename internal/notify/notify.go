// Package notify delivers the result of `unth run` to HTTP endpoints.
//
// Webhook URLs are treated as secrets (Slack URLs embed a token) and are read
// from environment variables; they are registered with the redactor and never
// logged. Generic webhooks can be signed with HMAC-SHA256:
//
//	X-Unth-Timestamp: <unix seconds>
//	X-Unth-Signature: sha256=<hex(HMAC(secret, timestamp + "." + body))>
//
// Receivers should verify the signature and reject old timestamps.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/unmadethings/unthrecover/internal/buildinfo"
	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/security"
)

// Event is the payload sent to generic webhooks.
type Event struct {
	Event              string    `json:"event"`
	SchemaVersion      int       `json:"schema_version"`
	ToolVersion        string    `json:"tool_version"`
	Host               string    `json:"host"`
	Workload           string    `json:"workload"`
	Outcome            string    `json:"outcome"` // success | failure
	BackupID           string    `json:"backup_id,omitempty"`
	BackupStatus       string    `json:"backup_status,omitempty"`
	RunID              string    `json:"verification_run_id,omitempty"`
	VerificationStatus string    `json:"verification_status,omitempty"`
	ErrorCategory      string    `json:"error_category,omitempty"`
	ErrorMessage       string    `json:"error_message,omitempty"`
	Hint               string    `json:"hint,omitempty"`
	Pruned             []string  `json:"pruned_backup_ids,omitempty"`
	StartedAt          time.Time `json:"started_at"`
	FinishedAt         time.Time `json:"finished_at"`
	DurationMS         int64     `json:"duration_ms"`
}

// Failed reports whether the event represents a failure.
func (e *Event) Failed() bool { return e.Outcome != "success" }

// Result is the delivery outcome for one notifier.
type Result struct {
	Name    string `json:"name"`
	Sent    bool   `json:"sent"`
	Skipped string `json:"skipped,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Sender delivers events.
type Sender struct {
	Redactor *security.Redactor
	// Client may be overridden in tests.
	Client *http.Client
	// Now may be overridden in tests.
	Now func() time.Time
}

// SendAll delivers ev to every configured notifier whose trigger matches.
// Errors are returned per notifier; they never change the run outcome.
func (s *Sender) SendAll(ctx context.Context, ns []config.Notifier, ev *Event) []Result {
	out := make([]Result, 0, len(ns))
	for _, n := range ns {
		r := Result{Name: n.Name}
		if n.On == config.OnFailure && !ev.Failed() {
			r.Skipped = "on=failure and the run succeeded"
			out = append(out, r)
			continue
		}
		if err := s.send(ctx, n, ev); err != nil {
			r.Error = s.Redactor.RedactError(err)
		} else {
			r.Sent = true
		}
		out = append(out, r)
	}
	return out
}

func (s *Sender) send(ctx context.Context, n config.Notifier, ev *Event) error {
	raw, ok := os.LookupEnv(n.URLEnv)
	if !ok || raw == "" {
		return fmt.Errorf("environment variable %s (url_env) is not set", n.URLEnv)
	}
	s.Redactor.Add(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("invalid webhook URL")
	}
	if err := checkURL(u); err != nil {
		return err
	}
	var secret string
	if n.SecretEnv != "" {
		secret = os.Getenv(n.SecretEnv)
		if secret == "" {
			return fmt.Errorf("environment variable %s (secret_env) is not set", n.SecretEnv)
		}
		s.Redactor.Add(secret)
	}

	var body []byte
	switch n.Type {
	case config.NotifySlack:
		body, err = json.Marshal(map[string]string{"text": SlackText(ev)})
	default:
		body, err = json.Marshal(ev)
	}
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, n.Timeout.D())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot build request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "unth-recover/"+buildinfo.Version)
	req.Header.Set("X-Unth-Event", ev.Event)
	if secret != "" {
		now := time.Now
		if s.Now != nil {
			now = s.Now
		}
		ts := strconv.FormatInt(now().Unix(), 10)
		req.Header.Set("X-Unth-Timestamp", ts)
		req.Header.Set("X-Unth-Signature", "sha256="+Sign(secret, ts, body))
	}
	client := s.Client
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		// url.Error embeds the full URL (a secret); report only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("delivery to %s failed: %v", u.Hostname(), ue.Err)
		}
		return fmt.Errorf("delivery to %s failed", u.Hostname())
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("delivery to %s failed: HTTP %d", u.Hostname(), resp.StatusCode)
	}
	return nil
}

// checkURL requires https, except for loopback hosts (local relays, tests).
func checkURL(u *url.URL) error {
	if u.User != nil {
		return errors.New("webhook URL must not contain credentials in userinfo; use secret_env")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return errors.New("webhook URL must use https (http is allowed only for loopback hosts)")
	default:
		return errors.New("webhook URL must use https")
	}
}

// Sign returns hex(HMAC-SHA256(secret, timestamp + "." + body)).
func Sign(secret, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// SlackText renders a short human-readable message.
func SlackText(ev *Event) string {
	var b strings.Builder
	icon := ":white_check_mark:"
	if ev.Failed() {
		icon = ":rotating_light:"
	}
	fmt.Fprintf(&b, "%s *Unth Recover* — `%s` on %s: *%s*", icon, ev.Workload, ev.Host, strings.ToUpper(ev.Outcome))
	if ev.BackupID != "" {
		fmt.Fprintf(&b, "\nBackup `%s` (%s)", ev.BackupID, ev.BackupStatus)
	}
	if ev.VerificationStatus != "" {
		fmt.Fprintf(&b, ", verification: %s", ev.VerificationStatus)
	}
	if ev.ErrorCategory != "" {
		fmt.Fprintf(&b, "\n%s: %s", ev.ErrorCategory, ev.ErrorMessage)
	}
	if ev.Hint != "" {
		fmt.Fprintf(&b, "\nSuggested fix: %s", ev.Hint)
	}
	if len(ev.Pruned) > 0 {
		fmt.Fprintf(&b, "\nPruned %d expired backup(s)", len(ev.Pruned))
	}
	return b.String()
}
