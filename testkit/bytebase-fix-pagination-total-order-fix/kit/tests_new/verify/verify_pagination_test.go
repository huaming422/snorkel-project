// External-package (store_test) checks for paged reads of the store's list
// functions. Each test drives only the public store API (store.New, the
// List*/Create*/Update* methods, the exported Find*/Message/OrderByKey types)
// against a real PostgreSQL, seeds rows through raw SQL, and reads them back
// one LIMIT/OFFSET page at a time the way the API layer does.
//
// Each test creates its own freshly named database on the PostgreSQL server
// that tests/test-setup.sh starts (connection parameters arrive via the
// PG*/BYTEBASE_TEST_PG_* env vars) and runs the store schema migration into it,
// so tests stay isolated on the shared server.

package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	storepb "github.com/bytebase/bytebase/backend/generated-go/store"
	"github.com/bytebase/bytebase/backend/migrator"
	"github.com/bytebase/bytebase/backend/store"

	// Registers the pgx stdlib driver used by store.New's connection manager.
	_ "github.com/bytebase/bytebase/backend/plugin/db/pg"
)

// ---------------------------------------------------------------------------
// Local PostgreSQL bootstrap.
// ---------------------------------------------------------------------------

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// pgParams reads the server's connection parameters. tests/test-setup.sh
// exports both the libpq PG* names and the BYTEBASE_TEST_PG_* names; prefer the
// latter, fall back to the former, then to the documented local defaults.
func pgParams() (host, port, user, password string) {
	host = envOr("BYTEBASE_TEST_PG_HOST", envOr("PGHOST", "localhost"))
	port = envOr("BYTEBASE_TEST_PG_PORT", envOr("PGPORT", "5432"))
	user = envOr("BYTEBASE_TEST_PG_USER", envOr("PGUSER", "postgres"))
	password = envOr("BYTEBASE_TEST_PG_PASSWORD", envOr("PGPASSWORD", "root-password"))
	return host, port, user, password
}

func dsn(host, port, user, password, database string) string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s database=%s sslmode=disable",
		host, port, user, password, database)
}

// freshStore creates a uniquely-named empty database on the server, migrates
// the store schema into it, and returns a store.Store bound to it plus a
// *sql.DB on the same database for raw seeding. Everything is torn down via
// t.Cleanup. The database name is derived from the test name so a leaked
// database (should a cold-start kill a cleanup) names the test that owns it.
func freshStore(ctx context.Context, t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()

	host, port, user, password := pgParams()

	// Create the per-test database from an admin connection on the default
	// "postgres" database. A sanitized, test-scoped name avoids collisions when
	// tests run in the same package binary.
	admin, err := sql.Open("pgx", dsn(host, port, user, password, envOr("PGDATABASE", "postgres")))
	require.NoError(t, err)
	require.NoError(t, admin.PingContext(ctx))

	dbName := sanitizeDBName(t.Name())
	// Drop any stale database of the same name first, then recreate. Identifiers
	// cannot be bound as parameters, so they are quoted; dbName is restricted to
	// [a-z0-9_] by sanitizeDBName, so the quoting is safe.
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q`, dbName)); err != nil {
		require.NoError(t, admin.Close())
		require.NoError(t, err)
	}
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %q`, dbName))
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	t.Cleanup(func() {
		cleanupAdmin, err := sql.Open("pgx", dsn(host, port, user, password, envOr("PGDATABASE", "postgres")))
		if err != nil {
			return
		}
		defer cleanupAdmin.Close()
		// Terminate lingering backends so DROP DATABASE is not blocked.
		_, _ = cleanupAdmin.ExecContext(ctx,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
			dbName)
		_, _ = cleanupAdmin.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q`, dbName))
	})

	// Raw handle for schema migration and seeding.
	seedDB, err := sql.Open("pgx", dsn(host, port, user, password, dbName))
	require.NoError(t, err)
	require.NoError(t, seedDB.PingContext(ctx))
	t.Cleanup(func() { _ = seedDB.Close() })

	require.NoError(t, migrator.MigrateSchema(ctx, seedDB))

	stores, err := store.New(ctx, dsn(host, port, user, password, dbName), false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stores.Close()) })

	return stores, seedDB
}

// sanitizeDBName turns a Go test name into a valid, lowercase PostgreSQL
// identifier. Test names use '/' and CamelCase; both are normalized to '_'
// segments so the result matches [a-z0-9_]+.
func sanitizeDBName(name string) string {
	out := make([]rune, 0, len(name)+8)
	out = append(out, []rune("sb_")...)
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// ---------------------------------------------------------------------------
// Shared raw-SQL seeding helpers. These write only columns the schema already
// has, so every implementation is seeded the same way.
// ---------------------------------------------------------------------------

func seedWorkspace(ctx context.Context, t *testing.T, db *sql.DB, workspace string) {
	t.Helper()
	_, err := db.ExecContext(ctx, `INSERT INTO workspace (resource_id) VALUES ($1)`, workspace)
	require.NoError(t, err)
}

func seedProject(ctx context.Context, t *testing.T, db *sql.DB, project, workspace string) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT INTO project (resource_id, workspace, name) VALUES ($1, $2, $3)`,
		project, workspace, project)
	require.NoError(t, err)
}

func seedInstance(ctx context.Context, t *testing.T, db *sql.DB, instance, workspace string) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT INTO instance (resource_id, workspace, environment) VALUES ($1, $2, 'prod')`,
		instance, workspace)
	require.NoError(t, err)
}

func seedDatabase(ctx context.Context, t *testing.T, db *sql.DB, instance, name, project string) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT INTO db (instance, name, project, environment) VALUES ($1, $2, $3, 'prod')`,
		instance, name, project)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Pagination walk helpers: read a list page by page with a fresh LIMIT/OFFSET
// per page (the way the server's API layer does).
// ---------------------------------------------------------------------------

// walkPages returns how many times each row key was observed across the walk.
func walkPages(t *testing.T, total, pageSize int, page func(limit, offset int) []string) map[string]int {
	t.Helper()
	seen := map[string]int{}
	// Walk one page past the end so the final partial page is included.
	for offset := 0; offset < total+pageSize; offset += pageSize {
		for _, key := range page(pageSize, offset) {
			seen[key]++
		}
	}
	return seen
}

// walkInOrder returns the row keys in the order the pages delivered them.
func walkInOrder(total, pageSize int, page func(limit, offset int) []string) []string {
	var keys []string
	for offset := 0; offset < total+pageSize; offset += pageSize {
		keys = append(keys, page(pageSize, offset)...)
	}
	return keys
}

// walkIssues pages through ListIssues with the given filter and ordering and
// returns the issues in delivery order.
func walkIssues(ctx context.Context, t *testing.T, stores *store.Store, find store.FindIssueMessage, total, pageSize int) []*store.IssueMessage {
	t.Helper()
	var out []*store.IssueMessage
	for offset := 0; offset < total+pageSize; offset += pageSize {
		f := find
		l, o := pageSize, offset
		f.Limit, f.Offset = &l, &o
		page, err := stores.ListIssues(ctx, &f)
		require.NoErrorf(t, err, "issues page at offset %d", offset)
		out = append(out, page...)
	}
	return out
}

// sortKeys turns an API order_by string into store sort keys through the
// store's own parser for that list, the way the API layer does.
func sortKeys(t *testing.T, parse func(string) ([]*store.OrderByKey, error), orderBy string) []*store.OrderByKey {
	t.Helper()
	keys, err := parse(orderBy)
	require.NoErrorf(t, err, "order_by %q", orderBy)
	require.NotEmptyf(t, keys, "order_by %q", orderBy)
	return keys
}

func issueKeyCounts(issues []*store.IssueMessage) map[string]int {
	seen := map[string]int{}
	for _, issue := range issues {
		seen[fmt.Sprintf("%s/%d", issue.ProjectID, issue.UID)]++
	}
	return seen
}

func assertCompleteAndUnique(t *testing.T, seen map[string]int, total int, label string) {
	t.Helper()
	var duplicated []string
	for key, count := range seen {
		if count > 1 {
			duplicated = append(duplicated, fmt.Sprintf("%s seen %d times", key, count))
		}
	}
	require.Emptyf(t, duplicated, "%s: offset paging returned the same row more than once: %v", label, duplicated)
	require.Lenf(t, seen, total,
		"%s: offset paging returned %d distinct rows, want %d (%d were skipped entirely)",
		label, len(seen), total, total-len(seen))
}

// ---------------------------------------------------------------------------
// Ordered fixtures: orderRows rows whose sort value is a permutation of their
// seed index g, so a list sorted by that value comes out in a different order
// than one sorted by g, by the reverse of g, or by a generated id.
// ---------------------------------------------------------------------------

const (
	orderRows = 30
	// orderMod is prime, so (g * 7) % orderMod maps 1..orderRows onto
	// 1..orderRows with no repeats. The seeding SQL uses the same formula.
	orderMod = 31
)

func permutedRank(g int) int {
	return (g * 7) % orderMod
}

// rankedKeys maps the key each seeded row is read back by to its sort value.
func rankedKeys(key func(g int) string) map[string]int {
	ranks := make(map[string]int, orderRows)
	for g := 1; g <= orderRows; g++ {
		ranks[key(g)] = permutedRank(g)
	}
	return ranks
}

// assertRankOrder checks that keys holds every seeded row exactly once and
// that the rows' sort values strictly rise (ascending) or fall (descending) in
// the order the pages delivered them.
func assertRankOrder(t *testing.T, keys []string, ranks map[string]int, ascending bool, label string) {
	t.Helper()
	seen := map[string]int{}
	for _, key := range keys {
		_, ok := ranks[key]
		require.Truef(t, ok, "%s: paging returned a row that was never seeded: %s", label, key)
		seen[key]++
	}
	assertCompleteAndUnique(t, seen, len(ranks), label)
	for i := 1; i < len(keys); i++ {
		prev, cur := ranks[keys[i-1]], ranks[keys[i]]
		if ascending {
			require.Lessf(t, prev, cur, "%s: %s came after %s, against the list's order", label, keys[i], keys[i-1])
		} else {
			require.Greaterf(t, prev, cur, "%s: %s came after %s, against the list's order", label, keys[i], keys[i-1])
		}
	}
}

// ===========================================================================
// Paging a list over a static dataset whose rows all tie on the list's default
// sort key returns every row exactly once: no omissions, no duplicates. One
// subtest per list. Each batch is inserted in one transaction, so timestamp
// columns that default to now() share one value. Columns with no unique index
// hold one shared value too, so the rows differ only in their unique key.
// Each subtest asserts only completeness and uniqueness.
// ===========================================================================

func TestPaginationStability(t *testing.T) {
	ctx := context.Background()

	// Workload identities and service accounts both list oldest first by
	// creation time.
	t.Run("workload_identity", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "stability-wi-ws"
		const total = 240
		const pageSize = 7
		seedWorkspace(ctx, t, seedDB, workspace)

		// All rows share one created_at (now() is the transaction timestamp)
		// and one name.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workload_identity (email, name, workspace)
			SELECT 'wi' || g || '@workload.bytebase.com', 'shared wi', $1
			FROM generate_series(1, $2::int) g`,
			workspace, total)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		seen := walkPages(t, total, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			wis, err := stores.ListWorkloadIdentities(ctx, &store.FindWorkloadIdentityMessage{
				Workspace: workspace,
				Limit:     &l,
				Offset:    &o,
			})
			require.NoErrorf(t, err, "workload_identity page at offset %d", offset)
			keys := make([]string, 0, len(wis))
			for _, wi := range wis {
				keys = append(keys, wi.Email)
			}
			return keys
		})
		assertCompleteAndUnique(t, seen, total, "workload_identity")
	})

	t.Run("service_account", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "stability-sa-ws"
		const total = 240
		const pageSize = 7
		seedWorkspace(ctx, t, seedDB, workspace)

		// All service accounts share one created_at and one name.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO service_account (email, name, workspace, service_key_hash)
			SELECT 'sa' || g || '@service.bytebase.com', 'shared sa', $1, 'hash'
			FROM generate_series(1, $2::int) g`,
			workspace, total)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		seen := walkPages(t, total, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			sas, err := stores.ListServiceAccounts(ctx, &store.FindServiceAccountMessage{
				Workspace: workspace,
				Limit:     &l,
				Offset:    &o,
			})
			require.NoErrorf(t, err, "service_account page at offset %d", offset)
			keys := make([]string, 0, len(sas))
			for _, sa := range sas {
				keys = append(keys, sa.Email)
			}
			return keys
		})
		assertCompleteAndUnique(t, seen, total, "service_account")
	})

	// The remaining subtests cover other list shapes: newest-first by creation
	// time (access grants, releases), a version string sort (revisions), an
	// email sort where every email is empty (groups), and oldest-first users.

	t.Run("access_grant", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "stability-ag-ws"
		const project = "stability-ag-p"
		const total = 240
		const pageSize = 7
		seedWorkspace(ctx, t, seedDB, workspace)
		seedProject(ctx, t, seedDB, project, workspace)

		// id has no default, so it is supplied; created_at is the one
		// transaction timestamp.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO access_grant (id, project, creator)
			SELECT 'ag' || g, $1, 'users/ag@example.com'
			FROM generate_series(1, $2::int) g`,
			project, total)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		seen := walkPages(t, total, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			grants, err := stores.ListAccessGrants(ctx, &store.FindAccessGrantMessage{
				Workspace: workspace,
				Limit:     &l,
				Offset:    &o,
			})
			require.NoErrorf(t, err, "access_grant page at offset %d", offset)
			keys := make([]string, 0, len(grants))
			for _, grant := range grants {
				keys = append(keys, grant.ID)
			}
			return keys
		})
		assertCompleteAndUnique(t, seen, total, "access_grant")
	})

	t.Run("release", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "stability-rel-ws"
		const project = "stability-rel-p"
		const total = 240
		const pageSize = 7
		seedWorkspace(ctx, t, seedDB, workspace)
		seedProject(ctx, t, seedDB, project, workspace)

		// One project, 16 trains with 15 iterations each, so each train and
		// each iteration number repeats and only (train, iteration) tells two
		// releases apart. release_id keeps its empty default, and every row
		// shares the single transaction created_at.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO release (project, train, iteration, creator)
			SELECT $1, 't' || lpad((g % 16)::text, 2, '0'), g / 16 + 1, 'users/rel@example.com'
			FROM generate_series(0, $2::int - 1) g`,
			project, total)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		pid := project
		seen := walkPages(t, total, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			releases, err := stores.ListReleases(ctx, &store.FindReleaseMessage{
				ProjectID: &pid,
				Limit:     &l,
				Offset:    &o,
			})
			require.NoErrorf(t, err, "release page at offset %d", offset)
			keys := make([]string, 0, len(releases))
			for _, r := range releases {
				keys = append(keys, fmt.Sprintf("%s/%d", r.Train, r.Iteration))
			}
			return keys
		})
		assertCompleteAndUnique(t, seen, total, "release")
	})

	t.Run("revision", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "stability-rev-ws"
		const project = "stability-rev-p"
		const instance = "stability-rev-i"
		const dbName = "stability-rev-db"
		const total = 240
		const pageSize = 7
		seedWorkspace(ctx, t, seedDB, workspace)
		seedProject(ctx, t, seedDB, project, workspace)
		seedInstance(ctx, t, seedDB, instance, workspace)
		seedDatabase(ctx, t, seedDB, instance, dbName, project)

		// Every revision shares one version string. The payload is left empty
		// so its type is NULL and the partial unique index on (instance,
		// db_name, type, version) treats the rows as distinct.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO revision (instance, db_name, version)
			SELECT $1, $2, '1.0.0'
			FROM generate_series(1, $3::int) g`,
			instance, dbName, total)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		seen := walkPages(t, total, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			revisions, err := stores.ListRevisions(ctx, &store.FindRevisionMessage{
				InstanceID: instance,
				Limit:      &l,
				Offset:     &o,
			})
			require.NoErrorf(t, err, "revision page at offset %d", offset)
			keys := make([]string, 0, len(revisions))
			for _, r := range revisions {
				keys = append(keys, r.ResourceID)
			}
			return keys
		})
		assertCompleteAndUnique(t, seen, total, "revision")
	})

	t.Run("group", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "stability-grp-ws"
		const total = 240
		const pageSize = 7
		seedWorkspace(ctx, t, seedDB, workspace)

		// email is left NULL, so every row ties on email, and the partial
		// unique index on (workspace, email) skips NULL emails. Every group
		// has the same name. id is generated.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO user_group (workspace, name)
			SELECT $1, 'shared group'
			FROM generate_series(1, $2::int) g`,
			workspace, total)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		seen := walkPages(t, total, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			groups, err := stores.ListGroups(ctx, &store.FindGroupMessage{
				Workspace: workspace,
				Limit:     &l,
				Offset:    &o,
			})
			require.NoErrorf(t, err, "group page at offset %d", offset)
			keys := make([]string, 0, len(groups))
			for _, grp := range groups {
				keys = append(keys, grp.ID)
			}
			return keys
		})
		assertCompleteAndUnique(t, seen, total, "group")
	})

	t.Run("principal", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const total = 240
		const pageSize = 7

		// Every principal shares one created_at and one name.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO principal (name, email, password_hash)
			SELECT 'shared user', 'user' || g || '@example.com', ''
			FROM generate_series(1, $1::int) g`,
			total)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		seen := walkPages(t, total, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			users, err := stores.ListUsers(ctx, &store.FindUserMessage{
				Limit:  &l,
				Offset: &o,
			})
			require.NoErrorf(t, err, "principal page at offset %d", offset)
			keys := make([]string, 0, len(users))
			for _, u := range users {
				keys = append(keys, u.Email)
			}
			return keys
		})
		assertCompleteAndUnique(t, seen, total, "principal")
	})
}

// ===========================================================================
// Projects sorted by title, databases sorted by project and access grants
// sorted by creator, over rows that all share the caller's sort value, page
// with every row exactly once in both sort directions. The sort keys come from
// the store's order_by parser for each list, as in the API layer.
// ===========================================================================

func TestCallerOrderByStability(t *testing.T) {
	ctx := context.Background()

	t.Run("project_by_title", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "caller-proj-ws"
		const total = 240
		const pageSize = 7
		seedWorkspace(ctx, t, seedDB, workspace)

		// Every project shares one title; resource_id is distinct. There is
		// no index on the title column.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO project (resource_id, workspace, name)
			SELECT 'caller-proj-' || g, $1, 'shared title'
			FROM generate_series(1, $2::int) g`,
			workspace, total)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		for _, orderBy := range []string{"title", "title desc"} {
			orderKeys := sortKeys(t, store.GetProjectOrders, orderBy)
			seen := walkPages(t, total, pageSize, func(limit, offset int) []string {
				l, o := limit, offset
				projects, err := stores.ListProjects(ctx, &store.FindProjectMessage{
					Workspace:   workspace,
					OrderByKeys: orderKeys,
					Limit:       &l,
					Offset:      &o,
				})
				require.NoErrorf(t, err, "project page at offset %d", offset)
				keys := make([]string, 0, len(projects))
				for _, p := range projects {
					keys = append(keys, p.ResourceID)
				}
				return keys
			})
			assertCompleteAndUnique(t, seen, total, "projects by "+orderBy)
		}
	})

	t.Run("database_by_project", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "caller-db-ws"
		const project = "caller-db-p"
		const instances = 16
		const total = 240
		const pageSize = 7
		seedWorkspace(ctx, t, seedDB, workspace)
		seedProject(ctx, t, seedDB, project, workspace)
		for i := 0; i < instances; i++ {
			seedInstance(ctx, t, seedDB, fmt.Sprintf("caller-db-i%02d", i), workspace)
		}

		// All databases share one project. They are spread over 16 instances
		// with 15 names each, and every name is used on every instance, so
		// only (instance, name) tells two databases apart.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO db (instance, name, project, environment)
			SELECT 'caller-db-i' || lpad((g % $3::int)::text, 2, '0'), 'db' || (g / $3::int), $1, 'prod'
			FROM generate_series(0, $2::int - 1) g`,
			project, total, instances)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		for _, orderBy := range []string{"project", "project desc"} {
			orderKeys := sortKeys(t, store.GetDatabaseOrders, orderBy)
			seen := walkPages(t, total, pageSize, func(limit, offset int) []string {
				l, o := limit, offset
				databases, err := stores.ListDatabases(ctx, &store.FindDatabaseMessage{
					Workspace:   workspace,
					OrderByKeys: orderKeys,
					Limit:       &l,
					Offset:      &o,
				})
				require.NoErrorf(t, err, "database page at offset %d", offset)
				keys := make([]string, 0, len(databases))
				for _, d := range databases {
					keys = append(keys, fmt.Sprintf("%s/%s", d.InstanceID, d.DatabaseName))
				}
				return keys
			})
			assertCompleteAndUnique(t, seen, total, "databases by "+orderBy)
		}
	})

	t.Run("access_grant_by_creator", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "caller-ag-ws"
		const project = "caller-ag-p"
		const total = 240
		const pageSize = 7
		seedWorkspace(ctx, t, seedDB, workspace)
		seedProject(ctx, t, seedDB, project, workspace)

		// Every grant has the same creator.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO access_grant (id, project, creator)
			SELECT 'cag' || g, $1, 'users/cag@example.com'
			FROM generate_series(1, $2::int) g`,
			project, total)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		for _, orderBy := range []string{"creator", "creator desc"} {
			orderKeys := sortKeys(t, store.GetAccessGrantOrders, orderBy)
			seen := walkPages(t, total, pageSize, func(limit, offset int) []string {
				l, o := limit, offset
				grants, err := stores.ListAccessGrants(ctx, &store.FindAccessGrantMessage{
					Workspace:   workspace,
					OrderByKeys: orderKeys,
					Limit:       &l,
					Offset:      &o,
				})
				require.NoErrorf(t, err, "access_grant page at offset %d", offset)
				keys := make([]string, 0, len(grants))
				for _, grant := range grants {
					keys = append(keys, grant.ID)
				}
				return keys
			})
			assertCompleteAndUnique(t, seen, total, "access grants by "+orderBy)
		}
	})
}

// ===========================================================================
// Every list keeps the order it reads in today when its rows have distinct
// sort values: the default order of each list in TestPaginationStability, and
// both directions of a caller's order on projects, databases and access
// grants. The rows are
// read back page by page and must come out complete and in that order.
// ===========================================================================

func TestListOrderKept(t *testing.T) {
	ctx := context.Background()
	const pageSize = 7

	t.Run("workload_identity_oldest_first", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "order-wi-ws"
		seedWorkspace(ctx, t, seedDB, workspace)
		_, err := seedDB.ExecContext(ctx, `
			INSERT INTO workload_identity (email, name, workspace, created_at)
			SELECT 'wi' || lpad(g::text, 3, '0') || '@workload.bytebase.com', 'wi ' || g, $1,
			       TIMESTAMPTZ '2026-01-01 00:00:00+00' + ((g * 7) % $3::int) * INTERVAL '1 minute'
			FROM generate_series(1, $2::int) g`,
			workspace, orderRows, orderMod)
		require.NoError(t, err)

		keys := walkInOrder(orderRows, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			wis, err := stores.ListWorkloadIdentities(ctx, &store.FindWorkloadIdentityMessage{
				Workspace: workspace,
				Limit:     &l,
				Offset:    &o,
			})
			require.NoErrorf(t, err, "workload_identity page at offset %d", offset)
			out := make([]string, 0, len(wis))
			for _, wi := range wis {
				out = append(out, wi.Email)
			}
			return out
		})
		ranks := rankedKeys(func(g int) string { return fmt.Sprintf("wi%03d@workload.bytebase.com", g) })
		assertRankOrder(t, keys, ranks, true, "workload identities by creation time")
	})

	t.Run("service_account_oldest_first", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "order-sa-ws"
		seedWorkspace(ctx, t, seedDB, workspace)
		_, err := seedDB.ExecContext(ctx, `
			INSERT INTO service_account (email, name, workspace, service_key_hash, created_at)
			SELECT 'sa' || lpad(g::text, 3, '0') || '@service.bytebase.com', 'sa ' || g, $1, 'hash',
			       TIMESTAMPTZ '2026-01-01 00:00:00+00' + ((g * 7) % $3::int) * INTERVAL '1 minute'
			FROM generate_series(1, $2::int) g`,
			workspace, orderRows, orderMod)
		require.NoError(t, err)

		keys := walkInOrder(orderRows, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			sas, err := stores.ListServiceAccounts(ctx, &store.FindServiceAccountMessage{
				Workspace: workspace,
				Limit:     &l,
				Offset:    &o,
			})
			require.NoErrorf(t, err, "service_account page at offset %d", offset)
			out := make([]string, 0, len(sas))
			for _, sa := range sas {
				out = append(out, sa.Email)
			}
			return out
		})
		ranks := rankedKeys(func(g int) string { return fmt.Sprintf("sa%03d@service.bytebase.com", g) })
		assertRankOrder(t, keys, ranks, true, "service accounts by creation time")
	})

	t.Run("access_grant_newest_first", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "order-ag-ws"
		const project = "order-ag-p"
		seedWorkspace(ctx, t, seedDB, workspace)
		seedProject(ctx, t, seedDB, project, workspace)
		_, err := seedDB.ExecContext(ctx, `
			INSERT INTO access_grant (id, project, creator, created_at)
			SELECT 'ag' || lpad(g::text, 3, '0'), $1, 'users/ag@example.com',
			       TIMESTAMPTZ '2026-01-01 00:00:00+00' + ((g * 7) % $3::int) * INTERVAL '1 minute'
			FROM generate_series(1, $2::int) g`,
			project, orderRows, orderMod)
		require.NoError(t, err)

		keys := walkInOrder(orderRows, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			grants, err := stores.ListAccessGrants(ctx, &store.FindAccessGrantMessage{
				Workspace: workspace,
				Limit:     &l,
				Offset:    &o,
			})
			require.NoErrorf(t, err, "access_grant page at offset %d", offset)
			out := make([]string, 0, len(grants))
			for _, grant := range grants {
				out = append(out, grant.ID)
			}
			return out
		})
		ranks := rankedKeys(func(g int) string { return fmt.Sprintf("ag%03d", g) })
		assertRankOrder(t, keys, ranks, false, "access grants by creation time")
	})

	t.Run("release_newest_first", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "order-rel-ws"
		const project = "order-rel-p"
		seedWorkspace(ctx, t, seedDB, workspace)
		seedProject(ctx, t, seedDB, project, workspace)
		_, err := seedDB.ExecContext(ctx, `
			INSERT INTO release (project, train, iteration, release_id, creator, created_at)
			SELECT $1, '', g, 'r' || g, 'users/rel@example.com',
			       TIMESTAMPTZ '2026-01-01 00:00:00+00' + ((g * 7) % $3::int) * INTERVAL '1 minute'
			FROM generate_series(1, $2::int) g`,
			project, orderRows, orderMod)
		require.NoError(t, err)

		pid := project
		keys := walkInOrder(orderRows, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			releases, err := stores.ListReleases(ctx, &store.FindReleaseMessage{
				ProjectID: &pid,
				Limit:     &l,
				Offset:    &o,
			})
			require.NoErrorf(t, err, "release page at offset %d", offset)
			out := make([]string, 0, len(releases))
			for _, r := range releases {
				out = append(out, fmt.Sprintf("%d", r.Iteration))
			}
			return out
		})
		ranks := rankedKeys(func(g int) string { return fmt.Sprintf("%d", g) })
		assertRankOrder(t, keys, ranks, false, "releases by creation time")
	})

	t.Run("revision_by_version", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "order-rev-ws"
		const project = "order-rev-p"
		const instance = "order-rev-i"
		const dbName = "order-rev-db"
		seedWorkspace(ctx, t, seedDB, workspace)
		seedProject(ctx, t, seedDB, project, workspace)
		seedInstance(ctx, t, seedDB, instance, workspace)
		seedDatabase(ctx, t, seedDB, instance, dbName, project)
		// resource_id is a generated UUID, so its order has nothing to do with
		// the version order.
		_, err := seedDB.ExecContext(ctx, `
			INSERT INTO revision (instance, db_name, version)
			SELECT $1, $2, 'v' || lpad(((g * 7) % $4::int)::text, 3, '0')
			FROM generate_series(1, $3::int) g`,
			instance, dbName, orderRows, orderMod)
		require.NoError(t, err)

		keys := walkInOrder(orderRows, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			revisions, err := stores.ListRevisions(ctx, &store.FindRevisionMessage{
				InstanceID: instance,
				Limit:      &l,
				Offset:     &o,
			})
			require.NoErrorf(t, err, "revision page at offset %d", offset)
			out := make([]string, 0, len(revisions))
			for _, r := range revisions {
				out = append(out, r.Version)
			}
			return out
		})
		ranks := rankedKeys(func(g int) string { return fmt.Sprintf("v%03d", permutedRank(g)) })
		assertRankOrder(t, keys, ranks, false, "revisions by version")
	})

	t.Run("group_by_email", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "order-grp-ws"
		seedWorkspace(ctx, t, seedDB, workspace)
		// id is a generated UUID, so its order has nothing to do with the
		// email order.
		_, err := seedDB.ExecContext(ctx, `
			INSERT INTO user_group (workspace, name, email)
			SELECT $1, 'group ' || g, 'grp' || lpad(((g * 7) % $3::int)::text, 3, '0') || '@example.com'
			FROM generate_series(1, $2::int) g`,
			workspace, orderRows, orderMod)
		require.NoError(t, err)

		keys := walkInOrder(orderRows, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			groups, err := stores.ListGroups(ctx, &store.FindGroupMessage{
				Workspace: workspace,
				Limit:     &l,
				Offset:    &o,
			})
			require.NoErrorf(t, err, "group page at offset %d", offset)
			out := make([]string, 0, len(groups))
			for _, grp := range groups {
				out = append(out, grp.Email)
			}
			return out
		})
		ranks := rankedKeys(func(g int) string { return fmt.Sprintf("grp%03d@example.com", permutedRank(g)) })
		assertRankOrder(t, keys, ranks, true, "groups by email")
	})

	t.Run("principal_oldest_first", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		// principal.id is a serial that follows g, not the creation time.
		_, err := seedDB.ExecContext(ctx, `
			INSERT INTO principal (name, email, password_hash, created_at)
			SELECT 'user ' || g, 'user' || lpad(g::text, 3, '0') || '@example.com', '',
			       TIMESTAMPTZ '2026-01-01 00:00:00+00' + ((g * 7) % $2::int) * INTERVAL '1 minute'
			FROM generate_series(1, $1::int) g`,
			orderRows, orderMod)
		require.NoError(t, err)

		keys := walkInOrder(orderRows, pageSize, func(limit, offset int) []string {
			l, o := limit, offset
			users, err := stores.ListUsers(ctx, &store.FindUserMessage{
				Limit:  &l,
				Offset: &o,
			})
			require.NoErrorf(t, err, "principal page at offset %d", offset)
			out := make([]string, 0, len(users))
			for _, u := range users {
				out = append(out, u.Email)
			}
			return out
		})
		ranks := rankedKeys(func(g int) string { return fmt.Sprintf("user%03d@example.com", g) })
		assertRankOrder(t, keys, ranks, true, "users by creation time")
	})

	t.Run("project_by_title_both_directions", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "order-proj-ws"
		seedWorkspace(ctx, t, seedDB, workspace)
		_, err := seedDB.ExecContext(ctx, `
			INSERT INTO project (resource_id, workspace, name)
			SELECT 'order-proj-' || lpad(g::text, 3, '0'), $1, 'title ' || lpad(((g * 7) % $3::int)::text, 3, '0')
			FROM generate_series(1, $2::int) g`,
			workspace, orderRows, orderMod)
		require.NoError(t, err)

		ranks := rankedKeys(func(g int) string { return fmt.Sprintf("order-proj-%03d", g) })
		for _, orderBy := range []string{"title", "title desc"} {
			orderKeys := sortKeys(t, store.GetProjectOrders, orderBy)
			keys := walkInOrder(orderRows, pageSize, func(limit, offset int) []string {
				l, o := limit, offset
				projects, err := stores.ListProjects(ctx, &store.FindProjectMessage{
					Workspace:   workspace,
					OrderByKeys: orderKeys,
					Limit:       &l,
					Offset:      &o,
				})
				require.NoErrorf(t, err, "project page at offset %d", offset)
				out := make([]string, 0, len(projects))
				for _, p := range projects {
					out = append(out, p.ResourceID)
				}
				return out
			})
			assertRankOrder(t, keys, ranks, orderBy == "title", "projects by "+orderBy)
		}
	})

	t.Run("database_by_project_both_directions", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "order-db-ws"
		const instance = "order-db-i"
		seedWorkspace(ctx, t, seedDB, workspace)
		seedInstance(ctx, t, seedDB, instance, workspace)
		// One project per database. Database g sits in the project whose name
		// carries g's rank, so the project order differs from the
		// (instance, name) order.
		_, err := seedDB.ExecContext(ctx, `
			INSERT INTO project (resource_id, workspace, name)
			SELECT 'order-db-p' || lpad(g::text, 3, '0'), $1, 'p ' || g
			FROM generate_series(1, $2::int) g`,
			workspace, orderRows)
		require.NoError(t, err)
		_, err = seedDB.ExecContext(ctx, `
			INSERT INTO db (instance, name, project, environment)
			SELECT $1, 'db' || lpad(g::text, 3, '0'), 'order-db-p' || lpad(((g * 7) % $3::int)::text, 3, '0'), 'prod'
			FROM generate_series(1, $2::int) g`,
			instance, orderRows, orderMod)
		require.NoError(t, err)

		ranks := rankedKeys(func(g int) string { return fmt.Sprintf("%s/db%03d", instance, g) })
		for _, orderBy := range []string{"project", "project desc"} {
			orderKeys := sortKeys(t, store.GetDatabaseOrders, orderBy)
			keys := walkInOrder(orderRows, pageSize, func(limit, offset int) []string {
				l, o := limit, offset
				databases, err := stores.ListDatabases(ctx, &store.FindDatabaseMessage{
					Workspace:   workspace,
					OrderByKeys: orderKeys,
					Limit:       &l,
					Offset:      &o,
				})
				require.NoErrorf(t, err, "database page at offset %d", offset)
				out := make([]string, 0, len(databases))
				for _, d := range databases {
					out = append(out, fmt.Sprintf("%s/%s", d.InstanceID, d.DatabaseName))
				}
				return out
			})
			assertRankOrder(t, keys, ranks, orderBy == "project", "databases by "+orderBy)
		}
	})

	t.Run("access_grant_by_creator_both_directions", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "order-cag-ws"
		const project = "order-cag-p"
		seedWorkspace(ctx, t, seedDB, workspace)
		seedProject(ctx, t, seedDB, project, workspace)
		// Grant g's creator carries g's rank, so the creator order differs from
		// the id order.
		_, err := seedDB.ExecContext(ctx, `
			INSERT INTO access_grant (id, project, creator)
			SELECT 'ag' || lpad(g::text, 3, '0'), $1, 'users/c' || lpad(((g * 7) % $3::int)::text, 3, '0') || '@example.com'
			FROM generate_series(1, $2::int) g`,
			project, orderRows, orderMod)
		require.NoError(t, err)

		ranks := rankedKeys(func(g int) string { return fmt.Sprintf("ag%03d", g) })
		for _, orderBy := range []string{"creator", "creator desc"} {
			orderKeys := sortKeys(t, store.GetAccessGrantOrders, orderBy)
			keys := walkInOrder(orderRows, pageSize, func(limit, offset int) []string {
				l, o := limit, offset
				grants, err := stores.ListAccessGrants(ctx, &store.FindAccessGrantMessage{
					Workspace:   workspace,
					OrderByKeys: orderKeys,
					Limit:       &l,
					Offset:      &o,
				})
				require.NoErrorf(t, err, "access_grant page at offset %d", offset)
				out := make([]string, 0, len(grants))
				for _, grant := range grants {
					out = append(out, grant.ID)
				}
				return out
			})
			assertRankOrder(t, keys, ranks, orderBy == "creator", "access grants by "+orderBy)
		}
	})
}

// ===========================================================================
// The issues list across several projects, where issue ids are assigned per
// project and so repeat from one project to the next, pages with every issue
// exactly once and keeps its newest-first order by issue id. 3 projects each
// hold ids 101..160 (the store's per-project counter starts at 101), all
// created in one transaction.
// ===========================================================================

func TestIssueCrossProjectStability(t *testing.T) {
	ctx := context.Background()
	stores, seedDB := freshStore(ctx, t)

	const (
		workspace    = "xproj-ws"
		projectCount = 3
		perProject   = 60
		pageSize     = 7
	)
	seedWorkspace(ctx, t, seedDB, workspace)

	projectIDs := make([]string, 0, projectCount)
	for i := 0; i < projectCount; i++ {
		projectIDs = append(projectIDs, fmt.Sprintf("xproj-p%d", i))
	}

	// Seed all projects and their issues in one transaction so every row shares
	// a created_at and ids repeat across projects.
	tx, err := seedDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	for _, projectID := range projectIDs {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO project (resource_id, workspace, name) VALUES ($1, $2, $3)`,
			projectID, workspace, projectID)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO issue (id, project, creator, name, status, type, description)
			SELECT g, $1, 'users/xproj@example.com', 'issue ' || g, 'OPEN', 'DATABASE_CHANGE', ''
			FROM generate_series(101, $2::int) g`,
			projectID, 100+perProject)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	total := projectCount * perProject
	var uids []int64
	seen := walkPages(t, total, pageSize, func(limit, offset int) []string {
		l, o := limit, offset
		issues, err := stores.ListIssues(ctx, &store.FindIssueMessage{
			ProjectIDs: projectIDs,
			Limit:      &l,
			Offset:     &o,
		})
		require.NoErrorf(t, err, "issues page at offset %d", offset)
		keys := make([]string, 0, len(issues))
		for _, issue := range issues {
			keys = append(keys, fmt.Sprintf("%s/%d", issue.ProjectID, issue.UID))
			uids = append(uids, issue.UID)
		}
		return keys
	})
	assertCompleteAndUnique(t, seen, total, "issues cross-project")
	for i := 1; i < len(uids); i++ {
		require.LessOrEqualf(t, uids[i], uids[i-1],
			"issues cross-project: issue %d came after issue %d, against the newest-first order", uids[i], uids[i-1])
	}
}

// ===========================================================================
// The issues list across several projects, sorted by create time or update
// time in either direction, pages with every issue exactly once and in the
// requested order. Issue ids repeat across 3 projects, and the time columns
// repeat across groups of ids, so many rows share the requested sort value.
// ===========================================================================

func TestIssueCallerOrderCrossProject(t *testing.T) {
	ctx := context.Background()
	stores, seedDB := freshStore(ctx, t)

	const (
		workspace    = "xorder-ws"
		projectCount = 3
		perProject   = 60
		pageSize     = 7
	)
	seedWorkspace(ctx, t, seedDB, workspace)

	projectIDs := make([]string, 0, projectCount)
	for i := 0; i < projectCount; i++ {
		projectID := fmt.Sprintf("xorder-p%d", i)
		projectIDs = append(projectIDs, projectID)
		seedProject(ctx, t, seedDB, projectID, workspace)
		// created_at takes 4 values and updated_at 5, both cycling with the id,
		// so neither follows the id order.
		_, err := seedDB.ExecContext(ctx, `
			INSERT INTO issue (id, project, creator, name, status, type, description, created_at, updated_at)
			SELECT g, $1, 'users/xorder@example.com', 'issue ' || g, 'OPEN', 'DATABASE_CHANGE', '',
			       TIMESTAMPTZ '2026-01-01 00:00:00+00' + (g % 4) * INTERVAL '1 minute',
			       TIMESTAMPTZ '2026-02-01 00:00:00+00' + (g % 5) * INTERVAL '1 minute'
			FROM generate_series(101, $2::int) g`,
			projectID, 100+perProject)
		require.NoError(t, err)
	}
	total := projectCount * perProject

	for _, orderBy := range []string{"create_time", "create_time desc", "update_time", "update_time desc"} {
		t.Run(orderBy, func(t *testing.T) {
			got := walkIssues(ctx, t, stores, store.FindIssueMessage{
				ProjectIDs:  projectIDs,
				OrderByKeys: sortKeys(t, store.GetIssueOrders, orderBy),
			}, total, pageSize)
			assertCompleteAndUnique(t, issueKeyCounts(got), total, "issues by "+orderBy)
			for i := 1; i < len(got); i++ {
				prev, cur := got[i-1].CreatedAt, got[i].CreatedAt
				if strings.HasPrefix(orderBy, "update_time") {
					prev, cur = got[i-1].UpdatedAt, got[i].UpdatedAt
				}
				outOfOrder := cur.Before(prev)
				if strings.HasSuffix(orderBy, " desc") {
					outOfOrder = cur.After(prev)
				}
				require.Falsef(t, outOfOrder, "issues by %s: %s/%d came after %s/%d, against the requested order",
					orderBy, got[i].ProjectID, got[i].UID, got[i-1].ProjectID, got[i-1].UID)
			}
		})
	}
}

// ===========================================================================
// Issue search across several projects pages with every match exactly once,
// and keeps relevance order: issues whose search text holds the term come
// before issues that only match on a title substring. Issue ids repeat across
// 3 projects.
// ===========================================================================

func TestIssueSearchCrossProject(t *testing.T) {
	ctx := context.Background()
	stores, seedDB := freshStore(ctx, t)

	const (
		workspace    = "xsearch-ws"
		projectCount = 3
		perProject   = 60
		pageSize     = 7
	)
	seedWorkspace(ctx, t, seedDB, workspace)

	// Every third id holds the term in its stored search text. The other
	// issues reach the results only through the title substring match, since
	// their stored search text does not hold the term.
	strongVector := store.IssueSearchVector("deploy step", "")
	weakVector := store.IssueSearchVector("cache", "")
	isStrong := func(uid int64) bool { return uid%3 == 0 }

	projectIDs := make([]string, 0, projectCount)
	for i := 0; i < projectCount; i++ {
		projectID := fmt.Sprintf("xsearch-p%d", i)
		projectIDs = append(projectIDs, projectID)
		seedProject(ctx, t, seedDB, projectID, workspace)
		_, err := seedDB.ExecContext(ctx, `
			INSERT INTO issue (id, project, creator, name, status, type, description, ts_vector)
			SELECT g, $1, 'users/xsearch@example.com',
			       CASE WHEN g % 3 = 0 THEN 'deploy step' ELSE 'redeploy cache' END,
			       'OPEN', 'DATABASE_CHANGE', '',
			       CAST(CASE WHEN g % 3 = 0 THEN $3::text ELSE $4::text END AS tsvector)
			FROM generate_series(101, $2::int) g`,
			projectID, 100+perProject, strongVector, weakVector)
		require.NoError(t, err)
	}
	total := projectCount * perProject

	query := "deploy"
	got := walkIssues(ctx, t, stores, store.FindIssueMessage{
		ProjectIDs: projectIDs,
		Query:      &query,
	}, total, pageSize)
	assertCompleteAndUnique(t, issueKeyCounts(got), total, "issue search")

	seenWeak := false
	for _, issue := range got {
		if isStrong(issue.UID) {
			require.Falsef(t, seenWeak,
				"issue search: %s/%d holds the term but came after an issue that only matched its title",
				issue.ProjectID, issue.UID)
		} else {
			seenWeak = true
		}
	}
}

// ===========================================================================
// A batch of issue comments created together, as one multi-field issue update
// writes them, reads back page by page in the order it was written, with
// every comment exactly once. One comment of the batch is edited before the
// read, and the edit does not move it. The batch of 32 spans several pages.
// ===========================================================================

func TestIssueCommentBatchOrder(t *testing.T) {
	ctx := context.Background()
	stores, seedDB := freshStore(ctx, t)

	const (
		workspace = "comment-ws"
		projectID = "comment-p"
		issueUID  = int64(101)
		pageSize  = 7
	)
	seedWorkspace(ctx, t, seedDB, workspace)
	seedProject(ctx, t, seedDB, projectID, workspace)
	_, err := seedDB.ExecContext(ctx, `
		INSERT INTO issue (id, project, creator, name, status, type, description)
		VALUES ($1, $2, 'users/comment@example.com', 'issue', 'OPEN', 'DATABASE_CHANGE', '')`,
		issueUID, projectID)
	require.NoError(t, err)

	// A multi-field update writes one comment per changed field in a single
	// batch; the batch is padded to 32 comments.
	want := []string{"title", "description", "labels", "status", "assignee"}
	for i := len(want); i < 32; i++ {
		want = append(want, fmt.Sprintf("field_%02d", i))
	}

	creates := make([]*store.IssueCommentMessage, 0, len(want))
	for _, comment := range want {
		creates = append(creates, &store.IssueCommentMessage{
			ProjectID: projectID,
			IssueUID:  issueUID,
			Payload:   &storepb.IssueCommentPayload{Comment: comment},
		})
	}
	_, err = stores.CreateIssueComments(ctx, "users/comment@example.com", creates...)
	require.NoError(t, err)

	// Edit one comment in the middle of the batch, keeping its text.
	uid := issueUID
	all, err := stores.ListIssueComment(ctx, &store.FindIssueCommentMessage{
		ProjectID: projectID,
		IssueUID:  &uid,
	})
	require.NoError(t, err)
	require.Len(t, all, len(want))
	edited := "labels"
	var editedID string
	for _, comment := range all {
		if comment.Payload.Comment == edited {
			editedID = comment.ResourceID
		}
	}
	require.NotEmpty(t, editedID, "the batch should hold the %q comment", edited)
	require.NoError(t, stores.UpdateIssueComment(ctx, &store.UpdateIssueCommentMessage{
		ProjectID:  projectID,
		ResourceID: editedID,
		Comment:    &edited,
	}))

	// Read the feed page by page with a fresh LIMIT/OFFSET per page and
	// concatenate in page order.
	order := make([]string, 0, len(want))
	for offset := 0; offset < len(want); offset += pageSize {
		l, o := pageSize, offset
		page, err := stores.ListIssueComment(ctx, &store.FindIssueCommentMessage{
			ProjectID: projectID,
			IssueUID:  &uid,
			Limit:     &l,
			Offset:    &o,
		})
		require.NoErrorf(t, err, "issue_comment page at offset %d", offset)
		for _, comment := range page {
			order = append(order, comment.Payload.Comment)
		}
	}
	require.Equal(t, want, order,
		"a batch of issue comments must read back in the order it was written, across pages")
}

// ===========================================================================
// A caller-supplied ordering on the issues list is honored: with distinct
// create times, a create_time ascending read returns the issues in that order.
// ===========================================================================

func TestIssueOrderByPreserved(t *testing.T) {
	ctx := context.Background()
	stores, seedDB := freshStore(ctx, t)

	const (
		workspace = "orderby-ws"
		projectID = "orderby-p"
		count     = 25
	)
	seedWorkspace(ctx, t, seedDB, workspace)
	seedProject(ctx, t, seedDB, projectID, workspace)

	// Distinct, strictly increasing created_at per row; id ascends with
	// created_at, so an ascending create_time sort yields ids 101,102,... in
	// order.
	_, err := seedDB.ExecContext(ctx, `
		INSERT INTO issue (id, project, creator, name, status, type, description, created_at)
		SELECT 100 + g, $1, 'users/orderby@example.com', 'issue ' || g, 'OPEN', 'DATABASE_CHANGE', '',
		       now() + (g * interval '1 second')
		FROM generate_series(1, $2::int) g`,
		projectID, count)
	require.NoError(t, err)

	orderKeys := []*store.OrderByKey{
		{Key: "issue.created_at", SortOrder: store.ASC},
	}
	limit := count
	issues, err := stores.ListIssues(ctx, &store.FindIssueMessage{
		ProjectIDs:  []string{projectID},
		OrderByKeys: orderKeys,
		Limit:       &limit,
	})
	require.NoError(t, err)
	require.Len(t, issues, count)

	// Rows come back with non-decreasing created_at.
	for i := 1; i < len(issues); i++ {
		require.Truef(t, !issues[i].CreatedAt.Before(issues[i-1].CreatedAt),
			"caller-requested create_time asc ordering violated at index %d: %v before %v",
			i, issues[i].CreatedAt, issues[i-1].CreatedAt)
	}
	// created_at increases with id, so the requested order is ascending UIDs.
	for i := 0; i < len(issues); i++ {
		require.Equalf(t, int64(101+i), issues[i].UID,
			"requested create_time asc ordering did not lead at index %d", i)
	}
}
