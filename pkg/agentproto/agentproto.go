// Package agentproto defines the protocol between the open-source unth agent
// and a control plane (Unth Recover Cloud or a self-hosted one). It is a
// public package so both sides share the exact message types and the request
// signature.
//
// Principles:
//   - The agent only makes outbound HTTPS requests (works behind NAT/firewalls).
//   - No shared secret after enrollment: every request is signed with the
//     agent's Ed25519 key; the server stores only the public key.
//   - Backup data never goes to the control plane: only summaries (status,
//     sizes, durations, error categories). Secrets the user enters in the
//     control plane are sent age-encrypted to the agent's own key, so the
//     control plane cannot read them back.
package agentproto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Version is the protocol version sent by agents.
const Version = 1

// API paths (relative to the control plane base URL).
const (
	PathEnroll    = "/api/agent/v1/enroll"
	PathHeartbeat = "/api/agent/v1/heartbeat"
	PathJobs      = "/api/agent/v1/jobs"
	PathEvents    = "/api/agent/v1/events"
	// PathDriveToken: a short-lived Google Drive access token for a Drive
	// connected in the control plane (offsite type gdrive in managed jobs).
	PathDriveToken = "/api/agent/v1/drive-token" // #nosec G101 -- a URL path, not a credential
)

// Signature headers.
const (
	HeaderAgent     = "X-Unth-Agent"
	HeaderTimestamp = "X-Unth-Timestamp"
	HeaderNonce     = "X-Unth-Nonce"
	HeaderSignature = "X-Unth-Signature"
	// MaxSkew is the accepted clock difference.
	MaxSkew = 5 * time.Minute
	// MaxBody bounds request and response bodies.
	MaxBody = 4 << 20
)

// EnrollRequest is sent once with the one-time enrollment token from the
// control plane ("Add server").
type EnrollRequest struct {
	Token        string `json:"token"`
	PublicKey    string `json:"public_key"`    // Ed25519, base64
	AgeRecipient string `json:"age_recipient"` // age1..., for secrets sent to this agent
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Version      string `json:"version"`
	Protocol     int    `json:"protocol"`
}

// EnrollResponse identifies the agent from then on.
type EnrollResponse struct {
	AgentID string `json:"agent_id"`
	OrgName string `json:"org_name"`
}

// Heartbeat is sent periodically.
type Heartbeat struct {
	Version     string         `json:"version"`
	Protocol    int            `json:"protocol"`
	Hostname    string         `json:"hostname"`
	JobsVersion string         `json:"jobs_version"` // version of the job set the agent has
	Running     []string       `json:"running"`      // job ids currently running
	Docker      string         `json:"docker"`       // engine version or "unreachable"
	FreeBytes   int64          `json:"free_bytes"`
	Stats       map[string]int `json:"stats,omitempty"`
}

// HeartbeatResponse carries commands for the agent.
type HeartbeatResponse struct {
	Commands    []Command `json:"commands"`
	JobsVersion string    `json:"jobs_version"` // differs from the agent's: fetch jobs
	IntervalSec int       `json:"interval_sec"` // next heartbeat
}

// Command types.
const (
	CmdRunJob = "run_job"   // run one job now (backup, verify, offsite, prune)
	CmdVerify = "verify"    // verify an existing backup now
	CmdSync   = "sync_jobs" // fetch the job set now
)

// Command is an instruction from the control plane.
type Command struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	JobID    string `json:"job_id,omitempty"`
	BackupID string `json:"backup_id,omitempty"`
}

// Job is a job managed in the control plane. Config is the job YAML; it must
// reference secrets only through password_env / *_env names listed in Secrets.
type Job struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Schedule string            `json:"schedule"` // "daily 03:00" | "every 6h" | "" (manual only)
	Config   string            `json:"config"`
	Secrets  map[string]string `json:"secrets"` // env name -> age-encrypted (armored) value
	Disabled bool              `json:"disabled"`
	// TimeZone (IANA, e.g. Europe/Istanbul) names dated offsite copy folders
	// when the job file sets no offsite.time_zone. Older agents ignore it.
	TimeZone string `json:"time_zone,omitempty"`
}

// JobsResponse is the agent's complete job set.
type JobsResponse struct {
	Version string `json:"version"`
	Jobs    []Job  `json:"jobs"`
}

// Event types.
const (
	EvRunCompleted    = "run.completed"
	EvVerifyCompleted = "verify.completed"
	EvCommandRejected = "command.rejected"
	EvJobRejected     = "job.rejected"
)

// Event reports a result. Summary holds only metadata (see package doc).
type Event struct {
	ID        string         `json:"id"` // unique per event (idempotency)
	Type      string         `json:"type"`
	CommandID string         `json:"command_id,omitempty"`
	JobID     string         `json:"job_id,omitempty"`
	At        time.Time      `json:"at"`
	Summary   map[string]any `json:"summary"`
}

// Events is a batch of events.
type Events struct {
	Events []Event `json:"events"`
}

// ParseSchedule validates a schedule string. It returns (daily, hour, minute,
// every); manual-only is "".
func ParseSchedule(s string) (daily bool, h, m int, every time.Duration, err error) {
	switch {
	case s == "":
		return false, 0, 0, 0, nil
	case strings.HasPrefix(s, "daily "):
		t, perr := time.Parse("15:04", strings.TrimPrefix(s, "daily "))
		if perr != nil {
			return false, 0, 0, 0, fmt.Errorf("schedule %q: want daily HH:MM", s)
		}
		return true, t.Hour(), t.Minute(), 0, nil
	case strings.HasPrefix(s, "every "):
		d, perr := time.ParseDuration(strings.TrimPrefix(s, "every "))
		if perr != nil || d < 15*time.Minute || d > 7*24*time.Hour {
			return false, 0, 0, 0, fmt.Errorf("schedule %q: want every 15m..168h", s)
		}
		return false, 0, 0, d, nil
	}
	return false, 0, 0, 0, fmt.Errorf("schedule %q: want \"daily HH:MM\" or \"every DURATION\"", s)
}

// canonical is the signed string: method, path with query, timestamp, nonce,
// hex SHA-256 of the body.
func canonical(method, pathQuery, ts, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{"UNTH-AGENT-V1", method, pathQuery, ts, nonce, hex.EncodeToString(sum[:])}, "\n"))
}

// Sign adds the signature headers to req. body must be the exact body sent.
func Sign(req *http.Request, agentID string, key ed25519.PrivateKey, nonce string, now time.Time, body []byte) {
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := ed25519.Sign(key, canonical(req.Method, req.URL.RequestURI(), ts, nonce, body))
	req.Header.Set(HeaderAgent, agentID)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(sig))
}

// ErrBadSignature is returned for any authentication failure.
var ErrBadSignature = errors.New("invalid agent signature")

// Verify checks a signed request on the server side. lookup returns the
// agent's public key; seen reports (and records) whether the nonce was used
// before within MaxSkew. It returns the agent id and the body (read fully).
func Verify(r *http.Request, now time.Time, lookup func(agentID string) (ed25519.PublicKey, error),
	seen func(agentID, nonce string) bool) (string, []byte, error) {
	id, ts, nonce := r.Header.Get(HeaderAgent), r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderNonce)
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get(HeaderSignature))
	if id == "" || ts == "" || len(nonce) < 16 || len(nonce) > 128 || err != nil {
		return "", nil, ErrBadSignature
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", nil, ErrBadSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > MaxSkew || d < -MaxSkew {
		return "", nil, fmt.Errorf("%w: clock skew %s (check the server time)", ErrBadSignature, d.Round(time.Second))
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		return "", nil, ErrBadSignature
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	pub, err := lookup(id)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", nil, ErrBadSignature
	}
	if !ed25519.Verify(pub, canonical(r.Method, r.URL.RequestURI(), ts, nonce, body), sig) {
		return "", nil, ErrBadSignature
	}
	if seen(id, nonce) {
		return "", nil, fmt.Errorf("%w: replayed request", ErrBadSignature)
	}
	return id, body, nil
}

// DriveTokenRequest asks for an access token of a Drive connection of the
// agent's organization.
type DriveTokenRequest struct {
	Connection string `json:"connection"`
}

// DriveTokenResponse is a short-lived access token (scope drive.file) and the
// folder copies go to. The refresh token never leaves the control plane.
type DriveTokenResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	Account     string    `json:"account"`
	FolderID    string    `json:"folder_id"`
	FolderName  string    `json:"folder_name"`
}
