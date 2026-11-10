package offsite

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/unmadethings/unthrecover/internal/security"
)

// Key file names written by GenerateKeys.
const (
	AgeKeyFile     = "age.key"  // age X25519 identity (secret)
	SignKeyFile    = "sign.key" // Ed25519 private key, PKCS#8 PEM (secret)
	SignPubKeyFile = "sign.pub" // Ed25519 public key, PKIX PEM
)

// Keys holds the loaded encryption and signing keys of a job.
type Keys struct {
	Identity   *age.X25519Identity // nil: copies are not encrypted
	Recipients []age.Recipient     // identity's recipient + extra recipients
	RecipientS []string            // the same as age1... strings, recorded in the descriptor
	SignKey    ed25519.PrivateKey  // nil: copies are not signed
	VerifyKey  ed25519.PublicKey   // nil: signatures are not checked
}

// GeneratedKeys describes the result of GenerateKeys.
type GeneratedKeys struct {
	Dir          string `json:"dir"`
	AgeRecipient string `json:"age_public_key"`
	SignPublic   string `json:"signing_public_key_fingerprint"`
}

// GenerateKeys creates a new age identity and Ed25519 signing key in dir.
// Existing key files are never overwritten.
func GenerateKeys(dir string) (*GeneratedKeys, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for _, n := range []string{AgeKeyFile, SignKeyFile, SignPubKeyFile} {
		if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
			return nil, fmt.Errorf("%s already exists in %s; refusing to overwrite a key", n, dir)
		}
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	created := time.Now().UTC().Format(time.RFC3339)
	ageText := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n", created, id.Recipient(), id)
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{AgeKeyFile, []byte(ageText), 0o600},
		{SignKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600},
		{SignPubKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		fh, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, f.mode) // #nosec G304 -- operator-chosen key directory
		if err != nil {
			return nil, err
		}
		if _, err := fh.Write(f.data); err != nil {
			_ = fh.Close()
			return nil, err
		}
		if err := fh.Close(); err != nil {
			return nil, err
		}
	}
	return &GeneratedKeys{Dir: dir, AgeRecipient: id.Recipient().String(), SignPublic: Fingerprint(pub)}, nil
}

// Fingerprint is a short, stable identifier of an Ed25519 public key.
func Fingerprint(pub ed25519.PublicKey) string {
	s := sha256.Sum256(pub)
	return "ed25519:" + hex.EncodeToString(s[:16])
}

// LoadKeys loads the keys configured for a job. identityFile and signKeyFile
// must be private (0600). Empty paths disable the corresponding feature.
func LoadKeys(identityFile string, extraRecipients []string, signKeyFile, verifyKeyFile string) (*Keys, error) {
	k := &Keys{}
	if identityFile != "" {
		text, err := security.ReadSecretFile(identityFile)
		if err != nil {
			return nil, fmt.Errorf("encryption.identity_file: %w", err)
		}
		ids, err := age.ParseIdentities(strings.NewReader(text))
		if err != nil {
			return nil, fmt.Errorf("encryption.identity_file: %w", err)
		}
		if len(ids) != 1 {
			return nil, errors.New("encryption.identity_file: must contain exactly one age identity")
		}
		x, ok := ids[0].(*age.X25519Identity)
		if !ok {
			return nil, errors.New("encryption.identity_file: only X25519 identities (AGE-SECRET-KEY-1...) are supported")
		}
		k.Identity = x
		k.Recipients = append(k.Recipients, x.Recipient())
		k.RecipientS = append(k.RecipientS, x.Recipient().String())
		for _, r := range extraRecipients {
			rec, err := age.ParseX25519Recipient(r)
			if err != nil {
				return nil, fmt.Errorf("encryption.recipients: %w", err)
			}
			k.Recipients = append(k.Recipients, rec)
			k.RecipientS = append(k.RecipientS, rec.String())
		}
	}
	if signKeyFile != "" {
		text, err := security.ReadSecretFile(signKeyFile)
		if err != nil {
			return nil, fmt.Errorf("signing.key_file: %w", err)
		}
		blk, _ := pem.Decode([]byte(text))
		if blk == nil || blk.Type != "PRIVATE KEY" {
			return nil, errors.New("signing.key_file: not a PEM private key")
		}
		key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("signing.key_file: %w", err)
		}
		priv, ok := key.(ed25519.PrivateKey)
		if !ok {
			return nil, errors.New("signing.key_file: not an Ed25519 key")
		}
		k.SignKey = priv
		k.VerifyKey = priv.Public().(ed25519.PublicKey)
	}
	if verifyKeyFile != "" {
		raw, err := os.ReadFile(verifyKeyFile) // #nosec G304 -- operator-configured public key
		if err != nil {
			return nil, fmt.Errorf("signing.public_key_file: %w", err)
		}
		blk, _ := pem.Decode(raw)
		if blk == nil || blk.Type != "PUBLIC KEY" {
			return nil, errors.New("signing.public_key_file: not a PEM public key")
		}
		key, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("signing.public_key_file: %w", err)
		}
		pub, ok := key.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("signing.public_key_file: not an Ed25519 key")
		}
		k.VerifyKey = pub
	}
	return k, nil
}
