package volume

import (
	"archive/tar"
	"bytes"
	"errors"
	"testing"
)

type ent struct {
	name, body, link string
	typ              byte
	mode             int64
	uid              int
}

func mkTar(t *testing.T, es []ent) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Uid: e.uid, Linkname: e.link, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	return buf.Bytes()
}

var sample = []ent{
	{name: "data/", typ: tar.TypeDir, mode: 0o755},
	{name: "data/a.txt", typ: tar.TypeReg, mode: 0o640, uid: 1234, body: "hello\n"},
	{name: "data/sub/", typ: tar.TypeDir, mode: 0o755},
	{name: "data/sub/b", typ: tar.TypeReg, mode: 0o644, body: "x"},
	{name: "data/link", typ: tar.TypeSymlink, mode: 0o777, link: "a.txt"},
}

func TestScanAndCompare(t *testing.T) {
	m, err := Scan(bytes.NewReader(mkTar(t, sample)), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if m.Files != 2 || m.Dirs != 2 || m.Symlinks != 1 || m.TotalBytes != 7 || m.Entries[0].Path != "." {
		t.Fatalf("%+v", m)
	}
	m2, _ := Scan(bytes.NewReader(mkTar(t, sample)), DefaultLimits)
	if m.Digest() != m2.Digest() || !Compare(m, m2).OK() {
		t.Fatal("identical archives differ")
	}

	changed := append([]ent{}, sample...)
	changed[1].body = "hellO\n"
	changed[3].mode = 0o600
	changed[4].link = "elsewhere"
	m3, _ := Scan(bytes.NewReader(mkTar(t, changed[:4])), DefaultLimits) // also drops the symlink
	c := Compare(m, m3)
	fields := map[string]bool{}
	for _, x := range c.Mismatches {
		fields[x.Path+":"+x.Field] = true
	}
	for _, want := range []string{"a.txt:sha256", "sub/b:mode", "link:present"} {
		if !fields[want] {
			t.Errorf("missing mismatch %s in %+v", want, c.Mismatches)
		}
	}
	if m.Digest() == m3.Digest() {
		t.Fatal("digest ignores content")
	}
}

func TestScanRejectsUnsafeArchives(t *testing.T) {
	cases := map[string][]ent{
		"absolute":     {{name: "data/", typ: tar.TypeDir}, {name: "/etc/passwd", typ: tar.TypeReg, body: "x"}},
		"traversal":    {{name: "data/", typ: tar.TypeDir}, {name: "data/../../etc/x", typ: tar.TypeReg, body: "x"}},
		"outside root": {{name: "data/", typ: tar.TypeDir}, {name: "other/x", typ: tar.TypeReg, body: "x"}},
		"hardlink out": {{name: "data/", typ: tar.TypeDir}, {name: "data/h", typ: tar.TypeLink, link: "../etc/shadow"}},
		"no root":      {{name: "data/a", typ: tar.TypeReg, body: "x"}},
		"duplicate":    {{name: "data/", typ: tar.TypeDir}, {name: "data/a", typ: tar.TypeReg}, {name: "data/a", typ: tar.TypeReg}},
	}
	for name, es := range cases {
		if _, err := Scan(bytes.NewReader(mkTar(t, es)), DefaultLimits); !errors.Is(err, ErrUnsafeArchive) {
			t.Errorf("%s: expected ErrUnsafeArchive, got %v", name, err)
		}
	}
	if _, err := Scan(bytes.NewReader([]byte("not a tar at all, definitely not")), DefaultLimits); err == nil {
		t.Error("garbage accepted")
	}
	lim := Limits{MaxEntries: 2, MaxTotalSize: 1 << 20, MaxNameLen: 100}
	if _, err := Scan(bytes.NewReader(mkTar(t, sample)), lim); !errors.Is(err, ErrUnsafeArchive) {
		t.Error("entry limit not enforced")
	}
	lim = Limits{MaxEntries: 100, MaxTotalSize: 3, MaxNameLen: 100}
	if _, err := Scan(bytes.NewReader(mkTar(t, sample)), lim); !errors.Is(err, ErrUnsafeArchive) {
		t.Error("size limit not enforced")
	}
}
