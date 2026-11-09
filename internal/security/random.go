package security

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// RandomHex returns n random bytes encoded as lowercase hex.
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the platform is unusable for secrets.
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(b)
}

// NewID returns a sortable, collision-resistant identifier with a prefix,
// e.g. "bk-20261109t181640-3f9a1c2b". It always satisfies ValidateID.
func NewID(prefix string) string {
	return fmt.Sprintf("%s-%s-%s", prefix, time.Now().UTC().Format("20060102t150405"), RandomHex(4))
}
