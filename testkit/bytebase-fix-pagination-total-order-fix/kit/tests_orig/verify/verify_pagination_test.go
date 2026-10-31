// External-package (store_test) contract test for the paginated-list stability
// change in backend/store. Drives only the public store API (store.New, the
// List*/Create* methods, the exported Find*/Message/OrderByKey types) against a
// real PostgreSQL — the defect is invisible to static reading and to a single
// unpaginated fetch, so it can only be reproduced by paging real seeded data in
// which rows tie on the displayed ordering.
//
// Infrastructure note: the upstream PR drives these scenarios through
// testcontainers-go, which pulls an image and needs a Docker daemon — neither is
// available in the offline grading sandbox. Instead each test creates its own
// freshly-named database on the locally-baked PostgreSQL that tests/test-setup.sh
// starts (connection parameters arrive via the PG*/BYTEBASE_TEST_PG_* env vars),
// runs the store schema migration into it, seeds through raw SQL, and reads back
// through the public store API. One database per test keeps them isolated on the
// shared server.

package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	storepb "github.com/bytebase/bytebase/backend/generated-go/store"
	"github.com/bytebase/bytebase/backend/migrator"
	"github.com/bytebase/bytebase/backend/store"

	// Registers the pgx stdlib driver used by store.New's connection manager.
	_ "github.com/bytebase/bytebase/backend/plugin/db/pg"
)

// ---------------------------------------------------------------------------
// Local PostgreSQL bootstrap (replaces the PR's testcontainer helper).
// ---------------------------------------------------------------------------

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// pgParams reads the baked server's connection parameters. tests/test-setup.sh
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

// freshStore creates a uniquely-named empty database on the baked server,
// migrates the store schema into it, and returns a store.Store bound to it plus
// a *sql.DB on the same database for raw seeding. Everything is torn down via
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
// Shared raw-SQL seeding helpers. These write only the columns the store's own
// CREATE statements expose; they never set any column the fix introduces, so an
// unfixed and a fixed implementation are seeded identically.
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
// Pagination walk helper: read a list page by page with a fresh LIMIT/OFFSET
// per page (the way the server's API layer does) and return how many times each
// row key was observed across the full walk.
// ---------------------------------------------------------------------------

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
		"%s: offset paging returned %d distinct rows, want %d — %d were skipped entirely",
		label, len(seen), total, total-len(seen))
}

// ===========================================================================
// Criterion: pagination_stability (fail_to_pass, basic_functionality)
//
// Paging an affected store list over a static dataset whose rows tie on the
// current sort key returns every row exactly once — no omissions, no
// duplicates. Parametrized across more than one affected list function so it
// asserts the fix is a class fix, not a single-query patch. Each sub-list is
// seeded so that its pre-fix default ordering (a non-unique column) ties many
// rows; pre-fix those ties reorder between page requests under LIMIT/OFFSET and
// the walk drops/repeats rows. Passes for any implementation whose ordering is
// total under the query scope, regardless of which unique column it ends on.
// ===========================================================================

func TestPaginationStability(t *testing.T) {
	ctx := context.Background()

	// Two independent affected lists, each reached through its public List API.
	// Both default-order on a column that is not unique (created_at), and
	// neither has an index on that column, so the page query is satisfied by a
	// bounded top-N sort over fully-tied rows rather than an ordered index scan
	// — which is what makes the pre-fix page boundaries reorder. A batch
	// inserted in one transaction ties exactly and exercises the instability
	// deterministically.
	t.Run("workload_identity", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "stability-wi-ws"
		const total = 240
		const pageSize = 7
		seedWorkspace(ctx, t, seedDB, workspace)

		// All rows share one created_at (now() is the transaction timestamp),
		// so the default ORDER BY created_at ASC ties every row.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workload_identity (email, name, workspace)
			SELECT 'wi' || g || '@workload.bytebase.com', 'wi ' || g, $1
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

		// All service accounts share one created_at, so the default
		// ORDER BY created_at ASC ties every row.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO service_account (email, name, workspace, service_key_hash)
			SELECT 'sa' || g || '@service.bytebase.com', 'sa ' || g, $1, 'hash'
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

	// The remaining subtests widen the same completeness/uniqueness walk to the
	// other affected lists so the fix is gated as a class, not a four-list
	// sample. Each one reproduces a pre-fix ordering shape the two created_at
	// lists above do not: a DESC-leading timestamp (access_grant, release), a
	// non-timestamp leading key (revision orders by a version string), an
	// email-leading list whose unique index is partial and so does not cover a
	// batch of rows with no email (group), and an ASC-leading timestamp on a
	// different table (principal/users). All are seeded so the leading key ties
	// across every row and none has an index that could turn the page query into
	// an ordered scan, so pre-fix each page is a bounded top-N sort over tied
	// rows that reorders between requests. Each asserts only completeness and
	// uniqueness, so it passes for any ordering that is total under the query's
	// scope regardless of the unique column it ends on.

	t.Run("access_grant", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "stability-ag-ws"
		const project = "stability-ag-p"
		const total = 240
		const pageSize = 7
		seedWorkspace(ctx, t, seedDB, workspace)
		seedProject(ctx, t, seedDB, project, workspace)

		// id has no default, so it is supplied; created_at defaults to the one
		// transaction timestamp and leads the default ORDER BY ... DESC.
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

		// One project, one train, distinct iterations, all sharing the single
		// transaction created_at that leads the default ORDER BY ... DESC.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO release (project, train, iteration, release_id, creator)
			SELECT $1, '', g, 'r' || g, 'users/rel@example.com'
			FROM generate_series(1, $2::int) g`,
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

		// Every revision shares one version string, which leads the default
		// ORDER BY version DESC. The payload is left empty so its type is NULL
		// and the partial unique index on (instance, db_name, type, version)
		// treats the rows as distinct, letting the tie stand.
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

		// email is left NULL, so the default ORDER BY email ties every row and
		// the partial unique index on (workspace, email) — which excludes NULL
		// emails — cannot serve the page as an ordered scan. id is generated.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO user_group (workspace, name)
			SELECT $1, 'group ' || g
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

		// Every principal shares one created_at, which leads the default
		// ORDER BY created_at ASC; there is no index on it.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO principal (name, email, password_hash)
			SELECT 'user ' || g, 'user' || g || '@example.com', ''
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
// Criterion: pagination_stability (fail_to_pass, basic_functionality)
//
// The same completeness/uniqueness guarantee for lists whose default ordering
// is already total but whose SQL replaces that default outright when the caller
// supplies an order_by. Pre-fix those lists page by the caller's key alone, so a
// caller sorting on a non-unique column (a title, a project) gets a non-total
// ordering and the page walk drops and repeats rows; the fix appends a
// scope-unique tiebreak after the caller's key. Seeds rows that tie on a
// non-unique caller key, pages with that order_by, and asserts complete+unique
// paging. Column-agnostic: passes for any fix whose ordering is total under the
// caller's requested lead.
// ===========================================================================

func TestCallerOrderByStability(t *testing.T) {
	ctx := context.Background()

	t.Run("project_by_title", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "caller-proj-ws"
		const total = 240
		const pageSize = 7
		seedWorkspace(ctx, t, seedDB, workspace)

		// Every project shares one title, so a caller ordering by title ties
		// every row; resource_id (the primary key) is distinct. There is no
		// index on the title column.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO project (resource_id, workspace, name)
			SELECT 'caller-proj-' || g, $1, 'shared title'
			FROM generate_series(1, $2::int) g`,
			workspace, total)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		orderKeys := []*store.OrderByKey{{Key: "project.name", SortOrder: store.ASC}}
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
		assertCompleteAndUnique(t, seen, total, "projects by title")
	})

	t.Run("database_by_project", func(t *testing.T) {
		stores, seedDB := freshStore(ctx, t)
		const workspace = "caller-db-ws"
		const project = "caller-db-p"
		const instance = "caller-db-i"
		const total = 240
		const pageSize = 7
		seedWorkspace(ctx, t, seedDB, workspace)
		seedProject(ctx, t, seedDB, project, workspace)
		seedInstance(ctx, t, seedDB, instance, workspace)

		// All databases share one project, so a caller ordering by project ties
		// every row; the primary key is (instance, name), distinct per row. The
		// list joins instance, so the page cannot be served as an ordered scan
		// of the project column alone.
		tx, err := seedDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO db (instance, name, project, environment)
			SELECT $1, 'db' || g, $2, 'prod'
			FROM generate_series(1, $3::int) g`,
			instance, project, total)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		orderKeys := []*store.OrderByKey{{Key: "db.project", SortOrder: store.ASC}}
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
		assertCompleteAndUnique(t, seen, total, "databases by project")
	})
}

// ===========================================================================
// Criterion: issue_cross_project_stability (fail_to_pass, basic_functionality)
//
// Paging the issues list across multiple projects — where issue identifiers
// collide between projects because they are assigned per project — returns
// every issue exactly once. This is the headline cross-project symptom. Seeds
// 3 projects each with many issues whose ids overlap across projects (ids start
// at 101 in every project, as the store's per-project counter does), with the
// ties created in a single transaction. Pre-fix (ORDER BY issue.id DESC alone)
// every id is tied three ways and rows cross page boundaries between reads.
// Passes for any fix whose ordering is total across the cross-project query.
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
	// a created_at and ids collide across projects exactly.
	tx, err := seedDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	for _, projectID := range projectIDs {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO project (resource_id, workspace, name) VALUES ($1, $2, $3)`,
			projectID, workspace, projectID)
		require.NoError(t, err)
		// ids 101..100+perProject, matching the store's per-project counter
		// floor so ids collide across projects the way real data does.
		_, err = tx.ExecContext(ctx, `
			INSERT INTO issue (id, project, creator, name, status, type, description)
			SELECT g, $1, 'users/xproj@example.com', 'issue ' || g, 'OPEN', 'DATABASE_CHANGE', ''
			FROM generate_series(101, $2::int) g`,
			projectID, 100+perProject)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	total := projectCount * perProject
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
		}
		return keys
	})
	assertCompleteAndUnique(t, seen, total, "issues cross-project")
}

// ===========================================================================
// Criterion: issue_comment_batch_order (fail_to_pass)
//
// A batch of issue comments created together is read back by the comment list
// in the order it was inserted, and that order survives reading the list page
// by page. Pre-fix, a batch shares one created_at value (now() at commit), so
// the default ordering ties every row of the batch; the comment table has no
// index on that column, so each page is served by a bounded top-N sort over
// fully-tied rows and the batch's rows land in different positions from one
// page request to the next — the paged read drops, repeats, and reorders the
// activity feed. A single unpaginated read does one full sort that happens to
// keep the rows in scan order, which hides the defect, so the read here is
// paged (the way the server's API layer reads) to surface it. Fails for a fix
// that only adds a read-side tiebreak without making the batch's sort key
// unique and insertion-ordered; passes for any fix that makes the batch read
// back in insertion order across pages. The batch is sized well above the
// page size and above Postgres's small-array insertion-sort threshold so the
// pre-fix ordering is reliably permuted rather than coincidentally preserved.
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

	// A realistic multi-field update produces several activity events in one
	// batch; this models that and additionally pads the batch past both the
	// page size and the small-array stable-sort threshold so a pre-fix all-tied
	// ordering is reliably scrambled once the read is split into pages.
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

	// Read the feed page by page with a fresh LIMIT/OFFSET per page and
	// concatenate in page order. A total ordering returns every comment exactly
	// once, in insertion order; a non-total one drops, repeats, or reorders rows
	// at the page boundaries, so the concatenation no longer equals want.
	uid := issueUID
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
		"a batch of issue comments must read back in insertion order across pages")
}

// ===========================================================================
// Criterion: issue_order_by_preserved (pass_to_pass)
//
// A caller-supplied explicit ordering is still honored: the fix only breaks
// ties and never overrides the requested lead sort. Calls ListIssues with an
// explicit order_by (create_time asc) over rows whose create_time is distinct,
// and asserts rows come back strictly in that requested order. This holds for
// both pre-fix and post-fix code (the caller's key leads in both), so it is a
// pass_to_pass guard: it fails only a "fix" that discards or overrides the
// caller's requested lead ordering.
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

	// Distinct, strictly increasing created_at per row so the requested
	// create_time ordering is unambiguous and independent of any tiebreak.
	// id ascends with created_at so an ascending create_time sort yields ids
	// 101,102,... in order.
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

	// The caller asked for create_time ascending; rows must come back with
	// strictly increasing created_at, lead-key-first, regardless of any
	// stability tiebreak appended after it.
	for i := 1; i < len(issues); i++ {
		require.Truef(t, !issues[i].CreatedAt.Before(issues[i-1].CreatedAt),
			"caller-requested create_time asc ordering violated at index %d: %v before %v",
			i, issues[i].CreatedAt, issues[i-1].CreatedAt)
	}
	// And since created_at is strictly increasing with id, the requested
	// ordering must yield ascending UIDs end to end.
	for i := 0; i < len(issues); i++ {
		require.Equalf(t, int64(101+i), issues[i].UID,
			"requested create_time asc ordering did not lead at index %d", i)
	}
}
