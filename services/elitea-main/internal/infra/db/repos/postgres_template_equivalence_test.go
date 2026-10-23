package repos

// What this file exists to catch.
//
// Every migrated test in this package now receives a database that PostgreSQL
// copied from a template (#425). The template is cheap. It is also a second
// source of truth, and a second source of truth can drift. A test database
// that no longer matches what a deployment applies is worse than a slow one:
// the suite stays green while production diverges.
//
// So this test builds one database the OLD way — CREATE DATABASE, then the
// seed, then migrate.Runner replays the whole ledgered corpus — and one
// database the NEW way, from the template. It then reads both catalogs and
// compares them line for line.
//
// The comparison covers the schema, not a row count: every schema, column with
// its type, nullability, default, identity and generation, every constraint
// definition, every index definition, every sequence with its parameters and
// current value, every view definition, every routine definition, every
// trigger definition, every user type with its enum labels, and every
// extension. It then compares the migration ledger itself: the scope, version,
// name, and SHA-256 checksum of every applied migration.
//
// Break the template path and this test fails. Point the template at a
// captured dump instead of the corpus and this test fails as soon as the dump
// lags one migration.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// schemaProbes read one catalog aspect each. Every probe must return one text
// column and must order its rows, so two runs give the same line order.
var schemaProbes = []struct {
	name  string
	query string
	// mustFind marks the aspects that this schema is known to populate. An
	// empty result there means the probe itself broke, not that the aspect is
	// absent. Views, routines and triggers are absent on purpose today, so
	// they stay comparable without a floor.
	mustFind bool
}{
	{
		name:     "schemas",
		mustFind: true,
		query: `
SELECT nspname
FROM pg_catalog.pg_namespace
WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
ORDER BY 1`,
	},
	{
		name:     "columns",
		mustFind: true,
		query: `
SELECT format('%s.%s | %s | %s | %s | notnull=%s | default=%s | identity=%s | generated=%s',
              n.nspname, c.relname, a.attnum, a.attname,
              pg_catalog.format_type(a.atttypid, a.atttypmod),
              a.attnotnull,
              coalesce(pg_catalog.pg_get_expr(d.adbin, d.adrelid), ''),
              a.attidentity, a.attgenerated)
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attnum > 0
  AND NOT a.attisdropped
  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
  AND n.nspname NOT LIKE 'pg\_%'
  AND n.nspname <> 'information_schema'
ORDER BY 1`,
	},
	{
		name:     "relations",
		mustFind: true,
		query: `
SELECT format('%s.%s | relkind=%s | persistence=%s | rowsecurity=%s',
              n.nspname, c.relname, c.relkind, c.relpersistence, c.relrowsecurity)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S')
  AND n.nspname NOT LIKE 'pg\_%'
  AND n.nspname <> 'information_schema'
ORDER BY 1`,
	},
	{
		name:     "constraints",
		mustFind: true,
		query: `
SELECT format('%s | %s | %s',
              con.conrelid::regclass::text, con.conname,
              pg_catalog.pg_get_constraintdef(con.oid))
FROM pg_catalog.pg_constraint con
JOIN pg_catalog.pg_namespace n ON n.oid = con.connamespace
WHERE n.nspname NOT LIKE 'pg\_%'
  AND n.nspname <> 'information_schema'
ORDER BY 1`,
	},
	{
		name:     "indexes",
		mustFind: true,
		query: `
SELECT format('%s.%s | %s | %s', schemaname, tablename, indexname, indexdef)
FROM pg_catalog.pg_indexes
WHERE schemaname NOT LIKE 'pg\_%' AND schemaname <> 'information_schema'
ORDER BY 1`,
	},
	{
		name:     "sequences",
		mustFind: true,
		query: `
SELECT format('%s.%s | start=%s | min=%s | max=%s | increment=%s | cycle=%s | last=%s',
              schemaname, sequencename, start_value, min_value, max_value,
              increment_by, cycle, coalesce(last_value::text, 'unset'))
FROM pg_catalog.pg_sequences
WHERE schemaname NOT LIKE 'pg\_%' AND schemaname <> 'information_schema'
ORDER BY 1`,
	},
	{
		name: "views",
		query: `
SELECT format('%s.%s | %s', schemaname, viewname, definition)
FROM pg_catalog.pg_views
WHERE schemaname NOT LIKE 'pg\_%' AND schemaname <> 'information_schema'
ORDER BY 1`,
	},
	{
		name: "routines",
		query: `
SELECT format('%s.%s | %s', n.nspname, p.proname, pg_catalog.pg_get_functiondef(p.oid))
FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
WHERE p.prokind IN ('f', 'p')
  AND n.nspname NOT LIKE 'pg\_%'
  AND n.nspname <> 'information_schema'
  AND NOT EXISTS (
      SELECT 1 FROM pg_catalog.pg_depend dep
      WHERE dep.objid = p.oid AND dep.deptype = 'e')
ORDER BY 1`,
	},
	{
		name: "triggers",
		query: `
SELECT format('%s | %s | %s',
              t.tgrelid::regclass::text, t.tgname,
              pg_catalog.pg_get_triggerdef(t.oid))
FROM pg_catalog.pg_trigger t
JOIN pg_catalog.pg_class c ON c.oid = t.tgrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE NOT t.tgisinternal
  AND n.nspname NOT LIKE 'pg\_%'
  AND n.nspname <> 'information_schema'
ORDER BY 1`,
	},
	{
		name:     "types",
		mustFind: true,
		query: `
SELECT format('%s.%s | typtype=%s | labels=%s',
              n.nspname, t.typname, t.typtype,
              coalesce((SELECT string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder)
                        FROM pg_catalog.pg_enum e WHERE e.enumtypid = t.oid), ''))
FROM pg_catalog.pg_type t
JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
WHERE n.nspname NOT LIKE 'pg\_%'
  AND n.nspname <> 'information_schema'
  AND t.typtype IN ('e', 'd', 'c', 'r')
ORDER BY 1`,
	},
	{
		name:     "extensions",
		mustFind: true,
		query: `
SELECT format('%s | %s | %s', extname, extversion, extnamespace::regnamespace::text)
FROM pg_catalog.pg_extension
ORDER BY 1`,
	},
	{
		name:     "ledger",
		mustFind: true,
		query: `
SELECT format('%s | %s | %04s | %s | %s',
              target_kind, target_id, version, name, encode(checksum, 'hex'))
FROM elitea_runtime.schema_migrations
ORDER BY target_kind, target_id, version`,
	},
	{
		name:     "row counts",
		mustFind: true,
		query: `
SELECT format('%s.%s | rows=%s', n.nspname, c.relname,
              (xpath('/row/c/text()',
                     query_to_xml(format('SELECT count(*) AS c FROM %I.%I',
                                         n.nspname, c.relname),
                                  false, true, '')))[1]::text::bigint)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'r'
  AND n.nspname NOT LIKE 'pg\_%'
  AND n.nspname <> 'information_schema'
ORDER BY 1`,
	},
}

func TestPostgresTemplateDatabaseMatchesMigrationReplay(t *testing.T) {
	databaseURL := postgresIntegrationURL()
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL 16-18 service-integration test", postgresIntegrationDatabaseURL)
	}
	if postgresIntegrationTemplate == "" {
		t.Fatalf("TestMain did not build the PostgreSQL integration template")
	}

	ctx, cancel := context.WithTimeout(context.Background(), postgresIntegrationDeadline)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", postgresIntegrationDatabaseURL, err)
	}
	adminConfig.MaxConns = 4
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	t.Cleanup(adminPool.Close)

	replayedPool := replayedEquivalencePool(t, adminPool, adminConfig)
	copiedPool := copiedEquivalencePool(t, adminPool, adminConfig)

	replayed := readSchemaSnapshot(t, replayedPool)
	copied := readSchemaSnapshot(t, copiedPool)

	if len(replayed) != len(schemaProbes) || len(copied) != len(schemaProbes) {
		t.Fatalf("snapshot sizes: replayed=%d copied=%d probes=%d",
			len(replayed), len(copied), len(schemaProbes))
	}
	for _, probe := range schemaProbes {
		replayedLines := replayed[probe.name]
		copiedLines := copied[probe.name]
		t.Logf("probe %q: %d line(s)", probe.name, len(replayedLines))
		if probe.mustFind && len(replayedLines) == 0 {
			t.Errorf("probe %q read nothing from the replayed database", probe.name)
			continue
		}
		if difference := firstDifference(replayedLines, copiedLines); difference != "" {
			t.Errorf("probe %q differs between the replayed database and the template copy:\n%s",
				probe.name, difference)
		}
	}

	// A comparison that reports "no difference" is only evidence when it can
	// report a difference. Change one column in the copy and read the same
	// probes again. Without this step a broken probe would read the same empty
	// result from both databases and look like proof of equivalence.
	t.Run("the comparison detects a schema change", func(t *testing.T) {
		mutateCtx, mutateCancel := context.WithTimeout(context.Background(), postgresIntegrationDeadline)
		defer mutateCancel()
		if _, err := copiedPool.Exec(mutateCtx,
			`ALTER TABLE elitea_runtime.execution_jobs ADD COLUMN template_drift_probe INTEGER`); err != nil {
			t.Fatalf("change the copied database: %v", err)
		}
		mutated := readSchemaSnapshot(t, copiedPool)
		if firstDifference(replayed["columns"], mutated["columns"]) == "" {
			t.Fatalf("the columns probe reports no difference after an added column")
		}
	})
}

// replayedEquivalencePool builds a database the way this package built one
// before #425: a plain CREATE DATABASE, then the seed, then a full replay.
func replayedEquivalencePool(t *testing.T, adminPool *pgxpool.Pool, adminConfig *pgxpool.Config) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), postgresIntegrationDeadline)
	defer cancel()

	name := fmt.Sprintf("elitea_eq_replay_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create replayed comparison database: %v", err)
	}
	pool := openEquivalencePool(t, adminPool, adminConfig, name)

	if _, err := pool.Exec(ctx, postgresIntegrationSeedSQL); err != nil {
		t.Fatalf("apply seed to the replayed comparison database: %v", err)
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("replay embedded shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, postgresIntegrationTenant); err != nil {
		t.Fatalf("replay embedded tenant migrations: %v", err)
	}
	// The template build installs the naive clock probe after the corpus
	// (dbtest.NaiveClockProbeSQL); the replay does the same, so the two
	// still differ only where the corpus would.
	if _, err := pool.Exec(ctx, dbtest.NaiveClockProbeSQL); err != nil {
		t.Fatalf("install the naive clock probe on the replayed database: %v", err)
	}
	return pool
}

// copiedEquivalencePool builds a database the way every migrated test now
// does: one CREATE DATABASE ... TEMPLATE.
func copiedEquivalencePool(t *testing.T, adminPool *pgxpool.Pool, adminConfig *pgxpool.Config) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), postgresIntegrationDeadline)
	defer cancel()

	name := fmt.Sprintf("elitea_eq_copy_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := dbtest.CreateFromTemplate(ctx, adminPool, postgresIntegrationTemplate, name); err != nil {
		t.Fatalf("copy the template into the comparison database: %v", err)
	}
	return openEquivalencePool(t, adminPool, adminConfig, name)
}

func openEquivalencePool(t *testing.T, adminPool *pgxpool.Pool, adminConfig *pgxpool.Config, name string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), postgresIntegrationDeadline)
	defer cancel()

	config := adminConfig.Copy()
	config.ConnConfig.Database = name
	config.MinConns = 0
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open comparison database %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), postgresIntegrationDeadline)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx,
			"DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop comparison database %s: %v", name, err)
		}
	})
	return pool
}

func readSchemaSnapshot(t *testing.T, pool *pgxpool.Pool) map[string][]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), postgresIntegrationDeadline)
	defer cancel()

	snapshot := make(map[string][]string, len(schemaProbes))
	for _, probe := range schemaProbes {
		rows, err := pool.Query(ctx, probe.query)
		if err != nil {
			t.Fatalf("run probe %q: %v", probe.name, err)
		}
		var lines []string
		for rows.Next() {
			var line *string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatalf("scan probe %q: %v", probe.name, err)
			}
			if line == nil {
				lines = append(lines, "<null>")
				continue
			}
			lines = append(lines, *line)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("read probe %q: %v", probe.name, err)
		}
		snapshot[probe.name] = lines
	}
	return snapshot
}

// firstDifference returns a readable report of the first divergence, or an
// empty string when the two line lists are identical.
func firstDifference(replayed, copied []string) string {
	limit := len(replayed)
	if len(copied) < limit {
		limit = len(copied)
	}
	for index := 0; index < limit; index++ {
		if replayed[index] != copied[index] {
			return fmt.Sprintf("line %d\n  replayed: %s\n  copied:   %s",
				index+1, replayed[index], copied[index])
		}
	}
	if len(replayed) == len(copied) {
		return ""
	}
	if len(replayed) > len(copied) {
		return fmt.Sprintf("the template copy is missing %d line(s), first missing:\n  %s",
			len(replayed)-len(copied), strings.Join(replayed[limit:min(limit+3, len(replayed))], "\n  "))
	}
	return fmt.Sprintf("the template copy has %d extra line(s), first extra:\n  %s",
		len(copied)-len(replayed), strings.Join(copied[limit:min(limit+3, len(copied))], "\n  "))
}
