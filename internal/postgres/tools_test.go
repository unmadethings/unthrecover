package postgres

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func fakeTool(t *testing.T, dir, name, version string) {
	t.Helper()
	_ = os.MkdirAll(dir, 0o755)
	script := "#!/bin/sh\necho '" + name + " (PostgreSQL) " + version + "'\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestFindToolPrefersMatchingMajor(t *testing.T) {
	root := t.TempDir()
	old := versionedDirs
	versionedDirs = []string{filepath.Join(root, "pg%d", "bin")}
	t.Cleanup(func() { versionedDirs = old })

	fakeTool(t, filepath.Join(root, "pg16", "bin"), "pg_dump", "16.4")
	fakeTool(t, filepath.Join(root, "pg13", "bin"), "pg_dump", "13.9")
	fakeTool(t, filepath.Join(root, "path"), "pg_dump", "17.2")
	t.Setenv("PATH", filepath.Join(root, "path")+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := context.Background()

	if tl, err := FindTool(ctx, "pg_dump", 16); err != nil || tl.Major != 16 {
		t.Fatalf("major 16: %+v %v", tl, err)
	}
	if tl, err := FindTool(ctx, "pg_dump", 13); err != nil || tl.Major != 13 {
		t.Fatalf("major 13: %+v %v", tl, err)
	}
	// no exact match -> nearest newer installed major, not the newest
	if tl, err := FindTool(ctx, "pg_dump", 12); err != nil || tl.Major != 13 {
		t.Fatalf("nearest newer for 12: %+v %v", tl, err)
	}
	if tl, err := FindTool(ctx, "pg_dump", 15); err != nil || tl.Major != 16 {
		t.Fatalf("nearest newer for 15: %+v %v", tl, err)
	}
	// nothing installed at or above the major -> PATH tool
	if tl, err := FindTool(ctx, "pg_dump", 17); err != nil || tl.Major != 17 {
		t.Fatalf("fallback: %+v %v", tl, err)
	}
	if ms := InstalledMajors(ctx, "pg_dump"); len(ms) != 2 || ms[0] != 13 || ms[1] != 16 {
		t.Fatalf("installed majors %v", ms)
	}
	// a directory whose binary reports a different major is not trusted
	fakeTool(t, filepath.Join(root, "pg14", "bin"), "pg_dump", "12.1")
	if tl, _ := FindTool(ctx, "pg_dump", 14); tl.Major != 16 {
		t.Fatalf("mismatched versioned dir accepted: %+v", tl)
	}
	if MajorOf("pg_dump (PostgreSQL) 16.15 (Ubuntu 16.15-1)") != 16 || MajorOf("garbage") != 0 {
		t.Fatal("MajorOf")
	}

	t.Setenv("UNTH_PG_BINDIR", filepath.Join(root, "pg13", "bin"))
	if tl, err := FindTool(ctx, "pg_dump", 16); err != nil || tl.Major != 13 {
		t.Fatalf("UNTH_PG_BINDIR override: %+v %v", tl, err)
	}
	t.Setenv("UNTH_PG_BINDIR", filepath.Join(root, "nowhere"))
	if _, err := FindTool(ctx, "pg_dump", 16); err == nil {
		t.Fatal("missing UNTH_PG_BINDIR tool accepted")
	}
}
