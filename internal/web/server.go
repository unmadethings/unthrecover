// Package web implements `unth serve`: an authenticated web management panel
// (embedded single-page app + JSON API) on top of the local metadata store.
//
// Security model:
//   - authentication is always required (admin token from a 0600 file or env);
//   - listening on a non-loopback address requires TLS;
//   - sessions are random 256-bit IDs in HttpOnly, SameSite=Strict cookies
//     (Secure when TLS is on), expiring after 12h;
//   - state-changing requests additionally require the X-Unth-CSRF header,
//     which browsers cannot send cross-origin without CORS (none is offered);
//   - failed logins are rate-limited per client address;
//   - strict security headers (CSP 'self', no framing, no sniffing, no referrer).
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/unmadethings/unthrecover/internal/buildinfo"
	"github.com/unmadethings/unthrecover/internal/docker"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/repo"
	"github.com/unmadethings/unthrecover/internal/report"
	"github.com/unmadethings/unthrecover/internal/security"
	"github.com/unmadethings/unthrecover/internal/verify"
)

//go:embed all:dist
var distFS embed.FS

const (
	cookieName  = "unth_session"
	sessionTTL  = 12 * time.Hour
	csrfHeader  = "X-Unth-CSRF"
	minTokenLen = 24
	maxFailures = 5
	failWindow  = time.Minute
)

// Server is the web panel.
type Server struct {
	Store    *metadata.Store
	Repo     *repo.Repo
	Docker   func() (*docker.Client, error)
	Redactor *security.Redactor
	Log      *slog.Logger
	Token    string
	TLS      bool
	Now      func() time.Time

	mu        sync.Mutex
	sessions  map[string]time.Time
	failures  map[string][]time.Time
	verifying map[string]bool
	bgCtx     context.Context
}

// ValidateToken checks the admin token strength.
func ValidateToken(t string) error {
	if len(t) < minTokenLen {
		return fmt.Errorf("UI token must be at least %d characters (e.g. `openssl rand -hex 32`)", minTokenLen)
	}
	return nil
}

// CheckListen refuses plain HTTP on non-loopback addresses.
func CheckListen(addr string, tls bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid --listen %q: %w", addr, err)
	}
	if tls {
		return nil
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("refusing to serve plain HTTP on %q: use a loopback address (e.g. 127.0.0.1:8484) or provide --tls-cert/--tls-key", addr)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handler returns the HTTP handler. bg is the lifetime context for background
// verifications started from the UI.
func (s *Server) Handler(bg context.Context) http.Handler {
	s.sessions = map[string]time.Time{}
	s.failures = map[string][]time.Time{}
	s.verifying = map[string]bool{}
	s.bgCtx = bg

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.auth(s.logout))
	mux.HandleFunc("GET /api/session", s.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "version": buildinfo.Version})
	}))
	mux.HandleFunc("GET /api/overview", s.auth(s.overview))
	mux.HandleFunc("GET /api/backups", s.auth(s.listBackups))
	mux.HandleFunc("GET /api/backups/{id}", s.auth(s.getBackup))
	mux.HandleFunc("POST /api/backups/{id}/verify", s.auth(s.startVerify))
	mux.HandleFunc("GET /api/runs/{id}/report", s.auth(s.getReport))
	mux.HandleFunc("GET /api/audit", s.auth(s.audit))
	mux.HandleFunc("GET /api/info", s.auth(s.info))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not found")
	})
	mux.Handle("/", s.static())
	return s.headers(mux)
}

func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if s.TLS {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) static() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			r.URL.Path = "/" // SPA fallback
		}
		files.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- auth

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	s.mu.Lock()
	now := s.now()
	recent := s.failures[ip][:0]
	for _, t := range s.failures[ip] {
		if now.Sub(t) < failWindow {
			recent = append(recent, t)
		}
	}
	s.failures[ip] = recent
	limited := len(recent) >= maxFailures
	s.mu.Unlock()
	if limited {
		w.Header().Set("Retry-After", strconv.Itoa(int(failWindow.Seconds())))
		writeErr(w, http.StatusTooManyRequests, "too many failed logins; try again later")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(s.Token)) != 1 {
		s.mu.Lock()
		s.failures[ip] = append(s.failures[ip], now)
		s.mu.Unlock()
		s.auditEvent(r.Context(), "ui.login", ip, "failed", "")
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	id := security.RandomHex(32)
	s.mu.Lock()
	s.sessions[id] = now.Add(sessionTTL)
	delete(s.failures, ip)
	s.mu.Unlock()
	// #nosec G124 -- Secure is set whenever TLS is on; plain HTTP is only allowed on loopback (CheckListen)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true, Secure: s.TLS,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
	s.auditEvent(r.Context(), "ui.login", ip, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	// #nosec G124 -- see login: Secure under TLS, loopback-only otherwise
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: s.TLS,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		s.mu.Lock()
		exp, ok := s.sessions[c.Value]
		if ok && s.now().After(exp) {
			delete(s.sessions, c.Value)
			ok = false
		}
		s.mu.Unlock()
		if !ok {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) != "1" {
			writeErr(w, http.StatusForbidden, "missing "+csrfHeader+" header")
			return
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------- API

type backupDTO struct {
	ID                 string     `json:"id"`
	Workload           string     `json:"workload"`
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	VerificationStatus string     `json:"verification_status"`
	CreatedAt          time.Time  `json:"created_at"`
	SnapshotAt         *time.Time `json:"snapshot_at,omitempty"`
	SizeBytes          int64      `json:"size_bytes"`
	SHA256             string     `json:"sha256,omitempty"`
	ServerVersion      string     `json:"server_version,omitempty"`
	ErrorCategory      string     `json:"error_category,omitempty"`
	ErrorMessage       string     `json:"error_message,omitempty"`
	LastVerificationID string     `json:"last_verification_id,omitempty"`
	Verifying          bool       `json:"verifying"`
}

func (s *Server) dto(b *metadata.Backup) backupDTO {
	s.mu.Lock()
	v := s.verifying[b.ID]
	s.mu.Unlock()
	return backupDTO{ID: b.ID, Workload: b.WorkloadID, Type: b.BackupType, Status: b.Status,
		VerificationStatus: b.VerificationStatus, CreatedAt: b.CreatedAt, SnapshotAt: b.SnapshotAt,
		SizeBytes: b.SizeBytes, SHA256: b.SHA256, ServerVersion: b.ServerVersion, ErrorCategory: b.ErrorCategory,
		ErrorMessage: s.Redactor.Redact(b.ErrorMessage), LastVerificationID: b.LastVerificationID, Verifying: v}
}

type workloadSummary struct {
	Workload           string     `json:"workload"`
	Type               string     `json:"type"`
	Backups            int        `json:"backups"`
	LastBackupAt       *time.Time `json:"last_backup_at,omitempty"`
	LastBackupStatus   string     `json:"last_backup_status,omitempty"`
	LastVerifiedAt     *time.Time `json:"last_verified_backup_at,omitempty"`
	LatestVerification string     `json:"latest_verification_status,omitempty"`
	StorageBytes       int64      `json:"storage_bytes"`
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bs, err := s.Store.ListBackups(ctx, 100000)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot read metadata")
		return
	}
	runs, err := s.Store.ListRunsSince(ctx, s.now().Add(-30*24*time.Hour))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot read metadata")
		return
	}
	byW := map[string]*workloadSummary{}
	var order []string
	var storage int64
	var lastBackup, lastVerified *time.Time
	statusCount := map[string]int{}
	for _, b := range bs { // newest first
		ws, ok := byW[b.WorkloadID]
		if !ok {
			ws = &workloadSummary{Workload: b.WorkloadID, Type: b.BackupType}
			byW[b.WorkloadID] = ws
			order = append(order, b.WorkloadID)
		}
		ws.Backups++
		statusCount[b.Status]++
		if b.Status == metadata.BackupCreated {
			ws.StorageBytes += b.SizeBytes
			storage += b.SizeBytes
			if ws.LastBackupAt == nil {
				t := b.CreatedAt
				ws.LastBackupAt = &t
				ws.LatestVerification = b.VerificationStatus
			}
			if lastBackup == nil || b.CreatedAt.After(*lastBackup) {
				t := b.CreatedAt
				lastBackup = &t
			}
		}
		if ws.LastBackupStatus == "" {
			ws.LastBackupStatus = b.Status
		}
		if b.VerificationStatus == metadata.VerifyVerified && b.Status == metadata.BackupCreated && ws.LastVerifiedAt == nil {
			t := b.CreatedAt
			ws.LastVerifiedAt = &t
			if lastVerified == nil || t.After(*lastVerified) {
				lastVerified = &t
			}
		}
	}
	var verified, failed, total int
	var restoreSum, restoreN int64
	for _, run := range runs {
		switch run.Status {
		case metadata.VerifyVerified:
			verified++
			total++
			if run.RestoreDurationMS > 0 {
				restoreSum += run.RestoreDurationMS
				restoreN++
			}
		case metadata.VerifyFailed:
			failed++
			total++
		}
	}
	workloads := make([]workloadSummary, 0, len(order))
	for _, k := range order {
		workloads = append(workloads, *byW[k])
	}
	var rate *float64
	if total > 0 {
		v := float64(verified) / float64(total)
		rate = &v
	}
	var avgRestore int64
	if restoreN > 0 {
		avgRestore = restoreSum / restoreN
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"protected_workloads":           len(workloads),
		"backups_by_status":             statusCount,
		"last_successful_backup_at":     lastBackup,
		"last_verified_backup_at":       lastVerified,
		"verification_success_rate_30d": rate,
		"verified_runs_30d":             verified,
		"failed_runs_30d":               failed,
		"storage_bytes":                 storage,
		"avg_restore_duration_ms_30d":   avgRestore,
		"workloads":                     workloads,
	})
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var bs []*metadata.Backup
	var err error
	if wl := r.URL.Query().Get("workload"); wl != "" {
		bs, err = s.Store.ListBackupsByWorkload(r.Context(), wl)
		if len(bs) > limit {
			bs = bs[:limit]
		}
	} else {
		bs, err = s.Store.ListBackups(r.Context(), limit)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot read metadata")
		return
	}
	out := make([]backupDTO, 0, len(bs))
	for _, b := range bs {
		out = append(out, s.dto(b))
	}
	writeJSON(w, http.StatusOK, out)
}

type runDTO struct {
	ID                string     `json:"id"`
	Status            string     `json:"status"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	RestoreDurationMS int64      `json:"restore_duration_ms"`
	TotalDurationMS   int64      `json:"total_duration_ms"`
	ErrorCategory     string     `json:"error_category,omitempty"`
	ErrorMessage      string     `json:"error_message,omitempty"`
	HasReport         bool       `json:"has_report"`
}

func (s *Server) getBackup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if security.ValidateID(id) != nil {
		writeErr(w, http.StatusBadRequest, "invalid backup id")
		return
	}
	b, err := s.Store.GetBackup(r.Context(), id)
	if errors.Is(err, metadata.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "backup not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot read metadata")
		return
	}
	runs, err := s.Store.ListRuns(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot read metadata")
		return
	}
	out := make([]runDTO, 0, len(runs))
	for _, run := range runs {
		out = append(out, runDTO{ID: run.ID, Status: run.Status, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
			RestoreDurationMS: run.RestoreDurationMS, TotalDurationMS: run.TotalDurationMS, ErrorCategory: run.ErrorCategory,
			ErrorMessage: s.Redactor.Redact(run.ErrorMessage), HasReport: run.ReportJSON != ""})
	}
	writeJSON(w, http.StatusOK, map[string]any{"backup": s.dto(b), "runs": out})
}

func (s *Server) getReport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if security.ValidateID(id) != nil {
		writeErr(w, http.StatusBadRequest, "invalid run id")
		return
	}
	run, err := s.Store.GetRun(r.Context(), id)
	if errors.Is(err, metadata.ErrNotFound) || (err == nil && run.ReportJSON == "") {
		writeErr(w, http.StatusNotFound, "report not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot read metadata")
		return
	}
	var rep report.Report
	if err := json.Unmarshal([]byte(run.ReportJSON), &rep); err != nil {
		writeErr(w, http.StatusInternalServerError, "stored report is unreadable")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// startVerify runs a REAL verification (same engine as `unth verify`) in the
// background. At most one verification per backup at a time.
func (s *Server) startVerify(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if security.ValidateID(id) != nil {
		writeErr(w, http.StatusBadRequest, "invalid backup id")
		return
	}
	b, err := s.Store.GetBackup(r.Context(), id)
	if errors.Is(err, metadata.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "backup not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot read metadata")
		return
	}
	if b.Status != metadata.BackupCreated {
		writeErr(w, http.StatusConflict, "only BACKUP_CREATED backups can be verified (status "+b.Status+")")
		return
	}
	s.mu.Lock()
	if s.verifying[id] {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "a verification of this backup is already running")
		return
	}
	s.verifying[id] = true
	s.mu.Unlock()
	s.auditEvent(r.Context(), "ui.verify", id, "started", "client="+clientIP(r))
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.verifying, id)
			s.mu.Unlock()
		}()
		eng := &verify.Engine{Store: s.Store, Repo: s.Repo, Docker: s.Docker, Redactor: s.Redactor, Log: s.Log}
		if _, err := eng.Run(s.bgCtx, id, verify.Options{}); err != nil {
			s.Log.Warn("ui verification failed to run", "backup_id", id, "error", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "backup_id": id})
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	evs, err := s.Store.ListAudit(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot read metadata")
		return
	}
	type ev struct {
		ID      int64     `json:"id"`
		At      time.Time `json:"at"`
		Actor   string    `json:"actor"`
		Action  string    `json:"action"`
		Target  string    `json:"target"`
		Outcome string    `json:"outcome"`
		Details string    `json:"details"`
	}
	out := make([]ev, 0, len(evs))
	for _, e := range evs {
		out = append(out, ev{e.ID, e.At, e.Actor, e.Action, e.Target, e.Outcome, s.Redactor.Redact(e.Details)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"version": buildinfo.Version, "commit": buildinfo.Commit, "go": buildinfo.GoVersion(),
		"data_dir": s.Repo.Root, "tls": s.TLS}
	if free, err := repo.FreeBytes(s.Repo.Root); err == nil {
		out["free_bytes"] = free
	}
	if v, err := s.Store.SchemaVersion(r.Context()); err == nil {
		out["metadata_schema"] = v
	}
	if s.Docker != nil {
		if dc, err := s.Docker(); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			if v, err := dc.Negotiate(ctx); err == nil {
				out["docker"] = "Engine " + v.Version
			} else {
				out["docker"] = "unreachable"
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) auditEvent(ctx context.Context, action, target, outcome, details string) {
	_ = s.Store.Audit(context.WithoutCancel(ctx), metadata.AuditEvent{Actor: "ui", Action: action, Target: target,
		Outcome: outcome, Details: details})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
