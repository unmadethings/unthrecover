package agentproto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_800_000_000, 0)
	nonces := map[string]bool{}
	lookup := func(id string) (ed25519.PublicKey, error) {
		if id != "ag-1" {
			return nil, errors.New("unknown")
		}
		return pub, nil
	}
	seen := func(id, n string) bool {
		k := id + n
		if nonces[k] {
			return true
		}
		nonces[k] = true
		return false
	}
	mk := func(method, path string, body []byte, key ed25519.PrivateKey, id, nonce string, at time.Time) *http.Request {
		r, _ := http.NewRequest(method, "https://cloud.example"+path, bytes.NewReader(body))
		Sign(r, id, key, nonce, at, body)
		return r
	}
	body := []byte(`{"version":"1"}`)
	r := mk("POST", PathHeartbeat, body, priv, "ag-1", "nonce-0000000000001", now)
	id, got, err := Verify(r, now, lookup, seen)
	if err != nil || id != "ag-1" || !bytes.Equal(got, body) {
		t.Fatalf("valid: %v %s", err, got)
	}
	// Replay of the same request.
	r = mk("POST", PathHeartbeat, body, priv, "ag-1", "nonce-0000000000001", now)
	if _, _, err := Verify(r, now, lookup, seen); err == nil {
		t.Fatal("replay accepted")
	}
	cases := map[string]*http.Request{
		"wrong key":     mk("POST", PathHeartbeat, body, other, "ag-1", "nonce-0000000000002", now),
		"unknown agent": mk("POST", PathHeartbeat, body, priv, "ag-2", "nonce-0000000000003", now),
		"too old":       mk("POST", PathHeartbeat, body, priv, "ag-1", "nonce-0000000000004", now.Add(-6*time.Minute)),
		"future":        mk("POST", PathHeartbeat, body, priv, "ag-1", "nonce-0000000000005", now.Add(6*time.Minute)),
		"short nonce":   mk("POST", PathHeartbeat, body, priv, "ag-1", "n", now),
	}
	for name, r := range cases {
		if _, _, err := Verify(r, now, lookup, seen); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Tampered body / path / method.
	r = mk("POST", PathHeartbeat, body, priv, "ag-1", "nonce-0000000000006", now)
	r.Body = http.NoBody
	if _, _, err := Verify(r, now, lookup, seen); err == nil {
		t.Fatal("tampered body accepted")
	}
	r = mk("POST", PathHeartbeat, body, priv, "ag-1", "nonce-0000000000007", now)
	r.URL.Path = PathEvents // same body, different endpoint
	r.Body = io.NopCloser(bytes.NewReader(body))
	if _, _, err := Verify(r, now, lookup, seen); err == nil {
		t.Fatal("tampered path accepted")
	}
}

func TestParseSchedule(t *testing.T) {
	for _, ok := range []string{"", "daily 03:00", "every 6h", "every 15m"} {
		if _, _, _, _, err := ParseSchedule(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"daily 3", "every 1m", "every 200h", "cron * * *", "weekly"} {
		if _, _, _, _, err := ParseSchedule(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
