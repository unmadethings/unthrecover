// Package fakecloud is a minimal control plane implementing pkg/agentproto
// for tests (and as the reference for the real one): enrollment tokens,
// signed agent requests with replay protection, job sets with secrets
// encrypted to the agent's age key, commands and events.
package fakecloud

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/unmadethings/unthrecover/pkg/agentproto"
)

// Agent is an enrolled agent.
type Agent struct {
	ID, Hostname, Recipient string
	Key                     ed25519.PublicKey
	LastHeartbeat           agentproto.Heartbeat
	Heartbeats              int
}

// Server is the fake control plane.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	tokens   map[string]time.Time
	agents   map[string]*Agent
	nonces   map[string]bool
	jobs     map[string]agentproto.JobsResponse
	commands map[string][]agentproto.Command
	Events   []agentproto.Event
	Rejected int // requests with bad signatures
	seq      int
	// Drive answers drive-token requests (connection -> token); nil: 404.
	Drive map[string]agentproto.DriveTokenResponse
}

// New starts the server.
func New() *Server {
	s := &Server{tokens: map[string]time.Time{}, agents: map[string]*Agent{}, nonces: map[string]bool{},
		jobs: map[string]agentproto.JobsResponse{}, commands: map[string][]agentproto.Command{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+agentproto.PathEnroll, s.enroll)
	mux.HandleFunc("POST "+agentproto.PathHeartbeat, s.signed(s.heartbeat))
	mux.HandleFunc("GET "+agentproto.PathJobs, s.signed(s.getJobs))
	mux.HandleFunc("POST "+agentproto.PathEvents, s.signed(s.events))
	mux.HandleFunc("POST "+agentproto.PathDriveToken, s.signed(s.driveToken))
	s.Server = httptest.NewServer(mux)
	return s
}

// NewToken creates a one-time enrollment token valid for an hour.
func (s *Server) NewToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	t := fmt.Sprintf("enr-%d-%d", time.Now().UnixNano(), s.seq)
	s.tokens[t] = time.Now().Add(time.Hour)
	return t
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var req agentproto.EnrollRequest
	if json.NewDecoder(io.LimitReader(r.Body, agentproto.MaxBody)).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if _, aerr := age.ParseX25519Recipient(req.AgeRecipient); err != nil || len(key) != ed25519.PublicKeySize || aerr != nil {
		http.Error(w, "bad keys", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.tokens[req.Token]
	delete(s.tokens, req.Token) // single use
	if !ok || time.Now().After(exp) {
		http.Error(w, "invalid or expired enrollment token", http.StatusForbidden)
		return
	}
	s.seq++
	a := &Agent{ID: fmt.Sprintf("ag-%04d", s.seq), Hostname: req.Hostname, Recipient: req.AgeRecipient, Key: key}
	s.agents[a.ID] = a
	_ = json.NewEncoder(w).Encode(agentproto.EnrollResponse{AgentID: a.ID, OrgName: "Test Org"})
}

func (s *Server) signed(h func(w http.ResponseWriter, a *Agent, body []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, body, err := agentproto.Verify(r, time.Now(), func(id string) (ed25519.PublicKey, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if a, ok := s.agents[id]; ok {
				return a.Key, nil
			}
			return nil, errors.New("unknown agent")
		}, func(id, nonce string) bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			k := id + "/" + nonce
			if s.nonces[k] {
				return true
			}
			s.nonces[k] = true
			return false
		})
		if err != nil {
			s.mu.Lock()
			s.Rejected++
			s.mu.Unlock()
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		s.mu.Lock()
		a := s.agents[id]
		s.mu.Unlock()
		h(w, a, body)
	}
}

func (s *Server) heartbeat(w http.ResponseWriter, a *Agent, body []byte) {
	var hb agentproto.Heartbeat
	_ = json.Unmarshal(body, &hb)
	s.mu.Lock()
	a.LastHeartbeat, a.Heartbeats = hb, a.Heartbeats+1
	resp := agentproto.HeartbeatResponse{Commands: s.commands[a.ID], JobsVersion: s.jobs[a.ID].Version, IntervalSec: 5}
	s.commands[a.ID] = nil
	s.mu.Unlock()
	if resp.Commands == nil {
		resp.Commands = []agentproto.Command{}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) getJobs(w http.ResponseWriter, a *Agent, _ []byte) {
	s.mu.Lock()
	jr := s.jobs[a.ID]
	s.mu.Unlock()
	if jr.Jobs == nil {
		jr.Jobs = []agentproto.Job{}
	}
	_ = json.NewEncoder(w).Encode(jr)
}

func (s *Server) driveToken(w http.ResponseWriter, _ *Agent, body []byte) {
	var req agentproto.DriveTokenRequest
	_ = json.Unmarshal(body, &req)
	s.mu.Lock()
	t, ok := s.Drive[req.Connection]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "unknown connection", http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(t) // #nosec G117 -- test fake: handing out the token is its job
}

func (s *Server) events(w http.ResponseWriter, _ *Agent, body []byte) {
	var evs agentproto.Events
	if json.Unmarshal(body, &evs) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.Events = append(s.Events, evs.Events...)
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// Agents returns the enrolled agents.
func (s *Server) Agents() []*Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Agent
	for _, a := range s.agents {
		out = append(out, a)
	}
	return out
}

// SetJobs replaces an agent's job set; plain secrets are encrypted to the
// agent's age recipient (the control plane keeps only ciphertext).
func (s *Server) SetJobs(agentID, version string, jobs []agentproto.Job, plainSecrets map[string]map[string]string) error {
	s.mu.Lock()
	a := s.agents[agentID]
	s.mu.Unlock()
	if a == nil {
		return errors.New("unknown agent")
	}
	rec, err := age.ParseX25519Recipient(a.Recipient)
	if err != nil {
		return err
	}
	for i := range jobs {
		jobs[i].Secrets = map[string]string{}
		for name, val := range plainSecrets[jobs[i].ID] {
			var buf bytes.Buffer
			aw := armor.NewWriter(&buf)
			ew, err := age.Encrypt(aw, rec)
			if err != nil {
				return err
			}
			_, _ = io.WriteString(ew, val)
			_ = ew.Close()
			_ = aw.Close()
			jobs[i].Secrets[name] = buf.String()
		}
	}
	s.mu.Lock()
	s.jobs[agentID] = agentproto.JobsResponse{Version: version, Jobs: jobs}
	s.mu.Unlock()
	return nil
}

// Command queues a command for an agent.
func (s *Server) Command(agentID string, c agentproto.Command) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands[agentID] = append(s.commands[agentID], c)
}

// TakeEvents returns and clears received events.
func (s *Server) TakeEvents() []agentproto.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.Events
	s.Events = nil
	return e
}
