package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/security"
)

func notifier(typ, on string) config.Notifier {
	return config.Notifier{Name: "n", Type: typ, URLEnv: "UNTH_TEST_HOOK", SecretEnv: "", On: on, Timeout: config.Duration(5 * time.Second)}
}

func TestWebhookSignedDelivery(t *testing.T) {
	var gotBody []byte
	var gotSig, gotTS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig, gotTS = r.Header.Get("X-Unth-Signature"), r.Header.Get("X-Unth-Timestamp")
		w.WriteHeader(204)
	}))
	defer srv.Close()
	t.Setenv("UNTH_TEST_HOOK", srv.URL+"/hook?token=supersecrettoken")
	t.Setenv("UNTH_TEST_SECRET", "hmac-secret-value")
	n := notifier(config.NotifyWebhook, config.OnAlways)
	n.SecretEnv = "UNTH_TEST_SECRET"
	s := &Sender{Redactor: security.NewRedactor(), Now: func() time.Time { return time.Unix(1700000000, 0) }}
	ev := &Event{Event: "unth.run.completed", Workload: "db", Outcome: "success"}
	res := s.SendAll(context.Background(), []config.Notifier{n}, ev)
	if !res[0].Sent {
		t.Fatalf("not sent: %+v", res)
	}
	if gotTS != "1700000000" || gotSig != "sha256="+Sign("hmac-secret-value", gotTS, gotBody) {
		t.Fatalf("bad signature %q ts %q", gotSig, gotTS)
	}
	var back Event
	if json.Unmarshal(gotBody, &back) != nil || back.Workload != "db" {
		t.Fatalf("body %s", gotBody)
	}
	// URL and secret are registered for redaction.
	if out := s.Redactor.Redact(srv.URL + "/hook?token=supersecrettoken hmac-secret-value"); strings.Contains(out, "supersecrettoken") || strings.Contains(out, "hmac-secret-value") {
		t.Fatalf("not redacted: %s", out)
	}
}

func TestOnFailureSkipsSuccessAndSlackFormat(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	t.Setenv("UNTH_TEST_HOOK", srv.URL)
	s := &Sender{Redactor: security.NewRedactor()}
	n := notifier(config.NotifySlack, config.OnFailure)
	res := s.SendAll(context.Background(), []config.Notifier{n}, &Event{Outcome: "success"})
	if res[0].Sent || res[0].Skipped == "" {
		t.Fatalf("success must be skipped for on=failure: %+v", res)
	}
	res = s.SendAll(context.Background(), []config.Notifier{n}, &Event{Outcome: "failure", Workload: "db",
		ErrorCategory: "CHECKSUM_MISMATCH", ErrorMessage: "bad", Hint: "take a new backup"})
	if !res[0].Sent || !strings.Contains(got["text"], "CHECKSUM_MISMATCH") || !strings.Contains(got["text"], "FAILURE") {
		t.Fatalf("slack: %+v %v", res, got)
	}
}

func TestDeliveryErrorsDoNotLeakURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	t.Setenv("UNTH_TEST_HOOK", srv.URL+"/secret-path-token")
	s := &Sender{Redactor: security.NewRedactor()}
	res := s.SendAll(context.Background(), []config.Notifier{notifier(config.NotifyWebhook, config.OnAlways)}, &Event{Outcome: "failure"})
	if res[0].Sent || !strings.Contains(res[0].Error, "HTTP 500") || strings.Contains(res[0].Error, "secret-path-token") {
		t.Fatalf("%+v", res)
	}
	// unreachable endpoint
	t.Setenv("UNTH_TEST_HOOK", "http://127.0.0.1:1/secret-path-token")
	res = s.SendAll(context.Background(), []config.Notifier{notifier(config.NotifyWebhook, config.OnAlways)}, &Event{Outcome: "failure"})
	if res[0].Sent || strings.Contains(res[0].Error, "secret-path-token") {
		t.Fatalf("%+v", res)
	}
}

func TestURLPolicy(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://hooks.example.com/x": true,
		"http://127.0.0.1:8080/x":     true,
		"http://localhost/x":          true,
		"http://[::1]/x":              true,
		"http://hooks.example.com/x":  false,
		"ftp://example.com":           false,
		"https://user:pw@example.com": false,
	} {
		u, _ := url.Parse(raw)
		if err := checkURL(u); (err == nil) != ok {
			t.Errorf("%s: got %v", raw, err)
		}
	}
	t.Setenv("UNTH_TEST_HOOK", "")
	s := &Sender{Redactor: security.NewRedactor()}
	res := s.SendAll(context.Background(), []config.Notifier{notifier(config.NotifyWebhook, config.OnAlways)}, &Event{Outcome: "failure"})
	if res[0].Sent || !strings.Contains(res[0].Error, "not set") {
		t.Fatalf("%+v", res)
	}
}

func TestNoRedirectFollowing(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	t.Setenv("UNTH_TEST_HOOK", srv.URL)
	s := &Sender{Redactor: security.NewRedactor()}
	res := s.SendAll(context.Background(), []config.Notifier{notifier(config.NotifyWebhook, config.OnAlways)}, &Event{Outcome: "failure"})
	if res[0].Sent {
		t.Fatal("redirect response must count as failure")
	}
}
