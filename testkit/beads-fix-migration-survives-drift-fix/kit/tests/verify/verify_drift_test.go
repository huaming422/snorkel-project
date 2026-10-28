package schema

// Behavioral checks for the aux-table row-id re-key's handling of
// schema-encoding drift, where a text column's on-disk encoding tag no longer
// matches its rows and reading an affected cell fails with an "invalid hash
// length" error from the storage engine.
//
// These drive the pre-existing, signature-stable per-pass entry point
// (rekeyAuxRowIDs, the same one the package's own crash-resume tests use)
// against a hand-rolled DBConn backed by an in-memory database/sql driver that
// answers each statement by its SQL shape rather than a fixed call order.
// local_metadata can be read as a COUNT probe or as a SELECT of any of its
// columns, and this backing answers every such read from the same in-memory
// key/value store, returning exactly the columns the query selects.
//
// They reference only package state that predates this work (rekeyAuxRowIDs,
// auxRekeyPassInitial and auxRekeyTables), never a helper or record key that a
// given implementation of the skip might introduce. Where the drift check
// lives, how the skip is recorded and how logging is routed are left open.
//
// The backing also persists and round-trips every record write it does not
// model specifically, keyed by the record's own key column, from one store per
// record table. That lets a two-pass check observe that a table skipped for
// drift on one pass is re-keyed by a later pass once the drift clears, without
// pinning where or under what key the skip was recorded: a comma-list under one
// local_metadata key, a per-table key, a separate dolt-ignored table, or a kept
// in-progress sentinel all round-trip the same way.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// In-memory DBConn backing
// ---------------------------------------------------------------------------

// vdHarness holds the simulated database state the re-key reads and writes. One
// instance backs one *sql.DB; a check tunes it, runs the re-key, and reads back
// the effects.
type vdHarness struct {
	// cursorExists is whether the ignored-migrations cursor table is present.
	cursorExists bool
	// currentVersion is the highest recorded ignored-migration version.
	currentVersion int
	// sentinelTablesPresent / sentinelColumnPresent answer the cursor-reality
	// probes so the cursor is believed as read (no clamp).
	sentinelTablesPresent bool
	sentinelColumnPresent bool
	// localMetaExists is whether the clone-local local_metadata table is present.
	localMetaExists bool
	// records is the fake's durable key/value storage, one map per record table:
	// local_metadata (which also holds the in-progress sentinel), plus any other
	// dolt-ignored table an implementation creates for its own records. A write
	// to a record table persists here and a read returns it, keyed by the
	// record's own key/identifier column, so a two-pass check can observe that a
	// skip was recorded and later honored without pinning which table or key
	// column holds it. Lazily created by h.store.
	records map[string]map[string]string
	// idColumns reports, per aux table, whether it still has its id column (so
	// the re-key proceeds to scan it).
	idColumns map[string]bool
	// scanErr injects the error a table's content SELECT returns, which is where
	// the storage-decode failure surfaces.
	scanErr map[string]error

	// selected / updated record which tables the re-key scanned and rewrote, so
	// a check can assert the effect without inspecting any private record.
	selected map[string]int
	updated  map[string]int
}

func vdNewHarness() *vdHarness {
	return &vdHarness{
		records:   map[string]map[string]string{},
		idColumns: map[string]bool{},
		scanErr:   map[string]error{},
		selected:  map[string]int{},
		updated:   map[string]int{},
	}
}

// store returns the key/value map backing one record table, creating it on
// first write. Reading an absent table returns a fresh empty map (the truthful
// answer for a never-written record).
func (h *vdHarness) store(table string) map[string]string {
	s := h.records[table]
	if s == nil {
		s = map[string]string{}
		h.records[table] = s
	}
	return s
}

// auxDataTables are the four tables the re-key rewrites. They are NOT record
// tables: a write to one is an id rewrite (counted in updated), and a read of
// one is a content scan (counted in selected), never a durable skip record.
var auxDataTables = map[string]bool{
	"events": true, "comments": true,
	"issue_snapshots": true, "compaction_snapshots": true,
}

// vdOpen wraps the harness in a *sql.DB whose every statement is answered from
// memory. A single connection keeps the shared state strictly serialized.
func vdOpen(h *vdHarness) *sql.DB {
	db := sql.OpenDB(vdConnector{h: h})
	db.SetMaxOpenConns(1)
	return db
}

type vdConnector struct{ h *vdHarness }

func (c vdConnector) Connect(context.Context) (driver.Conn, error) { return &vdConn{h: c.h}, nil }
func (c vdConnector) Driver() driver.Driver                        { return vdDriver{} }

type vdDriver struct{}

func (vdDriver) Open(string) (driver.Conn, error) { return nil, errors.New("vd: open via OpenDB") }

type vdConn struct{ h *vdHarness }

func (c *vdConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("vd: prepare unsupported") }
func (c *vdConn) Close() error                        { return nil }
func (c *vdConn) Begin() (driver.Tx, error)           { return nil, errors.New("vd: begin unsupported") }

func (c *vdConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.h.query(q, args)
}

func (c *vdConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return c.h.exec(q, args)
}

type vdRows struct {
	cols []string
	rows [][]driver.Value
	pos  int
}

func (r *vdRows) Columns() []string { return r.cols }
func (r *vdRows) Close() error      { return nil }
func (r *vdRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}

type vdResult struct{}

func (vdResult) LastInsertId() (int64, error) { return 0, nil }
func (vdResult) RowsAffected() (int64, error) { return 0, nil }

var vdWS = regexp.MustCompile(`\s+`)

func vdNorm(s string) string {
	return strings.ToLower(vdWS.ReplaceAllString(strings.TrimSpace(s), " "))
}

func vdArgStr(args []driver.NamedValue, i int) string {
	if i < 0 || i >= len(args) {
		return ""
	}
	s, _ := args[i].Value.(string)
	return s
}

func vdBool(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func vdScalar(v driver.Value) (driver.Rows, error) {
	return &vdRows{cols: []string{"n"}, rows: [][]driver.Value{{v}}}, nil
}

var vdFromRe = regexp.MustCompile(`(?i)\bfrom\s+` + "`?" + `(\w+)` + "`?")

func vdTable(q string) string {
	if m := vdFromRe.FindStringSubmatch(q); len(m) > 1 {
		return m[1]
	}
	return ""
}

var vdUpdateRe = regexp.MustCompile(`(?i)^update\s+` + "`?" + `(\w+)` + "`?")

func vdUpdateTable(q string) string {
	if m := vdUpdateRe.FindStringSubmatch(strings.TrimSpace(q)); len(m) > 1 {
		return m[1]
	}
	return ""
}

// vdWriteRe extracts the target table of a record write, across every shape a
// skip record is written through: REPLACE/INSERT [IGNORE] INTO <t>, DELETE FROM
// <t>, UPDATE <t>, CREATE TABLE [IF NOT EXISTS] <t>. Extracting it uniformly is
// what keeps a write's store and the later read's store the same, whatever
// clone-local record table a fix chose.
var vdWriteRe = regexp.MustCompile(`(?i)^(?:replace\s+into|insert(?:\s+ignore)?\s+into|delete\s+from|update|create\s+table(?:\s+if\s+not\s+exists)?)\s+` + "`?" + `(\w+)` + "`?")

func vdWriteTable(q string) string {
	if m := vdWriteRe.FindStringSubmatch(strings.TrimSpace(q)); len(m) > 1 {
		return m[1]
	}
	return ""
}

func (h *vdHarness) query(raw string, args []driver.NamedValue) (driver.Rows, error) {
	q := vdNorm(raw)
	switch {
	// Existence of the local_metadata table (its name is a literal in the SQL).
	case strings.Contains(q, "from information_schema.tables") && strings.Contains(q, "'local_metadata'"):
		return vdScalar(vdBool(h.localMetaExists))

	// Existence of the migration cursor and the cursor-reality sentinel tables
	// (the table name is bound as a parameter).
	case strings.Contains(q, "from information_schema.tables") && strings.Contains(q, "table_name = ?"):
		name := vdArgStr(args, 0)
		if name == "ignored_schema_migrations" || name == "schema_migrations" {
			return vdScalar(vdBool(h.cursorExists))
		}
		return vdScalar(vdBool(h.sentinelTablesPresent))

	// Highest recorded migration version.
	case strings.Contains(q, "coalesce(max(version)"):
		return vdScalar(int64(h.currentVersion))

	// Column presence: the id column of an aux table, or a cursor-reality
	// sentinel column.
	case strings.Contains(q, "from information_schema.columns"):
		col := vdArgStr(args, 1)
		if col == "id" {
			return vdScalar(vdBool(h.idColumns[vdArgStr(args, 0)]))
		}
		return vdScalar(vdBool(h.sentinelColumnPresent))

	// Reading a clone-local record: a COUNT probe or a SELECT of any of the
	// record's columns, matched by `=`, `IN (...)`, or `LIKE`, over
	// local_metadata or any other dolt-ignored record table. The same store
	// answers all of them, and each row carries exactly the columns the query
	// selects (`key`, value, both, or *), so a one-column read scans into one
	// destination just as it would against the real table.
	case h.isRecordRead(q):
		store := h.store(vdTable(raw))
		matched := matchRecordKeys(store, q, args)
		if strings.Contains(q, "count(") {
			return vdScalar(int64(len(matched)))
		}
		cols := vdRecordCols(q)
		rows := make([][]driver.Value, 0, len(matched))
		for _, k := range matched {
			row := make([]driver.Value, len(cols))
			for i, c := range cols {
				switch c {
				case "key":
					row[i] = k
				case "value":
					row[i] = store[k]
				default:
					// A constant such as SELECT 1, used as an existence probe.
					row[i] = int64(1)
				}
			}
			rows = append(rows, row)
		}
		return &vdRows{cols: cols, rows: rows}, nil

	// Scanning an aux table's contents to re-key it. The storage-decode failure
	// surfaces here.
	case strings.HasPrefix(q, "select id,") && strings.Contains(q, " from "):
		table := vdTable(raw)
		h.selected[table]++
		if err := h.scanErr[table]; err != nil {
			return nil, err
		}
		return &vdRows{cols: vdSelectCols(raw), rows: nil}, nil
	}

	// Any read this fake does not model specifically answers "nothing there" (an
	// empty result, or 0 for a COUNT), which is the true answer for the fresh,
	// never-skipped database these checks set up. That covers a record kept in
	// some other structure, such as a different local_metadata key, a distinct
	// dolt-ignored table or a new column.
	if strings.Contains(q, "count(") {
		return vdScalar(int64(0))
	}
	return &vdRows{cols: []string{"c0"}, rows: nil}, nil
}

// vdRecordCols returns the column list a record read selects, as "key",
// "value", or the raw expression for anything else (a constant probe). A query
// whose select list cannot be found reads as `key`, value.
func vdRecordCols(q string) []string {
	lo := strings.Index(q, "select ")
	hi := strings.Index(q, " from ")
	if lo < 0 || hi < 0 || hi <= lo {
		return []string{"key", "value"}
	}
	var cols []string
	for _, part := range strings.Split(q[lo+len("select "):hi], ",") {
		p := strings.TrimSpace(strings.ReplaceAll(part, "`", ""))
		p = strings.TrimPrefix(p, "distinct ")
		switch {
		case p == "*":
			cols = append(cols, "key", "value")
		case strings.Contains(p, "value"):
			cols = append(cols, "value")
		case strings.Contains(p, "key"):
			cols = append(cols, "key")
		default:
			cols = append(cols, p)
		}
	}
	return cols
}

func vdSelectCols(raw string) []string {
	lq := strings.ToLower(raw)
	lo := strings.Index(lq, "select ")
	hi := strings.Index(lq, " from ")
	if lo < 0 || hi < 0 || hi <= lo {
		return []string{"c0"}
	}
	parts := strings.Split(raw[lo+len("select "):hi], ",")
	cols := make([]string, len(parts))
	for i := range parts {
		cols[i] = fmt.Sprintf("c%d", i)
	}
	return cols
}

func (h *vdHarness) exec(raw string, args []driver.NamedValue) (driver.Result, error) {
	q := vdNorm(raw)
	switch {
	case strings.HasPrefix(q, "create table if not exists local_metadata"):
		h.localMetaExists = true
		return vdResult{}, nil
	case strings.HasPrefix(q, "create table"):
		// Any other dolt-ignored record table is accepted, so the row writes that
		// follow it persist.
		return vdResult{}, nil

	// An id rewrite of one of the four aux data tables, the observable effect a
	// check counts. Checked before the generic record writes so an UPDATE of a
	// data table is never mistaken for a record write.
	case strings.HasPrefix(q, "update ") && strings.Contains(q, "set id = ?"):
		h.updated[vdUpdateTable(raw)]++
		return vdResult{}, nil

	// Any write to a record table (local_metadata, a per-table keyed variant, a
	// kept in-flight sentinel, or a separate dolt-ignored record table) persists
	// in that table's store, keyed by the key/identifier bound as the first
	// argument. REPLACE/INSERT upserts and DELETE removes, so a record stays in
	// place across the two passes a resume check runs.
	case h.isRecordWrite(q, raw):
		store := h.store(vdWriteTable(raw))
		key := vdArgStr(args, 0)
		switch {
		case strings.HasPrefix(q, "delete "):
			for _, k := range matchRecordKeys(store, q, args) {
				delete(store, k)
			}
		default:
			v := "1"
			if len(args) > 1 {
				if s, ok := args[1].Value.(string); ok {
					v = s
				}
			}
			store[key] = v
		}
		return vdResult{}, nil
	}

	// A write this fake does not model succeeds silently, like the query path's
	// default. No modeled effect of these checks depends on an unmodeled write.
	return vdResult{}, nil
}

// isRecordRead reports whether a normalized query reads a clone-local record
// store rather than an aux data table's content. A record read names
// local_metadata, selects/counts a `key` column, or reads any non-data table
// that is not one of the modeled information_schema / cursor probes.
func (h *vdHarness) isRecordRead(q string) bool {
	if strings.Contains(q, "information_schema") || strings.Contains(q, "coalesce(max(version)") {
		return false
	}
	if strings.HasPrefix(q, "select id,") {
		return false // an aux-table content scan
	}
	if strings.Contains(q, "from local_metadata") || strings.Contains(q, "`key`") {
		return true
	}
	// A read of some other dolt-ignored record table: any SELECT whose FROM is
	// not one of the four aux data tables.
	if strings.HasPrefix(q, "select ") {
		return !auxDataTables[vdTable(q)]
	}
	return false
}

// isRecordWrite reports whether a normalized write targets a record store (not
// an aux data table's id rewrite, which the caller handles first).
func (h *vdHarness) isRecordWrite(q, raw string) bool {
	switch {
	case strings.Contains(q, "local_metadata"):
		return true
	case strings.HasPrefix(q, "replace into "), strings.HasPrefix(q, "insert into "),
		strings.HasPrefix(q, "insert ignore into "):
		return !auxDataTables[vdTable(raw)]
	case strings.HasPrefix(q, "delete from "):
		return !auxDataTables[vdTable(raw)]
	case strings.HasPrefix(q, "update "):
		// An UPDATE that is not an id rewrite (handled earlier) and targets a
		// non-data table is a record update (e.g. upsert of a skip flag).
		return !auxDataTables[vdUpdateTable(raw)]
	}
	return false
}

// matchRecordKeys returns the keys present in store that a WHERE clause selects.
// It supports the three shapes the skip record is read back through: `key` = ?,
// `key` IN (?, ?, ...), and `key` LIKE ? (with a trailing %). A read with no
// bound key arguments (a bare COUNT(*) or full-table SELECT) returns every key.
func matchRecordKeys(store map[string]string, q string, args []driver.NamedValue) []string {
	var argStrs []string
	for _, a := range args {
		if s, ok := a.Value.(string); ok {
			argStrs = append(argStrs, s)
		}
	}
	var out []string
	switch {
	case strings.Contains(q, " like "):
		for _, a := range argStrs {
			prefix := strings.TrimRight(a, "%")
			for k := range store {
				if strings.HasPrefix(k, prefix) {
					out = append(out, k)
				}
			}
		}
	case len(argStrs) > 0:
		for _, a := range argStrs {
			if _, ok := store[a]; ok {
				out = append(out, a)
			}
		}
	default:
		for k := range store {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return dedupeStrings(out)
}

func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Shared setup
// ---------------------------------------------------------------------------

// vdFreshPass returns a *sql.DB positioned so pass-initial's re-key runs over
// all four aux tables for the first time: the convergence marker is still
// pending, the clone has no recorded in-flight or skipped work, and only the
// events table still carries an id column, so it is the one table scanned. Its
// content scan returns scanErr. The other three report no id column and are
// left alone, so a check observes exactly what the re-key does with the one
// unreadable table.
func vdFreshPass(scanErr error) *sql.DB {
	h := vdNewHarness()
	h.cursorExists = true
	h.currentVersion = auxRekeyPassInitial.markerVersion - 1
	h.sentinelTablesPresent = true
	h.sentinelColumnPresent = true
	h.localMetaExists = true
	h.idColumns[auxRekeyTables[0].name] = true // events
	h.scanErr[auxRekeyTables[0].name] = scanErr
	return vdOpen(h)
}

// vdRunInitial drives the first-pass re-key: mainVersionBefore is one short of
// the shipped watershed so the pass is admitted.
func vdRunInitial(db *sql.DB) error {
	_, err := rekeyAuxRowIDs(context.Background(), db,
		auxRekeyPassInitial.shippedMainVersion-1, auxRekeyPassInitial)
	return err
}

// The storage-decode failure Dolt recovers per query and surfaces as a 1105.
var vdDriftErr = errors.New("Error 1105 (HY000): panic recovered: invalid hash length: 19")

// ---------------------------------------------------------------------------
// Checks
// ---------------------------------------------------------------------------

// TestVerifyDriftSurvivesMigration: a table whose storage cannot be decoded
// does not abort the re-key. The pass completes over the rest of the database
// and returns no error.
func TestVerifyDriftSurvivesMigration(t *testing.T) {
	db := vdFreshPass(vdDriftErr)
	defer db.Close()

	if err := vdRunInitial(db); err != nil {
		t.Fatalf("a storage-decode failure on one table must not abort the re-key, got: %v", err)
	}
}

// TestVerifyNonDriftAborts: an error that is not the storage-decode signature
// still aborts the pass and is returned to the caller.
func TestVerifyNonDriftAborts(t *testing.T) {
	db := vdFreshPass(errors.New("Error 1062 (23000): duplicate entry"))
	defer db.Close()

	if err := vdRunInitial(db); err == nil {
		t.Fatal("an ordinary (non storage-decode) failure must abort the re-key, got nil")
	}
}

// TestVerifyDriftToleranceBreadth covers the forms of the storage-decode
// signature: the reported width varies with the direction of the encoding flip
// and the engine's recovery wrapper is not always present, so each of those
// forms is tolerated, while unrelated recovered panics and ordinary failures
// still abort.
func TestVerifyDriftToleranceBreadth(t *testing.T) {
	cases := []struct {
		name        string
		errText     string
		wantSurvive bool
	}{
		{"recovered width 19", "Error 1105 (HY000): panic recovered: invalid hash length: 19", true},
		{"recovered width 1", "Error 1105 (HY000): panic recovered: invalid hash length: 1", true},
		{"unwrapped panic text", "panic: invalid hash length: 19", true},
		{"unrelated recovered panic", "Error 1105 (HY000): panic recovered: index out of range", false},
		{"ordinary sql error", "Error 1062 (23000): duplicate entry", false},
		{"lock wait timeout", "Error 1205 (HY000): lock wait timeout exceeded", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := vdFreshPass(errors.New(c.errText))
			defer db.Close()

			err := vdRunInitial(db)
			if c.wantSurvive && err != nil {
				t.Fatalf("%q carries the storage-decode signature and must be tolerated, got: %v", c.errText, err)
			}
			if !c.wantSurvive && err == nil {
				t.Fatalf("%q is not the storage-decode signature and must abort the pass, got nil", c.errText)
			}
		})
	}
}

// vdDriftSets are the table sets made unreadable on the first pass of the
// two-pass checks: one table on its own, and two tables at once.
var vdDriftSets = []struct {
	name    string
	drifted []string
}{
	{"comments", []string{"comments"}},
	{"events_and_compaction_snapshots", []string{"events", "compaction_snapshots"}},
}

// vdTwoPass runs pass-initial's re-key twice on one clone whose four aux
// tables all still carry an id column, and returns the harness with the second
// pass's scan counts.
//
// Pass 1 is the marker-pending first run, and the tables in drifted fail their
// content scan with the storage-decode signature. Between the passes the
// lineage records the convergence marker and the storage is repaired, so every
// table reads cleanly. Pass 2 then runs post-marker, at the shipped watershed.
func vdTwoPass(t *testing.T, drifted []string) *vdHarness {
	t.Helper()
	h := vdNewHarness()
	h.cursorExists = true
	h.currentVersion = auxRekeyPassInitial.markerVersion - 1
	h.sentinelTablesPresent = true
	h.sentinelColumnPresent = true
	h.localMetaExists = true
	for _, tbl := range auxRekeyTables {
		h.idColumns[tbl.name] = true
	}
	for _, name := range drifted {
		h.scanErr[name] = vdDriftErr
	}
	db := vdOpen(h)
	t.Cleanup(func() { db.Close() })

	if _, err := rekeyAuxRowIDs(context.Background(), db,
		auxRekeyPassInitial.shippedMainVersion-1, auxRekeyPassInitial); err != nil {
		t.Fatalf("pass 1: a storage-decode failure on %v must not abort the re-key, got: %v", drifted, err)
	}
	for _, name := range drifted {
		if h.selected[name] == 0 {
			t.Fatalf("pass 1: %s was never scanned, so its drift was never hit", name)
		}
	}

	h.currentVersion = auxRekeyPassInitial.markerVersion
	h.scanErr = map[string]error{}
	h.selected = map[string]int{}
	h.updated = map[string]int{}

	if _, err := rekeyAuxRowIDs(context.Background(), db,
		auxRekeyPassInitial.shippedMainVersion, auxRekeyPassInitial); err != nil {
		t.Fatalf("pass 2: the re-key after the storage repair must not error, got: %v", err)
	}
	return h
}

// TestVerifyDriftSkipResumes: a table skipped on the first pass because its
// storage could not be decoded is scanned again by the post-marker second pass
// once the storage reads cleanly. The check reads only the scan counts, not
// where or how the skip was recorded.
func TestVerifyDriftSkipResumes(t *testing.T) {
	for _, set := range vdDriftSets {
		t.Run(set.name, func(t *testing.T) {
			h := vdTwoPass(t, set.drifted)
			for _, name := range set.drifted {
				if h.selected[name] == 0 {
					t.Errorf("pass 2: %s, skipped on pass 1, was not re-keyed after the convergence marker was recorded", name)
				}
			}
		})
	}
}

// TestVerifyDriftResumeOnlySkippedTables: the post-marker second pass re-keys
// the tables skipped on the first pass and scans none of the tables that
// already converged there.
func TestVerifyDriftResumeOnlySkippedTables(t *testing.T) {
	for _, set := range vdDriftSets {
		t.Run(set.name, func(t *testing.T) {
			h := vdTwoPass(t, set.drifted)
			skipped := map[string]bool{}
			resumed := 0
			for _, name := range set.drifted {
				skipped[name] = true
				resumed += h.selected[name]
			}
			if resumed == 0 {
				t.Fatalf("pass 2: none of the skipped tables %v was re-keyed, so the resumed scope cannot be checked", set.drifted)
			}
			for _, tbl := range auxRekeyTables {
				if !skipped[tbl.name] && h.selected[tbl.name] > 0 {
					t.Errorf("pass 2: %s converged on pass 1 but was scanned again on the post-marker pass", tbl.name)
				}
			}
		})
	}
}

// TestVerifyPackageSuiteGreen guards against a partial change that compiles this
// one check but leaves the migration package's own suite broken. It builds and
// runs that suite in a child process, skipping checks named here (to avoid
// re-entering this one) and the tests that shell out to an external Dolt binary
// the environment does not provide.
func TestVerifyPackageSuiteGreen(t *testing.T) {
	if os.Getenv("VD_SUITE_INNER") == "1" {
		t.Skip("inner suite run")
	}
	root := vdModuleRoot(t)

	// First the whole module compiles: a change that renames or removes a symbol
	// but leaves a caller in another package behind fails here, not at runtime.
	build := exec.Command("go", "build", "./...")
	build.Dir = root
	build.Env = append(os.Environ(), "VD_SUITE_INNER=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the module does not build after the change:\n%s", out)
	}

	// Then the migration package's own suite runs. The tests that drive a real
	// Dolt CLI self-skip when no dolt binary is on PATH
	// (internal/testutil.RequireDoltBinary), so this stays green without one.
	// -skip avoids re-entering the TestVerify* checks in the child run.
	cmd := exec.Command("go", "test", "-count=1", "-skip", "^TestVerify", "./internal/storage/schema/")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "VD_SUITE_INNER=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("migration package build/suite is not green after the change:\n%s", out)
	}
}

func vdModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found upward from %s", dir)
		}
		dir = parent
	}
}
