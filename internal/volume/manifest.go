// Package volume implements file-level Docker volume backups and their
// verification.
//
// Archives are produced and extracted ONLY by the Docker daemon through the
// container archive API on never-started helper containers; unth never
// extracts a backup onto the host filesystem. unth only reads the tar stream
// to build and check a per-file manifest (type, mode, owner, size, SHA-256,
// symlink target), rejecting entries that could escape the volume root.
package volume

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// Root is the mount point of the volume inside helper containers; archive
// entries are named "data/...".
const Root = "data"

// Limits bound what a (possibly malicious) archive can make us process.
type Limits struct {
	MaxEntries   int
	MaxTotalSize int64
	MaxNameLen   int
}

// DefaultLimits are generous for real volumes but finite.
var DefaultLimits = Limits{MaxEntries: 10_000_000, MaxTotalSize: 1 << 40, MaxNameLen: 4096}

// ErrUnsafeArchive marks archives with entries that must never be trusted.
var ErrUnsafeArchive = errors.New("unsafe archive entry")

// Entry describes one filesystem object relative to the volume root ("." is the root).
type Entry struct {
	Path   string `json:"path"`
	Type   string `json:"type"` // file, dir, symlink, hardlink, char, block, fifo
	Mode   int64  `json:"mode"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Link   string `json:"link,omitempty"`
}

// FileManifest is the verifiable content description of a volume.
type FileManifest struct {
	Entries    []Entry `json:"entries"`
	Files      int     `json:"files"`
	Dirs       int     `json:"dirs"`
	Symlinks   int     `json:"symlinks"`
	Other      int     `json:"other"`
	TotalBytes int64   `json:"total_bytes"`
}

func (e Entry) line() string {
	return fmt.Sprintf("%s|%s|%o|%d|%d|%d|%s|%s", e.Path, e.Type, e.Mode, e.UID, e.GID, e.Size, e.SHA256, e.Link)
}

// Digest is a SHA-256 over the sorted entry lines.
func (m *FileManifest) Digest() string {
	h := sha256.New()
	for _, e := range m.Entries {
		_, _ = io.WriteString(h, e.line()+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func typeOf(t byte) (string, bool) {
	switch t {
	case tar.TypeReg: // archive/tar normalizes legacy TypeRegA to TypeReg
		return "file", true
	case tar.TypeDir:
		return "dir", true
	case tar.TypeSymlink:
		return "symlink", true
	case tar.TypeLink:
		return "hardlink", true
	case tar.TypeChar:
		return "char", true
	case tar.TypeBlock:
		return "block", true
	case tar.TypeFifo:
		return "fifo", true
	}
	return "", false
}

// relPath maps an archive name ("data/x/y") to a volume-relative path ("x/y"),
// rejecting anything outside the root.
func relPath(name string, max int) (string, error) {
	if len(name) > max || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: invalid name", ErrUnsafeArchive)
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("%w: absolute path %q", ErrUnsafeArchive, name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: parent reference in %q", ErrUnsafeArchive, name)
		}
	}
	clean := path.Clean(name)
	if clean == Root {
		return ".", nil
	}
	if !strings.HasPrefix(clean, Root+"/") {
		return "", fmt.Errorf("%w: %q is outside the volume root", ErrUnsafeArchive, name)
	}
	return strings.TrimPrefix(clean, Root+"/"), nil
}

// Scan reads a whole tar stream produced by the Docker archive API for /data
// and returns its manifest. It fails on unsafe entries and on limit overruns.
func Scan(r io.Reader, lim Limits) (*FileManifest, error) {
	tr := tar.NewReader(r)
	m := &FileManifest{}
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if len(m.Entries) >= lim.MaxEntries {
			return nil, fmt.Errorf("%w: more than %d entries", ErrUnsafeArchive, lim.MaxEntries)
		}
		typ, ok := typeOf(h.Typeflag)
		if !ok {
			// PAX/GNU metadata headers are consumed by archive/tar itself;
			// anything else is unexpected.
			return nil, fmt.Errorf("%w: unsupported entry type %q for %q", ErrUnsafeArchive, h.Typeflag, h.Name)
		}
		rel, err := relPath(h.Name, lim.MaxNameLen)
		if err != nil {
			return nil, err
		}
		if seen[rel] {
			return nil, fmt.Errorf("%w: duplicate entry %q", ErrUnsafeArchive, rel)
		}
		seen[rel] = true
		e := Entry{Path: rel, Type: typ, Mode: h.Mode & 0o7777, UID: h.Uid, GID: h.Gid}
		switch typ {
		case "file":
			m.TotalBytes += h.Size
			if h.Size < 0 || m.TotalBytes > lim.MaxTotalSize {
				return nil, fmt.Errorf("%w: archive exceeds size limit", ErrUnsafeArchive)
			}
			hs := sha256.New()
			n, err := io.CopyN(hs, tr, h.Size) // bounded by the header size checked above
			if err != nil {
				return nil, fmt.Errorf("read %q: %w", rel, err)
			}
			e.Size, e.SHA256 = n, hex.EncodeToString(hs.Sum(nil))
			m.Files++
		case "dir":
			m.Dirs++
		case "symlink":
			e.Link = h.Linkname
			m.Symlinks++
		case "hardlink":
			target, err := relPath(h.Linkname, lim.MaxNameLen)
			if err != nil {
				return nil, fmt.Errorf("hardlink %q: %w", rel, err)
			}
			e.Link = target
			m.Other++
		default:
			m.Other++
		}
		m.Entries = append(m.Entries, e)
	}
	if !seen["."] {
		return nil, fmt.Errorf("%w: archive has no %s/ root entry", ErrUnsafeArchive, Root)
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	return m, nil
}

// Mismatch is one difference between two manifests.
type Mismatch struct {
	Path     string `json:"path"`
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// Comparison is the result of comparing a restored volume with the backup.
type Comparison struct {
	EntriesExpected int        `json:"entries_expected"`
	EntriesActual   int        `json:"entries_actual"`
	BytesExpected   int64      `json:"bytes_expected"`
	BytesActual     int64      `json:"bytes_actual"`
	Mismatches      []Mismatch `json:"mismatches"`
	MismatchCount   int        `json:"mismatch_count"`
}

// OK reports whether the manifests are identical.
func (c *Comparison) OK() bool { return c.MismatchCount == 0 }

const maxListed = 50

// Compare checks every attribute of every entry. The root directory's
// owner/mode are included (they are part of the volume).
func Compare(expected, actual *FileManifest) *Comparison {
	c := &Comparison{EntriesExpected: len(expected.Entries), EntriesActual: len(actual.Entries),
		BytesExpected: expected.TotalBytes, BytesActual: actual.TotalBytes, Mismatches: []Mismatch{}}
	add := func(m Mismatch) {
		c.MismatchCount++
		if len(c.Mismatches) < maxListed {
			c.Mismatches = append(c.Mismatches, m)
		}
	}
	act := make(map[string]Entry, len(actual.Entries))
	for _, e := range actual.Entries {
		act[e.Path] = e
	}
	for _, e := range expected.Entries {
		a, ok := act[e.Path]
		if !ok {
			add(Mismatch{e.Path, "present", "yes", "missing"})
			continue
		}
		delete(act, e.Path)
		check := func(field, x, y string) {
			if x != y {
				add(Mismatch{e.Path, field, x, y})
			}
		}
		check("type", e.Type, a.Type)
		check("mode", fmt.Sprintf("%o", e.Mode), fmt.Sprintf("%o", a.Mode))
		check("owner", fmt.Sprintf("%d:%d", e.UID, e.GID), fmt.Sprintf("%d:%d", a.UID, a.GID))
		check("size", fmt.Sprint(e.Size), fmt.Sprint(a.Size))
		check("sha256", e.SHA256, a.SHA256)
		check("link", e.Link, a.Link)
	}
	extra := make([]string, 0, len(act))
	for p := range act {
		extra = append(extra, p)
	}
	sort.Strings(extra)
	for _, p := range extra {
		add(Mismatch{p, "present", "no", "unexpected"})
	}
	return c
}
