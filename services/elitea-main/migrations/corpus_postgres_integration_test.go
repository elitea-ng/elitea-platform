package migrations_test

// What this file exists to catch, and why nothing else could.
//
// Every other PostgreSQL integration test in this service builds the schema it
// needs by hand — repos/configuration_validation_postgres_integration_test.go
// CREATEs its own `centry.notifications`, repos/toolkits_postgres_integration_test.go
// CREATEs its own `p_1.elitea_tools`. Both of those hand-written shapes are
// transcribed from internal/db/schema/*.sql, the sqlc COMPILER projections. So
// the generated queries are only ever executed against the schema sqlc
// type-checked them against, and never against the schema the migration corpus
// actually produces. When the two disagree the whole suite stays green and the
// server fails at runtime — twice, measured on the standalone stack:
//
//   * `column "updated_at" does not exist` (42703) reading a toolkit, which made
//     the #93 index-start route answer 500 before it could do anything at all.
//   * `there is no unique or exclusion constraint matching the ON CONFLICT
//     specification` (42P10) persisting an index run's TERMINAL output, which
//     silently swallowed `index.ingest.completed` and left the browser's
//     EventSource waiting on a finished run forever.
//
// So this test applies the REAL corpus — 001_initial.sql, then every
// migrations/shared and migrations/tenant file — and then executes the two
// statements themselves. It asserts behaviour rather than shape: the toolkit
// SELECT must return the row it was given, and the notification INSERT must
// actually deduplicate on a repeat, which a merely-parsable ON CONFLICT would
// not do.
//
// Revert either 0124 or 0065 and the corresponding subtest fails.
//
// The third subtest (issue #306) is the same shape one level up: the gateway
// budget tables were declared by a migration corpus that only the dev bootstrap
// flag ever ran, while the corpus Helm runs — this one — had never heard of
// them. Every existing test of that schema called db.GatewayMigrationSQL()
// explicitly, so all of them passed against a schema production never built.
// Delete shared/0067 and the gateway subtest fails; no other test would.

import (
	"context"
	"errors"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	migrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
)

const (
	databaseURLEnv    = "ELITEA_TEST_DATABASE_URL"
	bootstrapSchemaSQ = "../internal/infra/db/migrations/001_initial.sql"
)

func TestMigrationCorpusSupportsGeneratedQueries(t *testing.T) {
	pool := newMigratedPool(t)

	// The generated query, character for character from
	// internal/db/sqlcgen/toolkits.sql.go:13-27. Copied rather than called
	// through the repository so that a change to either one shows up here as a
	// diff rather than as a silent divergence.
	const getCurrentToolkit = `
SELECT id, created_at, updated_at, type, name, description,
       settings, author_id, shared_owner_id, shared_id, meta
FROM elitea_tools
WHERE id = $1::integer
LIMIT 1`

	t.Run("toolkit read", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := pool.Exec(ctx, `
INSERT INTO p_1.elitea_tools (id, name, type, description, owner_id, author_id, meta, settings, created_at)
VALUES (4242, 'corpus-probe', 'artifact', 'migration corpus probe', 1, 1, '{}'::jsonb, '{}'::jsonb,
        now() AT TIME ZONE 'UTC')`,
		); err != nil {
			t.Fatalf("seed toolkit row: %v", err)
		}

		// The query names `elitea_tools` unqualified because it only ever runs
		// inside a project transaction whose local search_path is p_<id>
		// (sqlcgen/toolkits.sql.go:30-31). SET LOCAL needs that transaction to be
		// explicit — in an implicit one the setting is discarded before the
		// SELECT.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SET LOCAL search_path TO p_1"); err != nil {
			t.Fatalf("set search_path: %v", err)
		}

		var (
			id                      int32
			name                    *string
			createdAt               time.Time
			updatedAt               *time.Time
			toolkitType             string
			description             *string
			settings, meta          []byte
			authorID                int32
			sharedOwnerID, sharedID *int32
		)
		scanErr := tx.QueryRow(ctx, getCurrentToolkit, int32(4242)).Scan(
			&id, &createdAt, &updatedAt, &toolkitType, &name, &description,
			&settings, &authorID, &sharedOwnerID, &sharedID, &meta,
		)
		if scanErr != nil {
			t.Fatalf("the generated toolkit query does not run against the migrated schema: %v", scanErr)
		}
		if id != 4242 || name == nil || *name != "corpus-probe" || toolkitType != "artifact" {
			t.Fatalf("toolkit row read back wrong: id=%d name=%v type=%q", id, name, toolkitType)
		}
	})

	// The tail of InsertCurrentIndexTerminalNotification
	// (internal/db/queries/runtime_index_ingest.sql:265-295). The surrounding CTE
	// needs a whole execution graph; the clause that broke is this one, and it
	// breaks on its own.
	t.Run("notification upsert deduplicates", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		const insertNotification = `
INSERT INTO centry.notifications (uuid, is_seen, project_id, user_id, meta, event_type, created_at, updated_at)
VALUES ($1::text::uuid, FALSE, 1, 1, '{}'::jsonb, 'index_data_changed',
        now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')
ON CONFLICT (uuid) DO NOTHING`
		const notificationUUID = "00000000-0000-4000-8000-000000000042"

		for attempt := 1; attempt <= 2; attempt++ {
			if _, err := pool.Exec(ctx, insertNotification, notificationUUID); err != nil {
				t.Fatalf("attempt %d: ON CONFLICT (uuid) is not supported by the migrated schema: %v", attempt, err)
			}
		}

		var rows int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM centry.notifications WHERE uuid = $1::text::uuid",
			notificationUUID,
		).Scan(&rows); err != nil {
			t.Fatalf("count notifications: %v", err)
		}
		// 2 would mean the statement parsed against SOME unique index while the
		// uuid column itself stayed duplicable — which is exactly the state a
		// shape-only assertion would have accepted.
		if rows != 1 {
			t.Fatalf("uuid is not unique: the second insert produced %d rows total, want 1", rows)
		}
	})
}

// TestMigrationCorpusBuildsGatewayBudgetSchema proves the gateway.* tables
// exist because the LEDGERED corpus made them — the corpus deploy/helm/
// elitea-main/templates/migrate-job.yaml runs through elitea-migrate — and not
// because a fixture hand-created them. newMigratedPool applies nothing but
// 001_initial.sql and the embedded histories, so if shared/0067 is missing or
// inert, these statements fail with 42P01/42703 exactly as they would in a
// Helm-deployed cluster.
//
// It asserts BEHAVIOUR, not shape. Both statements below are copied verbatim
// from the services that run them (which are separate Go modules — the gateway
// is outside go.work — so they cannot be called through). A merely-parsable
// ON CONFLICT would satisfy a shape check; these subtests only pass if the
// unique constraint the money path conflicts on actually exists and actually
// deduplicates.
func TestMigrationCorpusBuildsGatewayBudgetSchema(t *testing.T) {
	pool := newMigratedPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// elitea-llm-gateway/internal/failmode/store.go snapshotSQL, the read the
	// gateway performs on EVERY /llm call before dispatching to a provider.
	// Trimmed to the FROM/JOIN/WHERE that names the tables; the projection is
	// exercised by the accumulate subtest below.
	const budgetSnapshot = `
SELECT pb.is_unlimited,
       COALESCE((pb.hard_limit_usd * 1000000000::numeric)::bigint, 0)    AS hard_limit_nano,
       COALESCE((acc.accumulated_cost * 1000000000::numeric)::bigint, 0) AS accumulated_nano,
       pb.soft_alert_pct,
       pb.nats_fail_mode,
       acc.id IS NOT NULL AS acc_found
FROM gateway.project_budget pb
LEFT JOIN gateway.llm_budget_accumulators acc
    ON acc.scope = $2 AND acc.scope_id = $3 AND acc.period_start = to_timestamp($4)
WHERE pb.project_id = $1`

	// elitea-scheduler/internal/budgetwriteback/store.go upsertSQL, character
	// for character apart from the nanoUSDPerUSD constant being inlined.
	const writeBackUpsert = `
INSERT INTO gateway.llm_budget_accumulators AS acc
		(project_id, org_id, scope, scope_id, period_start, period_end,
		 accumulated_cost, outage_mode, reconciled, last_updated)
	VALUES ($1, $2, $3, $4, to_timestamp($5), to_timestamp($6),
		$7::numeric / 1000000000, false, false, now())
	ON CONFLICT (scope, scope_id, period_start) DO UPDATE SET
		accumulated_cost = acc.accumulated_cost + EXCLUDED.accumulated_cost,
		last_updated = now()
	WHERE NOT (acc.outage_mode AND NOT acc.reconciled)`

	// elitea-scheduler/internal/budgetwriteback/store.go dedupSQL.
	const dedup = `INSERT INTO gateway.processed_event_ids (event_id) VALUES ($1)
	ON CONFLICT DO NOTHING RETURNING event_id`

	const (
		projectID   = 8306
		scope       = "project"
		scopeID     = "8306"
		periodStart = 1767225600 // 2026-01-01T00:00:00Z
		periodEnd   = 1769904000 // 2026-02-01T00:00:00Z
	)

	if _, err := pool.Exec(ctx, `
INSERT INTO gateway.project_budget (project_id, hard_limit_usd, is_unlimited, enabled, soft_alert_pct, nats_fail_mode)
VALUES ($1, 25.00, false, true, 80, 'fail_closed')`, projectID); err != nil {
		t.Fatalf("seed project budget: %v", err)
	}

	t.Run("gateway admission read", func(t *testing.T) {
		var (
			isUnlimited    bool
			hardLimitNano  int64
			accumulatedNan int64
			softAlertPct   int16
			failMode       *string
			accFound       bool
		)
		if err := pool.QueryRow(ctx, budgetSnapshot, projectID, scope, scopeID, periodStart).Scan(
			&isUnlimited, &hardLimitNano, &accumulatedNan, &softAlertPct, &failMode, &accFound,
		); err != nil {
			t.Fatalf("gateway budget snapshot: %v", err)
		}
		// 25.00 USD in nano-USD. A wrong NUMERIC precision on hard_limit_usd
		// would round this, which is the denomination bug the schema exists to
		// prevent — so assert the value, not merely that the read succeeded.
		if hardLimitNano != 25_000_000_000 {
			t.Errorf("hard_limit_nano = %d, want 25000000000", hardLimitNano)
		}
		if accFound {
			t.Error("acc_found = true with no accumulator row seeded")
		}
		if failMode == nil || *failMode != "fail_closed" {
			t.Errorf("nats_fail_mode = %v, want fail_closed", failMode)
		}
	})

	t.Run("write-back accumulates on the conflict target", func(t *testing.T) {
		// Two deltas for the SAME (scope, scope_id, period_start). If the
		// UNIQUE constraint is absent the second is a second ROW rather than a
		// conflict — spend silently splits in two and the gateway reads half of
		// it. Without the constraint Postgres would reject the statement with
		// 42P10 instead, which is the failure this catches on a fresh database.
		for _, deltaNano := range []int64{1_500_000_000, 2_500_000_000} {
			if _, err := pool.Exec(ctx, writeBackUpsert,
				projectID, nil, scope, scopeID, periodStart, periodEnd, deltaNano,
			); err != nil {
				t.Fatalf("write-back upsert: %v", err)
			}
		}

		var rows int
		var accumulated string
		if err := pool.QueryRow(ctx, `
SELECT count(*), COALESCE(max(accumulated_cost)::text, '')
FROM gateway.llm_budget_accumulators
WHERE scope = $1 AND scope_id = $2 AND period_start = to_timestamp($3)`,
			scope, scopeID, periodStart,
		).Scan(&rows, &accumulated); err != nil {
			t.Fatalf("read accumulator: %v", err)
		}
		if rows != 1 {
			t.Fatalf("accumulator rows = %d, want 1 — the UPSERT did not conflict, so spend is split across rows", rows)
		}
		// 1.5 + 2.5 USD. A summed 4.00 proves the DO UPDATE ran; 2.50 would
		// mean the second write replaced rather than accumulated.
		if accumulated != "4.00000000" {
			t.Errorf("accumulated_cost = %q, want %q", accumulated, "4.00000000")
		}
	})

	t.Run("dedup ledger actually deduplicates", func(t *testing.T) {
		const eventID = "6f1d0a3e-1c22-4f7a-9a41-3f0c5f7a1b20"
		var returned string
		if err := pool.QueryRow(ctx, dedup, eventID).Scan(&returned); err != nil {
			t.Fatalf("first dedup insert: %v", err)
		}
		// A redelivery MUST return no row. If event_id were not the primary key
		// the insert would succeed again and the consumer would double-apply
		// the delta onto the accumulator.
		err := pool.QueryRow(ctx, dedup, eventID).Scan(&returned)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("redelivery err = %v, want pgx.ErrNoRows — the event was not deduplicated", err)
		}
	})

	t.Run("re-applying is inert", func(t *testing.T) {
		// Every dev and dump-loaded database already carries these tables from
		// the deleted unledgered corpus and has no ledger row for 0067, so
		// elitea-migrate WILL apply this file over an existing schema holding
		// live rows. That must neither error nor disturb the data — the whole
		// reason every statement in it is guarded. Re-executing the real file
		// here is that exact sequence.
		sql, err := migrations.Files.ReadFile("shared/0067_gateway_budget_schema.sql")
		if err != nil {
			t.Fatalf("read 0067: %v", err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("re-apply 0067 over an existing schema: %v", err)
		}
		// The accumulated spend written by the subtest above must survive.
		var accumulated string
		if err := pool.QueryRow(ctx, `
SELECT accumulated_cost::text FROM gateway.llm_budget_accumulators
WHERE scope = $1 AND scope_id = $2 AND period_start = to_timestamp($3)`,
			scope, scopeID, periodStart,
		).Scan(&accumulated); err != nil {
			t.Fatalf("read accumulator after re-apply: %v", err)
		}
		if accumulated != "4.00000000" {
			t.Errorf("accumulated_cost = %q after re-apply, want %q", accumulated, "4.00000000")
		}
	})

	t.Run("remaining gateway tables are present", func(t *testing.T) {
		// The three tables no statement above touches. to_regclass returns NULL
		// rather than raising when the relation is absent, so one query covers
		// all of them and names the missing one.
		for _, table := range []string{
			"gateway.llm_credentials",
			"gateway.gateway_models",
			"gateway.governance_config",
			"gateway.user_budget",
		} {
			var oid *string
			if err := pool.QueryRow(ctx, "SELECT to_regclass($1)::text", table).Scan(&oid); err != nil {
				t.Fatalf("to_regclass(%s): %v", table, err)
			}
			if oid == nil {
				t.Errorf("%s does not exist after the ledgered corpus applied", table)
			}
		}
	})
}

// newMigratedPool builds a throwaway database holding ONLY what a real
// deployment of this repository holds: the bootstrap schema plus the embedded
// migration corpus. Nothing is created by hand.
func newMigratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(databaseURLEnv))
	if databaseURL == "" && os.Getenv("ELITEA_TEST_USE_SERVICE_DATABASE_URL") == "1" {
		databaseURL = testpg.DSN(os.Getenv("DATABASE_URL"))
	}
	if databaseURL == "" {
		t.Skipf("set %s to run the migration-corpus integration test", databaseURLEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_corpus_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", databaseURLEnv, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})

	bootstrap, err := os.ReadFile(bootstrapSchemaSQ)
	if err != nil {
		t.Fatalf("read bootstrap schema: %v", err)
	}
	if _, err := pool.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatalf("apply bootstrap schema: %v", err)
	}

	runner := migrate.New(pool, migrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("apply tenant migrations to p_1: %v", err)
	}
	// The naive-clock probe the dbtest template installs, after the last
	// schema statement: a writer that stores the session-local clock in a
	// `timestamp` WITHOUT time zone column fails instead of passing unread.
	if err := dbtest.InstallNaiveClockProbe(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// TestMigrationCorpusBuildsUsageLedgerAndAlertConfig is the fourth subject of
// this file and the same shape as the third: shared/0084 adds the per-request
// usage ledger the /llm billing path writes on every call (issue #320), relaxes
// the two soft-alert thresholds so a platform default can reach a project that
// authored none (#322), and seeds the global alert row the gateway's snapshot
// query joins.
//
// It runs against the corpus applied to an EMPTY database, which is the only
// arrangement that can fail: every dev and dump-loaded database already carries
// gateway.governance_config, so a migration that created nothing would look
// correct everywhere except a fresh Helm install.
//
// Behaviour, not shape. The ledger insert is executed twice under one event id,
// so a missing PRIMARY KEY fails here rather than duplicating a request in the
// per-model table; and a NULL threshold is stored, which 0067's NOT NULL
// refuses.
func TestMigrationCorpusBuildsUsageLedgerAndAlertConfig(t *testing.T) {
	pool := newMigratedPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// services/elitea-scheduler/internal/budgetwriteback/store.go usageEventSQL
	// and services/elitea-llm-gateway/internal/failmode/store.go
	// usageEventInsertSQL, which are the same statement, with the nanoUSDPerUSD
	// constant inlined.
	const usageInsert = `INSERT INTO gateway.llm_usage_events
		(event_id, project_id, user_id, provider, model,
		 prompt_tokens, completion_tokens, cost_usd, period_start, period_end)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric / 1000000000,
		to_timestamp($9), to_timestamp($10))
	ON CONFLICT (event_id) DO NOTHING`

	const (
		eventID     = "9c4b1f2e-0000-4000-8000-00000000abcd"
		projectID   = 8407
		userID      = 55
		periodStart = 1767225600 // 2026-01-01T00:00:00Z
		periodEnd   = 1769904000 // 2026-02-01T00:00:00Z
	)

	t.Run("usage ledger accepts a billed request", func(t *testing.T) {
		if _, err := pool.Exec(ctx, usageInsert, eventID, projectID, userID,
			"openai", "gpt-4o", 100, 200, 1_500_000_000, periodStart, periodEnd); err != nil {
			t.Fatalf("write a usage-ledger row: %v", err)
		}

		// The nano-USD → USD division must land exactly, and total_tokens must
		// be computed by Postgres rather than supplied.
		var costUSD string
		var totalTokens int64
		if err := pool.QueryRow(ctx,
			`SELECT cost_usd::text, total_tokens FROM gateway.llm_usage_events WHERE event_id = $1`,
			eventID).Scan(&costUSD, &totalTokens); err != nil {
			t.Fatalf("read the usage-ledger row back: %v", err)
		}
		if costUSD != "1.50000000" {
			t.Fatalf("cost_usd = %s, want 1.50000000 — the nano-USD conversion is inexact", costUSD)
		}
		if totalTokens != 300 {
			t.Fatalf("total_tokens = %d, want 300 computed from its own parts", totalTokens)
		}
	})

	t.Run("a redelivered event does not duplicate the row", func(t *testing.T) {
		// The gateway's outage path and the scheduler's consumer can both write
		// the same event. Without the PRIMARY KEY this second insert succeeds
		// and every per-model figure for that request doubles.
		if _, err := pool.Exec(ctx, usageInsert, eventID, projectID, userID,
			"openai", "gpt-4o", 100, 200, 1_500_000_000, periodStart, periodEnd); err != nil {
			t.Fatalf("re-apply the same usage event: %v", err)
		}
		var rows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM gateway.llm_usage_events WHERE event_id = $1`, eventID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatalf("the ledger holds %d rows for one event id, want 1", rows)
		}
	})

	t.Run("a call with no member is stored as NULL", func(t *testing.T) {
		const anonEvent = "9c4b1f2e-0000-4000-8000-00000000bcde"
		if _, err := pool.Exec(ctx, usageInsert, anonEvent, projectID, nil,
			"openai", "gpt-4o", 1, 2, 1, periodStart, periodEnd); err != nil {
			t.Fatalf("write a usage event with no member: %v", err)
		}
		var userIDIsNull bool
		if err := pool.QueryRow(ctx,
			`SELECT user_id IS NULL FROM gateway.llm_usage_events WHERE event_id = $1`,
			anonEvent).Scan(&userIDIsNull); err != nil {
			t.Fatal(err)
		}
		if !userIDIsNull {
			t.Fatal("a call with no member was stored as a member; the per-member views would attribute it to a user")
		}
	})

	t.Run("the retention prune touches only the ledger", func(t *testing.T) {
		// services/elitea-scheduler/internal/budgetwriteback/store.go
		// pruneUsageEventsSQL, with the interval and the batch bound supplied.
		const prune = `DELETE FROM gateway.llm_usage_events
			WHERE event_id IN (
				SELECT event_id FROM gateway.llm_usage_events
				WHERE occurred_at < now() - make_interval(secs => $1)
				LIMIT $2
			)`
		const oldEvent = "9c4b1f2e-0000-4000-8000-00000000cdef"
		if _, err := pool.Exec(ctx, usageInsert, oldEvent, projectID, userID,
			"openai", "gpt-4o", 1, 1, 1, periodStart, periodEnd); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE gateway.llm_usage_events SET occurred_at = now() - interval '500 days' WHERE event_id = $1`,
			oldEvent); err != nil {
			t.Fatal(err)
		}

		tag, err := pool.Exec(ctx, prune, (400 * 24 * time.Hour).Seconds(), 5000)
		if err != nil {
			t.Fatalf("run the retention prune: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("the prune removed %d rows, want the 1 past retention", tag.RowsAffected())
		}
		// The rows inside the window survive.
		var remaining int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM gateway.llm_usage_events WHERE project_id = $1`, projectID).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 2 {
			t.Fatalf("%d rows remain, want the 2 inside the retention window", remaining)
		}
	})

	t.Run("soft-alert thresholds accept NULL", func(t *testing.T) {
		// Under 0067 both columns are NOT NULL DEFAULT 80, so "this scope
		// authored no threshold" cannot be stored and the platform default
		// documented on PUT /admin/gateway/budget-alerts can never apply.
		if _, err := pool.Exec(ctx, `
INSERT INTO gateway.project_budget (project_id, hard_limit_usd, is_unlimited, enabled, soft_alert_pct)
VALUES ($1, 10.00, false, true, NULL)`, projectID); err != nil {
			t.Fatalf("store a project budget with no authored threshold: %v", err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO gateway.user_budget (project_id, user_id, hard_limit_usd, enabled, soft_alert_pct)
VALUES ($1, $2, 5.00, true, NULL)`, projectID, userID); err != nil {
			t.Fatalf("store a member budget with no authored threshold: %v", err)
		}
	})

	t.Run("the global alert row is seeded", func(t *testing.T) {
		// The three key values are the contract between the elitea-main writer
		// (internal/api/gateway/budget_alerts.go) and the gateway reader
		// (failmode/store.go globalAlertConfigSQL). A GET before the first PUT
		// must answer with the values the old in-process store returned.
		var enabled bool
		var thresholdPct int
		err := pool.QueryRow(ctx, `
SELECT (data->>'enabled')::boolean, (data->>'threshold_pct')::smallint
FROM gateway.governance_config
WHERE section = 'governance' AND type = 'budget_alert' AND name = 'global' AND enabled`).
			Scan(&enabled, &thresholdPct)
		if err != nil {
			t.Fatalf("the global budget-alert row was not seeded: %v", err)
		}
		if !enabled || thresholdPct != 80 {
			t.Fatalf("seeded config = (enabled=%v, threshold=%d), want (true, 80)", enabled, thresholdPct)
		}
	})

	t.Run("the per-member admission read resolves a member cap", func(t *testing.T) {
		// services/elitea-llm-gateway/internal/failmode/store.go
		// userSnapshotSQL, trimmed to the join and the derived is_unlimited.
		// The gateway is outside go.work so this cannot import the constant;
		// executing it here is what stops the two from drifting silently.
		const memberSnapshot = `
SELECT (ub.hard_limit_usd IS NULL OR NOT ub.enabled)                AS is_unlimited,
       COALESCE((ub.hard_limit_usd * 1000000000::numeric)::bigint, 0) AS hard_limit_nano,
       COALESCE(ub.soft_alert_pct, ga.threshold_pct, 80)            AS soft_alert_pct,
       pb.nats_fail_mode
FROM gateway.user_budget ub
LEFT JOIN gateway.project_budget pb ON pb.project_id = ub.project_id
LEFT JOIN (
    SELECT (data->>'enabled')::boolean        AS alerts_enabled,
           (data->>'threshold_pct')::smallint AS threshold_pct
    FROM gateway.governance_config
    WHERE section = 'governance' AND type = 'budget_alert' AND name = 'global' AND enabled
) ga ON true
WHERE ub.project_id = $1 AND ub.user_id = $2`

		var isUnlimited bool
		var hardLimitNano int64
		var softAlertPct int
		var failMode *string
		if err := pool.QueryRow(ctx, memberSnapshot, projectID, userID).
			Scan(&isUnlimited, &hardLimitNano, &softAlertPct, &failMode); err != nil {
			t.Fatalf("the gateway's per-member admission read failed: %v", err)
		}
		if isUnlimited {
			t.Fatal("a member with an enabled 5.00 cap resolved as unlimited; the cap would never bite")
		}
		if hardLimitNano != 5_000_000_000 {
			t.Fatalf("hard_limit_nano = %d, want 5e9 for a 5.00 USD cap", hardLimitNano)
		}
		// The member authored no threshold, so the seeded global default applies.
		if softAlertPct != 80 {
			t.Fatalf("soft_alert_pct = %d, want the global default 80", softAlertPct)
		}
	})
}
