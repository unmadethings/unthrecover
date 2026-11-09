//go:build integration

package integration

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLargeDatabase is opt-in: set UNTH_TEST_LARGE_ROWS (e.g. 5000000).
// It backs up and verifies a database whose main table exceeds the content
// hash threshold, checks that the report says so, and logs timings.
func TestLargeDatabase(t *testing.T) {
	rows, _ := strconv.Atoi(os.Getenv("UNTH_TEST_LARGE_ROWS"))
	if rows <= 0 {
		t.Skip("NOT RUN: opt-in, set UNTH_TEST_LARGE_ROWS")
	}
	db := "large_" + randHex(3)
	if err := psql(src, "postgres", "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	seedStart := time.Now()
	if err := psql(src, db, fmt.Sprintf(`
		CREATE TABLE events (id bigint PRIMARY KEY, account int NOT NULL, kind text NOT NULL,
			payload jsonb NOT NULL, created_at timestamptz NOT NULL);
		INSERT INTO events SELECT g, g %% 10000, (ARRAY['a','b','c'])[1 + g %% 3],
			jsonb_build_object('n', g, 'h', md5(g::text), 'pad', repeat('x', 120)),
			'2026-01-01'::timestamptz + (g || ' seconds')::interval
		FROM generate_series(1, %d) g;
		CREATE INDEX ON events (account, created_at);
		CREATE TABLE accounts AS SELECT g AS id, 'acct ' || g AS name FROM generate_series(0, 9999) g;
		ALTER TABLE accounts ADD PRIMARY KEY (id);
		ANALYZE;`, rows)); err != nil {
		t.Fatal(err)
	}
	size := psqlValue(t, src, db, "SELECT pg_size_pretty(pg_database_size(current_database()))")
	t.Logf("seeded %d rows (%s) in %s", rows, size, time.Since(seedStart).Round(time.Second))

	e := newEnv(t)
	start := time.Now()
	id := e.mustBackup(e.config(cfgOpts{db: db, timeout: "60m"}))
	backupDur := time.Since(start)

	start = time.Now()
	rep, r := e.verify(id)
	verifyDur := time.Since(start)
	if r.code != 0 || rep.FinalStatus != "VERIFIED" {
		t.Fatalf("code=%d rep=%+v stderr=%s", r.code, rep, r.stderr)
	}
	if rep.Comparison.TablesHashed != rep.Comparison.TablesChecked-1 {
		t.Fatalf("expected exactly one table above the hash threshold: %+v", rep.Comparison)
	}
	found := false
	for _, nc := range rep.Scope.NotCovered {
		if strings.Contains(nc, "row-level content of 1 tables larger than") {
			found = true
		}
	}
	if !found {
		t.Fatalf("report must state that the large table's content was not hashed: %v", rep.Scope.NotCovered)
	}
	fi, _ := os.Stat(e.artifactPath(id))
	var restoreMS int64
	rj := e.run("report", id, "--format", "json")
	if i := strings.Index(rj.stdout, `"observed_test_restore_duration_ms": `); i >= 0 {
		fmt.Sscanf(rj.stdout[i+len(`"observed_test_restore_duration_ms": `):], "%d", &restoreMS)
	}
	t.Logf("LARGE-DB RESULT rows=%d db_size=%s artifact=%.1fMiB backup=%s verify_total=%s restore=%s",
		rows, size, float64(fi.Size())/(1<<20), backupDur.Round(time.Second), verifyDur.Round(time.Second),
		(time.Duration(restoreMS) * time.Millisecond).Round(time.Second))
	e.assertNoResources(rep.RunID)
	_ = psql(src, "postgres", "DROP DATABASE "+db)
}
