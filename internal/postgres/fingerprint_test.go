package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/unmadethings/unthrecover/internal/failure"
)

func base() *Fingerprint {
	return &Fingerprint{
		Tables: []TableFingerprint{
			{Schema: "public", Name: "a", Rows: 10, Hash: "h1", Hashed: true},
			{Schema: "public", Name: "b", Rows: 5, Hash: "", Hashed: false},
		},
		Objects: map[string]*ObjectSet{
			"constraints": {Count: 2, Hash: "c1", Items: []string{"public.a.a_pkey|p|PRIMARY KEY (id)", "public.a.chk|c|CHECK (x > 0)"}},
		},
		Queries:        []QueryFingerprint{{Name: "q", Rows: 1, Hash: "qh"}},
		SequenceValues: map[string]int64{"public.a_id_seq": 10, "public.unused_seq": -1},
	}
}

func clone(f *Fingerprint) *Fingerprint {
	c := *f
	c.Tables = append([]TableFingerprint{}, f.Tables...)
	c.Queries = append([]QueryFingerprint{}, f.Queries...)
	c.SequenceValues = map[string]int64{}
	for k, v := range f.SequenceValues {
		c.SequenceValues[k] = v
	}
	c.Objects = map[string]*ObjectSet{}
	for k, v := range f.Objects {
		vv := *v
		vv.Items = append([]string{}, v.Items...)
		c.Objects[k] = &vv
	}
	return &c
}

func TestCompareIdentical(t *testing.T) {
	c := Compare(base(), clone(base()))
	if !c.OK() || c.TablesChecked != 2 || c.TablesHashed != 1 || c.RowsExpected != 15 || c.QueriesChecked != 1 {
		t.Fatalf("unexpected comparison: %+v", c)
	}
}

func TestCompareDetectsDifferences(t *testing.T) {
	a := clone(base())
	a.Tables[0].Rows = 9
	if c := Compare(base(), a); len(c.DataMismatches) != 1 || c.DataMismatches[0].Check != "row_count" {
		t.Fatalf("row count: %+v", c.DataMismatches)
	}
	a = clone(base())
	a.Tables[0].Hash = "other"
	if c := Compare(base(), a); len(c.DataMismatches) != 1 || c.DataMismatches[0].Check != "content_hash" {
		t.Fatalf("hash: %+v", c.DataMismatches)
	}
	a = clone(base())
	a.Tables = a.Tables[:1]
	if c := Compare(base(), a); c.OK() {
		t.Fatal("missing table not detected")
	}
	a = clone(base())
	a.Tables = append(a.Tables, TableFingerprint{Schema: "public", Name: "zz"})
	if c := Compare(base(), a); c.OK() {
		t.Fatal("extra table not detected")
	}
	a = clone(base())
	a.Objects["constraints"] = &ObjectSet{Count: 1, Hash: "c2", Items: []string{"public.a.a_pkey|p|PRIMARY KEY (id)"}}
	c := Compare(base(), a)
	if len(c.SchemaMismatches) != 1 || c.SchemaMismatches[0].Object != "public.a.chk|c|CHECK (x > 0)" {
		t.Fatalf("missing constraint: %+v", c.SchemaMismatches)
	}
	a = clone(base())
	delete(a.Objects, "constraints")
	if c := Compare(base(), a); c.OK() {
		t.Fatal("uncaptured object category not detected")
	}
	a = clone(base())
	a.Queries[0].Hash = "zz"
	if c := Compare(base(), a); len(c.QueryMismatches) != 1 {
		t.Fatal("query mismatch not detected")
	}
	a = clone(base())
	a.Queries = nil
	if c := Compare(base(), a); len(c.QueryMismatches) != 1 {
		t.Fatal("missing query not detected")
	}
}

func TestCompareSequencesAreMonotonic(t *testing.T) {
	a := clone(base())
	a.SequenceValues["public.a_id_seq"] = 15 // advanced concurrently after the snapshot: fine
	if c := Compare(base(), a); !c.OK() || c.SequencesChecked != 2 {
		t.Fatalf("advanced sequence should pass: %+v", c)
	}
	a.SequenceValues["public.a_id_seq"] = 9 // went backwards: duplicate-key risk
	if c := Compare(base(), a); len(c.DataMismatches) != 1 || c.DataMismatches[0].Check != "sequence_value" {
		t.Fatalf("regressed sequence not detected: %+v", c.DataMismatches)
	}
}

func TestDigestChangesWithContent(t *testing.T) {
	a, b := base(), clone(base())
	if a.Digest() != b.Digest() {
		t.Fatal("digest not deterministic")
	}
	b.Tables[1].Rows++
	if a.Digest() == b.Digest() {
		t.Fatal("digest ignores row counts")
	}
}

func TestCheckDumpCompatibility(t *testing.T) {
	tool := &ToolInfo{Name: "pg_dump", Major: 16}
	if err := CheckDumpCompatibility(tool, 16); err != nil {
		t.Fatal(err)
	}
	if err := CheckDumpCompatibility(tool, 13); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int{17, 18, 11, 9} {
		if err := CheckDumpCompatibility(tool, v); failure.CategoryOf(err) != failure.UnsupportedVersion {
			t.Errorf("server %d: got %v", v, err)
		}
	}
}

func TestClassifyErrors(t *testing.T) {
	auth := ClassifyConnError(&pgconn.PgError{Code: "28P01", Message: "password authentication failed"})
	if failure.CategoryOf(auth) != failure.AuthenticationFail {
		t.Fatalf("auth: %v", auth)
	}
	if failure.CategoryOf(ClassifyConnError(errors.New("dial tcp: connection refused"))) != failure.ConnectionFailed {
		t.Fatal("conn refused")
	}
	if !errors.Is(ClassifyConnError(context.Canceled), context.Canceled) {
		t.Fatal("cancel must pass through")
	}
	cases := map[string]failure.Category{
		`FATAL:  password authentication failed for user "x"`: failure.AuthenticationFail,
		"aborting because of server version mismatch":         failure.UnsupportedVersion,
		"could not connect to server: Connection refused":     failure.ConnectionFailed,
		"could not write: No space left on device":            failure.InsufficientStorage,
		"something else": failure.BackupFailed,
	}
	for msg, want := range cases {
		if got := failure.CategoryOf(classifyToolError("pg_dump failed", msg, errors.New("exit 1"))); got != want {
			t.Errorf("%q: got %s want %s", msg, got, want)
		}
	}
}

func TestToolEnvDoesNotLeakPasswordOrInherit(t *testing.T) {
	t.Setenv("PGPASSWORD", "ambient-secret")
	t.Setenv("PGSERVICE", "evil")
	env := toolEnv(ConnParams{Host: "h", Port: 1, Database: "d", User: "u", Password: "p4ssw0rd"}, "/tmp/pf")
	for _, kv := range env {
		if kv == "PGPASSWORD=ambient-secret" || kv == "PGSERVICE=evil" {
			t.Fatalf("ambient variable inherited: %s", kv)
		}
		if strings.Contains(kv, "p4ssw0rd") {
			t.Fatalf("password placed in environment: %s", kv)
		}
	}
}
