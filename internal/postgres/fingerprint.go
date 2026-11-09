package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/unmadethings/unthrecover/internal/failure"
)

// Connect opens a pgx connection built field-by-field (no DSN string
// concatenation) and classifies connection failures.
func Connect(ctx context.Context, p ConnParams) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, failure.New(failure.Internal, "pgx config", err)
	}
	cfg.Host = p.Host
	cfg.Port = uint16(p.Port) // #nosec G115 -- validated 1..65535
	cfg.Database = p.Database
	cfg.User = p.User
	cfg.Password = p.Password
	cfg.ConnectTimeout = time.Duration(p.ConnectTimeout) * time.Second
	cfg.Fallbacks = nil
	cfg.RuntimeParams = map[string]string{"application_name": "unth-recover"}
	if err := applySSLMode(cfg, p); err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, ClassifyConnError(err)
	}
	return conn, nil
}

func applySSLMode(cfg *pgx.ConnConfig, p ConnParams) error {
	if p.SSLMode == "" || p.SSLMode == "prefer" || p.SSLMode == "allow" || p.SSLMode == "disable" {
		// pgx's ParseConfig("") already produced prefer semantics (TLS with
		// plaintext fallback) unless sslmode=disable, handled below.
		if p.SSLMode == "disable" {
			cfg.TLSConfig = nil
			cfg.Fallbacks = nil
		}
		return nil
	}
	// For require/verify-*, re-parse with an explicit sslmode so pgx builds
	// the right TLS config.
	parsed, err := pgx.ParseConfig("sslmode=" + p.SSLMode + " host=" + p.Host)
	if err != nil {
		return failure.New(failure.ConfigInvalid, "sslmode", err)
	}
	cfg.TLSConfig = parsed.TLSConfig
	return nil
}

// ClassifyConnError maps driver errors to failure categories.
func ClassifyConnError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "28P01", "28000":
			return failure.New(failure.AuthenticationFail, "authentication failed for the configured user", err).
				WithHint("check source.user and the password source (password_env/password_file)")
		case "3D000":
			return failure.New(failure.ConnectionFailed, "database does not exist", err)
		}
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "password authentication failed") || strings.Contains(msg, "sasl auth") {
		return failure.New(failure.AuthenticationFail, "authentication failed for the configured user", err).
			WithHint("check source.user and the password source (password_env/password_file)")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return failure.New(failure.ConnectionFailed, "cannot connect to PostgreSQL", err).
		WithHint("check host, port, sslmode and that the server is reachable")
}

// ServerVersion returns the server_version string and major number.
func ServerVersion(ctx context.Context, q pgx.Tx) (string, int, error) {
	var v string
	var num int
	if err := q.QueryRow(ctx, `SELECT current_setting('server_version'), current_setting('server_version_num')::int`).Scan(&v, &num); err != nil {
		return "", 0, err
	}
	return v, num / 10000, nil
}

// TableFingerprint describes one user table.
type TableFingerprint struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Rows   int64  `json:"rows"`
	Hash   string `json:"hash,omitempty"`
	Hashed bool   `json:"hashed"`
}

// QueryFingerprint is the result digest of a user-defined assertion query.
type QueryFingerprint struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
	Hash string `json:"hash"`
}

// ObjectSet is a category of schema objects (constraints, indexes, ...).
type ObjectSet struct {
	Count int      `json:"count"`
	Hash  string   `json:"hash"`
	Items []string `json:"items"`
}

// Fingerprint captures the verifiable state of a database.
type Fingerprint struct {
	ServerVersion string                `json:"server_version"`
	ServerMajor   int                   `json:"server_major"`
	CapturedAt    time.Time             `json:"captured_at"`
	Tables        []TableFingerprint    `json:"tables"`
	Objects       map[string]*ObjectSet `json:"objects"`
	Queries       []QueryFingerprint    `json:"queries"`
	HashMaxRows   int64                 `json:"hash_max_rows"`
	// SequenceValues holds last_value per sequence (-1 = never called).
	// Sequences are NOT transactional: their value is read outside snapshot
	// semantics, so verification checks monotonicity (restored >= captured)
	// instead of equality.
	SequenceValues map[string]int64 `json:"sequence_values"`
}

// Digest returns a stable SHA-256 over the whole fingerprint content.
func (f *Fingerprint) Digest() string {
	h := sha256.New()
	for _, t := range f.Tables {
		fmt.Fprintf(h, "t|%s|%s|%d|%s\n", t.Schema, t.Name, t.Rows, t.Hash)
	}
	keys := make([]string, 0, len(f.Objects))
	for k := range f.Objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "o|%s|%d|%s\n", k, f.Objects[k].Count, f.Objects[k].Hash)
	}
	for _, q := range f.Queries {
		fmt.Fprintf(h, "q|%s|%d|%s\n", q.Name, q.Rows, q.Hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// UserQuery is a named read-only assertion.
type UserQuery struct {
	Name string
	SQL  string
}

// sessionSettings make text output deterministic across source and target.
var sessionSettings = []string{
	`SET LOCAL DateStyle = 'ISO, MDY'`,
	`SET LOCAL TimeZone = 'UTC'`,
	`SET LOCAL IntervalStyle = 'postgres'`,
	`SET LOCAL extra_float_digits = 1`,
	`SET LOCAL bytea_output = 'hex'`,
	`SET LOCAL statement_timeout = 0`,
	`SET LOCAL search_path = pg_catalog`,
}

const userNamespaceFilter = `n.nspname NOT IN ('pg_catalog','information_schema','pg_toast')
	AND n.nspname NOT LIKE 'pg_temp_%' AND n.nspname NOT LIKE 'pg_toast_temp_%'`

// objectQueries return one signature string per object. Extension-owned
// objects are excluded because they are created by CREATE EXTENSION in the
// target rather than restored from the archive.
var objectQueries = map[string]string{
	"schemas": `SELECT n.nspname FROM pg_namespace n WHERE ` + userNamespaceFilter + `
		AND n.nspname <> 'public'
		AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_namespace'::regclass AND d.objid=n.oid AND d.deptype='e')`,
	"tables": `SELECT n.nspname||'.'||c.relname||'|'||c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind IN ('r','p','v','m','f') AND ` + userNamespaceFilter + `
		AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_class'::regclass AND d.objid=c.oid AND d.deptype='e')`,
	"columns": `SELECT n.nspname||'.'||c.relname||'.'||a.attname||'|'||format_type(a.atttypid,a.atttypmod)||'|'||a.attnotnull::text
		FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE a.attnum>0 AND NOT a.attisdropped AND c.relkind IN ('r','p','v','m','f') AND ` + userNamespaceFilter + `
		AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_class'::regclass AND d.objid=c.oid AND d.deptype='e')`,
	"constraints": `SELECT n.nspname||'.'||cl.relname||'.'||co.conname||'|'||co.contype::text||'|'||pg_get_constraintdef(co.oid)
		FROM pg_constraint co JOIN pg_class cl ON cl.oid=co.conrelid JOIN pg_namespace n ON n.oid=cl.relnamespace
		WHERE ` + userNamespaceFilter + `
		AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_class'::regclass AND d.objid=cl.oid AND d.deptype='e')`,
	"indexes": `SELECT pg_get_indexdef(i.indexrelid) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
		JOIN pg_namespace n ON n.oid=c.relnamespace WHERE ` + userNamespaceFilter + `
		AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_class'::regclass AND d.objid=i.indrelid AND d.deptype='e')`,
	"sequences": `SELECT schemaname||'.'||sequencename FROM pg_sequences
		WHERE schemaname NOT IN ('pg_catalog','information_schema')`,
	"functions": `SELECT n.nspname||'.'||p.proname||'('||pg_get_function_identity_arguments(p.oid)||')|'||p.prokind::text
		FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE ` + userNamespaceFilter + `
		AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_proc'::regclass AND d.objid=p.oid AND d.deptype='e')`,
	"triggers": `SELECT n.nspname||'.'||c.relname||'.'||t.tgname FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid
		JOIN pg_namespace n ON n.oid=c.relnamespace WHERE NOT t.tgisinternal AND ` + userNamespaceFilter,
	"types": `SELECT n.nspname||'.'||t.typname||'|'||t.typtype::text||'|'||COALESCE((SELECT string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder)
		FROM pg_enum e WHERE e.enumtypid=t.oid),'')
		FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace
		WHERE t.typtype IN ('e','d','c') AND ` + userNamespaceFilter + `
		AND (t.typtype <> 'c' OR (SELECT relkind FROM pg_class WHERE oid=t.typrelid)='c')
		AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_type'::regclass AND d.objid=t.oid AND d.deptype='e')`,
	"extensions":    `SELECT extname FROM pg_extension WHERE extname <> 'plpgsql'`,
	"large_objects": `SELECT count(*)::text FROM pg_largeobject_metadata`,
}

// Capture computes a fingerprint inside tx. The caller controls the
// transaction (repeatable read + read only for sources).
func Capture(ctx context.Context, tx pgx.Tx, hashMaxRows int64, queries []UserQuery) (*Fingerprint, error) {
	for _, s := range sessionSettings {
		if _, err := tx.Exec(ctx, s); err != nil {
			return nil, fmt.Errorf("session setting: %w", err)
		}
	}
	ver, major, err := ServerVersion(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("server version: %w", err)
	}
	fp := &Fingerprint{ServerVersion: ver, ServerMajor: major, CapturedAt: time.Now().UTC(),
		Objects: map[string]*ObjectSet{}, HashMaxRows: hashMaxRows}

	for name, q := range objectQueries {
		items, err := collectStrings(ctx, tx, q)
		if err != nil {
			return nil, fmt.Errorf("fingerprint %s: %w", name, err)
		}
		sort.Strings(items)
		h := sha256.Sum256([]byte(strings.Join(items, "\n")))
		fp.Objects[name] = &ObjectSet{Count: len(items), Hash: hex.EncodeToString(h[:]), Items: items}
	}
	// large_objects is a single count row; expose the count directly.
	if lo := fp.Objects["large_objects"]; lo != nil && len(lo.Items) == 1 {
		if _, err := fmt.Sscanf(lo.Items[0], "%d", &lo.Count); err != nil {
			return nil, fmt.Errorf("large object count: %w", err)
		}
	}

	rows, err := tx.Query(ctx, `SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind='r' AND `+userNamespaceFilter+`
		AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_class'::regclass AND d.objid=c.oid AND d.deptype='e')
		ORDER BY 1,2`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	var tables []TableFingerprint
	for rows.Next() {
		var t TableFingerprint
		if err := rows.Scan(&t.Schema, &t.Name); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range tables {
		t := &tables[i]
		ident := pgx.Identifier{t.Schema, t.Name}.Sanitize()
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ONLY `+ident).Scan(&t.Rows); err != nil {
			return nil, fmt.Errorf("count %s: %w", ident, err)
		}
		if t.Rows <= hashMaxRows {
			if err := tx.QueryRow(ctx, `SELECT md5(COALESCE(string_agg(r.x, E'\n' ORDER BY r.x COLLATE "C"), ''))
				FROM (SELECT t::text AS x FROM ONLY `+ident+` AS t) r`).Scan(&t.Hash); err != nil {
				return nil, fmt.Errorf("hash %s: %w", ident, err)
			}
			t.Hashed = true
		}
	}
	fp.Tables = tables

	fp.SequenceValues = map[string]int64{}
	srows, err := tx.Query(ctx, `SELECT schemaname||'.'||sequencename, COALESCE(last_value, -1) FROM pg_sequences
		WHERE schemaname NOT IN ('pg_catalog','information_schema')`)
	if err != nil {
		return nil, fmt.Errorf("sequence values: %w", err)
	}
	for srows.Next() {
		var name string
		var v int64
		if err := srows.Scan(&name, &v); err != nil {
			srows.Close()
			return nil, err
		}
		fp.SequenceValues[name] = v
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return nil, err
	}

	if len(queries) > 0 {
		// Assertion queries are written by operators against the default schema.
		if _, err := tx.Exec(ctx, `SET LOCAL search_path = public`); err != nil {
			return nil, err
		}
	}
	for _, q := range queries {
		sqlText := strings.TrimRight(strings.TrimSpace(q.SQL), "; \n\t")
		var qf QueryFingerprint
		qf.Name = q.Name
		err := tx.QueryRow(ctx, `SELECT count(*), md5(COALESCE(string_agg(r.x, E'\n' ORDER BY r.x COLLATE "C"), ''))
			FROM (SELECT q::text AS x FROM (`+sqlText+`) AS q) r`).Scan(&qf.Rows, &qf.Hash)
		if err != nil {
			return nil, failure.New(failure.AssertionFailed, fmt.Sprintf("assertion query %q failed", q.Name), err)
		}
		fp.Queries = append(fp.Queries, qf)
	}
	return fp, nil
}

func collectStrings(ctx context.Context, tx pgx.Tx, q string) ([]string, error) {
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Mismatch is one difference between expected and actual fingerprints.
type Mismatch struct {
	Check    string `json:"check"`
	Object   string `json:"object"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// Comparison groups the result of comparing two fingerprints.
type Comparison struct {
	TablesChecked    int            `json:"tables_checked"`
	TablesHashed     int            `json:"tables_hashed"`
	RowsExpected     int64          `json:"rows_expected"`
	RowsActual       int64          `json:"rows_actual"`
	ObjectsChecked   map[string]int `json:"objects_checked"`
	QueriesChecked   int            `json:"queries_checked"`
	SequencesChecked int            `json:"sequences_checked"`
	DataMismatches   []Mismatch     `json:"data_mismatches"`
	SchemaMismatches []Mismatch     `json:"schema_mismatches"`
	QueryMismatches  []Mismatch     `json:"query_mismatches"`
}

// OK reports whether no mismatches were found.
func (c *Comparison) OK() bool {
	return len(c.DataMismatches) == 0 && len(c.SchemaMismatches) == 0 && len(c.QueryMismatches) == 0
}

// maxListedDiffs caps per-category mismatch listings in reports.
const maxListedDiffs = 50

// Compare checks actual against expected. Every expected table, object and
// query must match exactly; extra objects in actual are also mismatches.
func Compare(expected, actual *Fingerprint) *Comparison {
	c := &Comparison{ObjectsChecked: map[string]int{}}
	act := map[string]TableFingerprint{}
	for _, t := range actual.Tables {
		act[t.Schema+"."+t.Name] = t
		c.RowsActual += t.Rows
	}
	for _, e := range expected.Tables {
		key := e.Schema + "." + e.Name
		c.TablesChecked++
		c.RowsExpected += e.Rows
		a, ok := act[key]
		if !ok {
			c.DataMismatches = append(c.DataMismatches, Mismatch{"table_present", key, "present", "missing"})
			continue
		}
		delete(act, key)
		if a.Rows != e.Rows {
			c.DataMismatches = append(c.DataMismatches, Mismatch{"row_count", key, fmt.Sprint(e.Rows), fmt.Sprint(a.Rows)})
		}
		if e.Hashed {
			c.TablesHashed++
			if a.Hash != e.Hash {
				c.DataMismatches = append(c.DataMismatches, Mismatch{"content_hash", key, e.Hash, a.Hash})
			}
		}
	}
	for key := range act {
		c.DataMismatches = append(c.DataMismatches, Mismatch{"table_present", key, "absent", "unexpected table"})
	}

	keys := make([]string, 0, len(expected.Objects))
	for k := range expected.Objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := expected.Objects[k]
		a := actual.Objects[k]
		c.ObjectsChecked[k] = e.Count
		if a == nil {
			c.SchemaMismatches = append(c.SchemaMismatches, Mismatch{k, "*", fmt.Sprint(e.Count), "not captured"})
			continue
		}
		if a.Hash == e.Hash {
			continue
		}
		missing, extra := diff(e.Items, a.Items)
		listed := 0
		for _, m := range missing {
			if listed >= maxListedDiffs {
				break
			}
			c.SchemaMismatches = append(c.SchemaMismatches, Mismatch{k, m, "present", "missing"})
			listed++
		}
		for _, x := range extra {
			if listed >= maxListedDiffs {
				break
			}
			c.SchemaMismatches = append(c.SchemaMismatches, Mismatch{k, x, "absent", "unexpected"})
			listed++
		}
		if listed == 0 { // same items, different hash should be impossible; record anyway
			c.SchemaMismatches = append(c.SchemaMismatches, Mismatch{k, "*", e.Hash, a.Hash})
		}
	}

	seqNames := make([]string, 0, len(expected.SequenceValues))
	for k := range expected.SequenceValues {
		seqNames = append(seqNames, k)
	}
	sort.Strings(seqNames)
	for _, name := range seqNames {
		want := expected.SequenceValues[name]
		got, ok := actual.SequenceValues[name]
		c.SequencesChecked++
		if !ok {
			continue // reported by the "sequences" object set
		}
		if got < want {
			c.DataMismatches = append(c.DataMismatches, Mismatch{"sequence_value", name,
				fmt.Sprintf(">= %d", want), fmt.Sprint(got)})
		}
	}

	aq := map[string]QueryFingerprint{}
	for _, q := range actual.Queries {
		aq[q.Name] = q
	}
	for _, e := range expected.Queries {
		c.QueriesChecked++
		a, ok := aq[e.Name]
		switch {
		case !ok:
			c.QueryMismatches = append(c.QueryMismatches, Mismatch{"query", e.Name, "result", "not executed"})
		case a.Rows != e.Rows || a.Hash != e.Hash:
			c.QueryMismatches = append(c.QueryMismatches, Mismatch{"query", e.Name,
				fmt.Sprintf("%d rows/%s", e.Rows, e.Hash), fmt.Sprintf("%d rows/%s", a.Rows, a.Hash)})
		}
	}
	return c
}

func diff(expected, actual []string) (missing, extra []string) {
	am := make(map[string]int, len(actual))
	for _, a := range actual {
		am[a]++
	}
	for _, e := range expected {
		if am[e] > 0 {
			am[e]--
		} else {
			missing = append(missing, e)
		}
	}
	for _, a := range actual {
		if am[a] > 0 {
			extra = append(extra, a)
			am[a]--
		}
	}
	return missing, extra
}
