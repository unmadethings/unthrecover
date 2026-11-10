// Package offsite copies VERIFIED backups to storage outside the server
// (S3-compatible object storage or any rclone remote), proves each copy by
// downloading and checking it again, applies offsite retention and fetches
// copies back for disaster recovery.
//
// Layout of one copy under <prefix>/<workload>/<backup-id>/:
//
//	artifact.dump.age | volume.tar.age   the artifact (age-encrypted unless allowed otherwise)
//	manifest.json.age                    the backup manifest (encrypted: it names hosts and tables)
//	copy.json                            descriptor: object hashes, recipients (written last)
//	copy.json.sig                        Ed25519 signature of copy.json (when signing is set)
//
// copy.json is uploaded last, so a copy without it is incomplete and ignored.
package offsite

import (
	"context"
	"errors"
	"io"
)

// Backend is a minimal object store.
type Backend interface {
	// Target identifies the destination, e.g. "s3://bucket" or "rclone:gdrive:unth".
	Target() string
	// Put stores r under key. size is -1 when unknown.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Get returns the object content; ErrNotExist when it does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// List returns all keys below prefix (recursive).
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes one object; deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
}

// ErrNotExist is returned by Get for missing objects.
var ErrNotExist = errors.New("object does not exist")
