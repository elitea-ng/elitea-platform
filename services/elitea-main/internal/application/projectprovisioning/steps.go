package projectprovisioning

// The step pipeline, and the port/drop disposition issue #333 asks for.
//
// pylon's sequence (legacy/plugins/projects/utils/project_steps.py:319-332) is:
//
//     project_model → project_schema → project_permissions → system_user →
//     system_token → project_secrets → minio_buckets → rabbit_vhost →
//     influx_databases → project_admin
//
// PORTED, in the same relative order:
//
//   project_model        The centry.project row, its project_quota row and its
//                        statistic row. /projects/quota and /projects/statistics
//                        (#246) read those two tables and answer 404 without
//                        them, so a project provisioned without this step would
//                        report "no such project" on its own settings page.
//   project_schema       create_tenant_schema('p_<id>') then the tenant corpus.
//                        pylon runs CreateSchema + create_all inside its one
//                        step; this is the same two halves.
//   project_permissions  The per-project roles. pylon writes ROLES ONLY and no
//                        auth_core__project_role_permission rows, so permission
//                        resolution falls back to the central default-mode
//                        grants by role NAME. legacyrbac.projectPermissions()
//                        implements exactly that fallback, and it is suppressed
//                        for any project that carries per-project rows — so
//                        seeding permissions here would silently CUT a new
//                        project off from every grant migration 0062-0069 adds.
//   system_user          The `system_user_<id>@centry.user` identity.
//   system_token         Its PAT, named 'api'.
//   project_secrets      The project's rows in centry.secrets_key and
//                        centry.secrets_data — an EMPTY vault under a fresh
//                        Fernet key. See createProjectSecrets for why this step
//                        was first dropped and then restored (#373).
//   artifact_buckets     The project's `reports` and `tasks` system buckets.
//   project_pgvector     The project's PgVector role/database, its vault
//                        material, and the `vectorstorage` configuration row an
//                        index run resolves. See the note below: pylon does
//                        this from an EVENT rather than from a step.
//   project_admin        Membership for the administrators of the project.
//
// DROPPED, with the reason:
//
//   rabbit_vhost         AGENTS.md forbids the Arbiter transport, and this
//                        platform's durable worker transport is NATS JetStream.
//                        pylon itself skips this step unless ARBITER_RUNTIME is
//                        "rabbitmq", so dropping it is parity with a
//                        non-Arbiter deployment rather than a behaviour
//                        change. This is the drop the issue asked to confirm.
//   influx_databases     No component of this platform reads or writes InfluxDB.
//                        pylon gates the step on CENTRY_USE_INFLUX, which is
//                        false by default, so it is already inert on a stock
//                        deployment.
//
// PORTED AS A STEP, from an event:
//
//   project_created      pylon fires this event AFTER it commits
//                        create_success (project_steps.py:369), and two
//                        listeners consume it. One creates the project's
//                        pgvector credentials
//                        (elitea_core/events/vectorstore.py:9); the other
//                        creates a LiteLLM team and key.
//
//                        The pgvector half is reproduced, as the
//                        project_pgvector STEP above rather than as an event
//                        (#371). An earlier revision of this comment claimed
//                        that "pgvector credentials are resolved through a
//                        different mechanism". No such mechanism ran: the three
//                        components that would have implemented it —
//                        runtimecomposition's database provisioner and vault
//                        material repository, and repos'
//                        CurrentProjectPgvectorConfigurationsRepository — had
//                        no non-test caller at all, so nothing ever wrote the
//                        row. A project could be created and then could not
//                        index, because configuration expansion resolves
//                        `pgvector_configuration` by elitea_title in the
//                        project's own tenant and finds nothing.
//
//                        Making it a step rather than an event is a deliberate
//                        correction, not a shortcut. pylon's listener runs
//                        after create_success is already true, so a listener
//                        failure leaves a project that every reader treats as
//                        complete and that no index run can use. A step cannot
//                        end that way: it is compensated with the rest.
//
//                        The LiteLLM half stays unreproduced. This platform's
//                        LLM path is Bifrost, and nothing consumes a LiteLLM
//                        team or key.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Step names, as they appear in a Result's status records.
const (
	StepProjectModel       = "project_model"
	StepProjectSchema      = "project_schema"
	StepProjectPermissions = "project_permissions"
	StepSystemUser         = "system_user"
	StepSystemToken        = "system_token"
	StepProjectSecrets     = "project_secrets"
	StepArtifactBuckets    = "artifact_buckets"
	StepProjectPgvector    = "project_pgvector"
	StepProjectAdmin       = "project_admin"
)

// systemProjectRole is the project role the per-project system identity holds.
//
// pylon's `auth.get_roles(mode='default')` returns `system` alongside admin/
// editor/viewer, so its `admin_add_role` creates a `system` project role as a
// side effect. This repository names the role explicitly here instead.
//
// THE CENTRAL ROLE OF THE SAME NAME MATTERS, AND IT USED NOT TO EXIST.
// legacyrbac resolves a project role that carries no per-project grant rows
// through the CENTRAL default-mode role of the same name
// (internal/infra/legacyrbac/postgres.go). 001_initial.sql seeded admin, editor
// and viewer only. This role therefore matched nothing. The project-system PAT
// that a scheduled execution runs on resolved the EMPTY set. Every worker
// callback answered 403, while the same run started by a human succeeded.
//
// migrations/shared/0089_central_system_role.sql seeds the central role and
// grants it the worker callback surface. Read that file before you change the
// name here: the two have to agree, and nothing checks the spelling for you
// except migrations/administration_secret_and_system_role_grants_postgres_integration_test.go.
//
// It must exist. queries/auth_pat.sql's GetActiveProjectSystemPAT joins through
// auth_core__project_user_role, so a system user with no project role assignment
// yields no token at all — the shape
// authsvc/project_system_pat_issuer_postgres_integration_test.go pins as
// "missing-system-role".
const systemProjectRole = "system"

// systemTokenName is the PAT name GetActiveProjectSystemPAT matches on
// (`token.name = 'api'`). pylon's SystemToken step uses the same literal.
const systemTokenName = "api"

type step struct {
	name   string
	create func(ctx context.Context, p *Provisioner, state *provisionState) error
	remove func(ctx context.Context, p *Provisioner, state *provisionState) error
}

func createSteps() []step {
	return []step{
		{name: StepProjectModel, create: createProjectModel, remove: removeProjectModel},
		{name: StepProjectSchema, create: createProjectSchema, remove: removeProjectSchema},
		{name: StepProjectPermissions, create: createProjectPermissions, remove: removeProjectPermissions},
		{name: StepSystemUser, create: createSystemUser, remove: removeSystemUser},
		{name: StepSystemToken, create: createSystemToken, remove: removeSystemToken},
		// project_secrets sits where pylon puts it: after system_token and
		// before the bucket step. The position is load-bearing in the OTHER
		// direction — compensation and Deprovision both walk this list in
		// reverse — but nothing in the vault depends on a later step, and no
		// later step reads the vault, so the placement is parity rather than a
		// constraint.
		{name: StepProjectSecrets, create: createProjectSecrets, remove: removeProjectSecrets},
		{name: StepArtifactBuckets, create: createArtifactBuckets, remove: removeArtifactBuckets},
		{name: StepProjectPgvector, create: createProjectVectorStore, remove: removeProjectVectorStore},
		{name: StepProjectAdmin, create: createProjectAdmin, remove: removeProjectAdmin},
	}
}

/* ── project_model ─────────────────────────────────────────────────────── */

// createProjectModel writes the project row and its two companion rows.
//
// pylon commits after each of the three inserts. This does all three in ONE
// transaction, which is a strict improvement available here and not there: the
// three rows are in the same database, so either the project exists with its
// quota and statistics or it does not exist at all. There is no state in which
// /projects/quota answers 404 for a project the list endpoint returns.
func createProjectModel(ctx context.Context, p *Provisioner, state *provisionState) error {
	transaction, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = transaction.Rollback(context.WithoutCancel(ctx)) }()

	plugins := state.request.Plugins
	if plugins == nil {
		// TEXT[] NOT NULL-by-default column; pylon defaults the field to [].
		plugins = []string{}
	}

	// create_success stays FALSE until every step has succeeded. See the
	// package comment for why that ordering is load-bearing.
	if err := transaction.QueryRow(ctx, `
INSERT INTO centry.project (name, owner_id, plugins, create_success, keycloak_groups)
VALUES ($1, $2, $3, false, '{}')
RETURNING id`,
		state.request.Name, state.request.OwnerID, plugins,
	).Scan(&state.projectID); err != nil {
		return fmt.Errorf("insert project: %w", err)
	}

	limits := state.request.Limits
	if _, err := transaction.Exec(ctx, `
INSERT INTO centry.project_quota (
    project_id, data_retention_limit, test_duration_limit, cpu_limit,
    memory_limit, vcu_hard_limit, vcu_soft_limit, vcu_limit_total_block,
    storage_hard_limit, storage_soft_limit, storage_limit_total_block
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		state.projectID,
		limits.DataRetentionLimit, limits.TestDurationLimit, limits.CPULimit,
		limits.MemoryLimit, limits.VCUHardLimit, limits.VCUSoftLimit,
		limits.VCULimitTotalBlock, limits.StorageHardLimit, limits.StorageSoftLimit,
		limits.StorageLimitTotalBlock,
	); err != nil {
		return fmt.Errorf("insert project quota: %w", err)
	}

	// dast_scans and sast_scans are left to their column defaults (-1), exactly
	// as pylon leaves them; every counter defaults to 0.
	if _, err := transaction.Exec(ctx, `
INSERT INTO centry.statistic (project_id, start_time)
VALUES ($1, (now() AT TIME ZONE 'utc'))`,
		state.projectID,
	); err != nil {
		return fmt.Errorf("insert project statistic: %w", err)
	}

	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// removeProjectModel deletes the project row, its two companion rows, and every
// shared row that points at it.
//
// THE MECHANISM ISSUE #374 ASKS TO RECORD: the delete REMOVES THE REFERENCING
// ROWS. It does not disarm a foreign key and it does not add one. Seven foreign
// keys name centry.project(id) with no ON DELETE action, so a project that ever
// ran an agent turn or an index ingest could not be deleted at all — the row
// delete failed on the first of them. See referencingDeletes for the list and
// for the order.
//
// All of it runs in ONE transaction, the referencing rows included. Either the
// project and everything that names it go together, or nothing goes. A partial
// delete cannot commit, so the project row and its tenant schema stay in step.
func removeProjectModel(ctx context.Context, p *Provisioner, state *provisionState) error {
	if state.projectID == 0 {
		return nil
	}
	transaction, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = transaction.Rollback(context.WithoutCancel(ctx)) }()

	for _, cleanup := range referencingDeletes() {
		// A deployment that has not applied the shared history yet does not
		// have these tables. deleteTenantLedger guards the same way, and an
		// undefined table would abort the whole transaction.
		var present bool
		if err := transaction.QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, cleanup.table,
		).Scan(&present); err != nil {
			return fmt.Errorf("resolve %s: %w", cleanup.table, err)
		}
		if !present {
			continue
		}
		if _, err := transaction.Exec(ctx, cleanup.statement, state.projectID); err != nil {
			return fmt.Errorf("delete rows in %s: %w", cleanup.table, err)
		}
	}

	// The project-owned rows that no foreign key ties to the project (C1).
	// Nothing blocked the delete on them, so the project row went and they
	// stayed behind as orphans. See projectOwnedDeletes for what stays.
	for _, cleanup := range projectOwnedDeletes() {
		var present bool
		if err := transaction.QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, cleanup.table,
		).Scan(&present); err != nil {
			return fmt.Errorf("resolve %s: %w", cleanup.table, err)
		}
		if !present {
			continue
		}
		if _, err := transaction.Exec(ctx, cleanup.statement, state.projectID); err != nil {
			return fmt.Errorf("delete rows in %s: %w", cleanup.table, err)
		}
	}

	for _, statement := range []string{
		`DELETE FROM centry.statistic WHERE project_id = $1`,
		`DELETE FROM centry.project_quota WHERE project_id = $1`,
		`DELETE FROM centry.project WHERE id = $1`,
	} {
		if _, err := transaction.Exec(ctx, statement, state.projectID); err != nil {
			return fmt.Errorf("delete project rows: %w", err)
		}
	}
	return transaction.Commit(ctx)
}

// referencingDelete is one table to clear before the project row goes.
type referencingDelete struct {
	// table is the qualified name, used both as the to_regclass guard argument
	// and as the key the coverage test compares against the live catalogue.
	table string
	// statement takes the project id as $1.
	statement string
}

// referencingDeletes clears every shared row that would block the project row
// delete, in an order that leaves nothing dangling at any point.
//
// WHY A LIST RATHER THAN A CASCADE. The seven foreign keys are declared in
// shipped migrations, and a migration carries a content checksum, so the
// declarations cannot be edited. Adding ON DELETE CASCADE to them would also
// make every future project delete silently destroy the execution and index
// attestation records, without a reader of this package being able to see it.
// The list states what a delete removes.
//
// THE ORDER. Two of these tables hold rows that no project column selects:
// index_ingest_results and configuration_validation_results reach the project
// only through their execution job. They come first, because they block the
// tables below them. execution_jobs comes next to last of the runtime tables,
// because it cascades to the claims, the settlements, the outbox and the ingest
// jobs. input_bundles comes after it, because a job names its bundle.
//
// COVERAGE IS TESTED, NOT ASSUMED. A migration that adds a foreign key to
// centry.project reopens this defect. TestEveryBlockingForeignKeyIsCovered
// reads the live catalogue and fails while a table that can block the delete is
// absent from this list.
func referencingDeletes() []referencingDelete {
	// The project's execution jobs. Both project columns select them: a job
	// carries the project that owns the resource and the project the result is
	// projected into, and either one can name this project.
	const jobsOfProject = `
    SELECT job.execution_id, job.generation
    FROM elitea_runtime.execution_jobs AS job
    WHERE job.resource_project_id = $1 OR job.projection_project_id = $1`

	return []referencingDelete{

		// Remove recovery and immutable Code children before their execution owner.
		{
			table: "elitea_runtime.node_recovery_audit",
			statement: `DELETE FROM elitea_runtime.node_recovery_audit
WHERE (execution_id, generation) IN (` + jobsOfProject + `)`,
		},
		{
			table: "elitea_runtime.node_recovery_control_outbox",
			statement: `DELETE FROM elitea_runtime.node_recovery_control_outbox
WHERE (execution_id, generation) IN (` + jobsOfProject + `)`,
		},
		{
			table: "elitea_runtime.node_recovery_visits",
			statement: `DELETE FROM elitea_runtime.node_recovery_visits
WHERE (execution_id, generation) IN (` + jobsOfProject + `)`,
		},
		{
			table: "elitea_runtime.original_code_owner_receipts",
			statement: `DELETE FROM elitea_runtime.original_code_owner_receipts
WHERE (execution_id, generation) IN (` + jobsOfProject + `)`,
		},
		{
			table: "elitea_runtime.code_workspace_snapshots",
			statement: `DELETE FROM elitea_runtime.code_workspace_snapshots
WHERE resource_project_id = $1 OR (execution_id, generation) IN (` + jobsOfProject + `)`,
		},
		{
			table: "elitea_runtime.original_code_intents",
			statement: `DELETE FROM elitea_runtime.original_code_intents
WHERE (execution_id, generation) IN (` + jobsOfProject + `)`,
		},
		{
			table: "elitea_runtime.original_code_visits",
			statement: `DELETE FROM elitea_runtime.original_code_visits
WHERE (execution_id, generation) IN (` + jobsOfProject + `)`,
		},
		{
			table: "elitea_runtime.code_debug_artifacts",
			statement: `DELETE FROM elitea_runtime.code_debug_artifacts
WHERE project_id = $1 OR (execution_id, original_generation) IN (` + jobsOfProject + `)`,
		},
		{
			table: "elitea_runtime.execution_http_effects",
			statement: `DELETE FROM elitea_runtime.execution_http_effects
WHERE (execution_id, generation) IN (` + jobsOfProject + `)`,
		},
		{
			table: "elitea_runtime.execution_saved_child_definitions",
			statement: `DELETE FROM elitea_runtime.execution_saved_child_definitions
WHERE resource_project_id = $1 OR (execution_id, generation) IN (` + jobsOfProject + `)`,
		},
		{
			table: "elitea_runtime.execution_saved_child_captures",
			statement: `DELETE FROM elitea_runtime.execution_saved_child_captures
WHERE resource_project_id = $1 OR (execution_id, generation) IN (` + jobsOfProject + `)`,
		},
		{
			table: "elitea_runtime.execution_saved_child_scopes",
			statement: `DELETE FROM elitea_runtime.execution_saved_child_scopes
WHERE (execution_id, generation) IN (` + jobsOfProject + `)`,
		},
		{
			table: "elitea_runtime.index_ingest_results",
			statement: `
DELETE FROM elitea_runtime.index_ingest_results
WHERE (execution_id, generation) IN (` + jobsOfProject + `)`,
		},
		{
			table: "elitea_runtime.configuration_validation_results",
			statement: `
DELETE FROM elitea_runtime.configuration_validation_results
WHERE (execution_id, generation) IN (` + jobsOfProject + `)`,
		},
		{
			// Also a cascade child of execution_jobs. This statement takes the
			// rows this project owns whose job belongs to another project.
			table: "elitea_runtime.index_result_artifacts",
			statement: `
DELETE FROM elitea_runtime.index_result_artifacts WHERE resource_project_id = $1`,
		},
		{
			table: "elitea_runtime.execution_replay_events",
			statement: `
DELETE FROM elitea_runtime.execution_replay_events WHERE projection_project_id = $1`,
		},
		{
			table: "elitea_runtime.execution_replay_state",
			statement: `
DELETE FROM elitea_runtime.execution_replay_state WHERE projection_project_id = $1`,
		},
		{
			table: "elitea_runtime.execution_jobs",
			statement: `
DELETE FROM elitea_runtime.execution_jobs
WHERE resource_project_id = $1 OR projection_project_id = $1`,
		},
		{
			// A desktop local turn (shared/0155): no worker job, no children.
			table:     "elitea_runtime.local_turn_executions",
			statement: `DELETE FROM elitea_runtime.local_turn_executions WHERE project_id = $1`,
		},
		{
			table:     "elitea_runtime.execution_definition_source_refs",
			statement: `DELETE FROM elitea_runtime.execution_definition_source_refs WHERE resource_project_id = $1`,
		},
		{
			table:     "elitea_runtime.execution_captured_definitions",
			statement: `DELETE FROM elitea_runtime.execution_captured_definitions WHERE resource_project_id = $1`,
		},
		{
			table: "elitea_runtime.input_bundles",
			statement: `
DELETE FROM elitea_runtime.input_bundles WHERE resource_project_id = $1`,
		},
		{
			table: "elitea_runtime.index_generation_counters",
			statement: `
DELETE FROM elitea_runtime.index_generation_counters WHERE resource_project_id = $1`,
		},
	}
}

// projectOwnedDeletes clears the shared rows that belong to the project but
// carry no foreign key to centry.project (C1, found during the 2026-10
// regression cleanup). Each statement takes the project id as $1.
//
// The artifact bucket rows go here only when they are soft-deleted; the
// delete cascades to their object rows and transfer grants. The physical
// objects are purged before this, by the artifact_buckets step
// (TeardownProjectBuckets), which runs earlier in the reverse walk and
// soft-deletes a bucket only after its purge succeeded. A LIVE row is a bucket
// whose purge failed or never ran (no object store is configured). That row is
// the only handle on the bytes under p/<id>/<bucket>/, so it stays: Deprovision
// then reports ErrArtifactsNotRemoved, and another delete of the same id
// retries the purge.
//
// Request logs, usage events, audit events and the budget accumulators
// (gateway.llm_budget_accumulators) are deliberately NOT here. They are the
// history of what the project spent and did, and they outlive it. The
// accumulators are the one money source of /analytics_costs and the budgets
// API. A delete of them would also not hold: the scheduler's budget write-back
// re-creates the row for any delta that was in flight at delete time.
func projectOwnedDeletes() []referencingDelete {
	return []referencingDelete{
		{
			table:     "elitea_storage.buckets",
			statement: `DELETE FROM elitea_storage.buckets WHERE project_id = $1 AND deleted_at IS NOT NULL`,
		},
		{
			table:     "elitea_storage.bucket_permissions",
			statement: `DELETE FROM elitea_storage.bucket_permissions WHERE project_id = $1`,
		},
		{
			table:     "elitea_storage.project_storage_policy",
			statement: `DELETE FROM elitea_storage.project_storage_policy WHERE project_id = $1`,
		},
		{
			table:     "elitea_storage.attachment_chunks",
			statement: `DELETE FROM elitea_storage.attachment_chunks WHERE project_id = $1`,
		},
		{
			// project_id is TEXT on this table.
			table:     "public.pipeline_runs",
			statement: `DELETE FROM public.pipeline_runs WHERE project_id = ($1::bigint)::text`,
		},
		{
			table:     "centry.social_pins",
			statement: `DELETE FROM centry.social_pins WHERE project_id = $1`,
		},
		{
			table:     "elitea_runtime.tool_call_records",
			statement: `DELETE FROM elitea_runtime.tool_call_records WHERE project_id = $1`,
		},
	}
}

/* ── project_schema ────────────────────────────────────────────────────── */

// createProjectSchema builds the tenant.
//
// create_tenant_schema is the repository's single definition of the baseline
// tenant tables (internal/infra/db/migrations/001_initial.sql) and is idempotent
// throughout — every statement in it is CREATE ... IF NOT EXISTS. ApplyTenant
// then brings the schema to the head of migrations/tenant/. No table definition
// is restated in Go, which is what issue #333 requires.
func createProjectSchema(ctx context.Context, p *Provisioner, state *provisionState) error {
	if _, err := p.pool.Exec(ctx, `SELECT create_tenant_schema($1)`, state.tenantSchema()); err != nil {
		return fmt.Errorf("create tenant schema: %w", err)
	}
	// ApplyTenant re-reads centry.project on its own pooled connection, so this
	// can only run after createProjectModel committed. It is idempotent through
	// the migration ledger: versions already recorded for this project are
	// skipped, and the whole history applies in one transaction.
	if err := p.migrator.ApplyTenant(ctx, state.projectID); err != nil {
		return fmt.Errorf("apply tenant migrations: %w", err)
	}
	return nil
}

func removeProjectSchema(ctx context.Context, p *Provisioner, state *provisionState) error {
	if state.projectID == 0 {
		return nil
	}
	// Ledger first: if the DROP succeeds and this fails, the ledger would claim
	// a corpus is applied to a schema that no longer exists.
	if err := p.deleteTenantLedger(ctx, state.projectID); err != nil {
		return err
	}
	return p.dropTenantSchema(ctx, state.tenantSchema())
}

/* ── project_permissions ───────────────────────────────────────────────── */

// createProjectPermissions creates the project's roles, and only its roles.
//
// See the disposition note at the top of this file for why no
// auth_core__project_role_permission row is written: doing so would suppress
// legacyrbac's central fallback and cut the project off from the corpus grants.
func createProjectPermissions(ctx context.Context, p *Provisioner, state *provisionState) error {
	// Every central default-mode role name, plus `system`. pylon derives the
	// list from auth.get_roles(mode='default'), which includes `system` there.
	//
	// The UNION is kept although shared/0088 now seeds a central `system` role
	// and the SELECT above it therefore returns the name already. It is
	// idempotent under the ON CONFLICT clause. It also keeps provisioning
	// correct on a database whose central role list is not the shape the
	// migration assumed. An operator can rename or remove a central role.
	// A project with no `system` role issues no system PAT at all.
	if _, err := p.pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role (project_id, name)
SELECT $1, role_name
FROM (
    SELECT name AS role_name FROM public.auth_core__role WHERE mode = 'default'
    UNION
    SELECT $2::text
) AS project_roles
ON CONFLICT (project_id, name) DO NOTHING`,
		state.projectID, systemProjectRole,
	); err != nil {
		return fmt.Errorf("create project roles: %w", err)
	}
	return nil
}

// removeProjectPermissions deletes the project's roles and the token bindings
// that name it.
//
// pylon's counterpart is a no-op, which leaves orphan auth_core__project_role
// and auth_core__project_user_role rows behind after a project is deleted — its
// own deletion path has the same hole. Compensating properly here is a
// deliberate correction: the rows describe a project that will not exist, and
// auth_core__project_user_role cascades from the role, so one delete clears
// both.
//
// The binding delete is the same invariant one level up (ADR-0018,
// spec-llm-project-scope §7.3): a binding must not outlive membership. Removing
// one user from a project revokes that user's bindings for it, in
// eliteacore.UsersDelete. Removing the whole project takes every membership
// with it through the role cascade, so it must take every binding too.
// elitea_identity.token_project_binding.project_id carries no foreign key —
// centry.project is pylon-owned as well — so nothing deletes these rows for us.
//
// Both statements run in one transaction. Half a teardown leaves bindings that
// name a project no longer there, which is the state this function exists to
// prevent.
func removeProjectPermissions(ctx context.Context, p *Provisioner, state *provisionState) error {
	if state.projectID == 0 {
		return nil
	}
	transaction, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = transaction.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := transaction.Exec(ctx,
		`DELETE FROM elitea_identity.token_project_binding WHERE project_id = $1`,
		state.projectID,
	); err != nil {
		return fmt.Errorf("delete token project bindings: %w", err)
	}
	if _, err := transaction.Exec(ctx,
		`DELETE FROM public.auth_core__project_role WHERE project_id = $1`,
		state.projectID,
	); err != nil {
		return fmt.Errorf("delete project roles: %w", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

/* ── system_user ───────────────────────────────────────────────────────── */

// createSystemUser creates the project's system identity and gives it the
// `system` project role.
//
// This is not optional decoration. Three subsystems resolve a project's system
// PAT through queries/auth_pat.sql's GetActiveProjectSystemPAT —
// infra/storage/index_runtime_context.go, runtimecomposition's index-schedule
// token, and authcomposition's graph — and that query matches the identity by
// the literal email below. A project provisioned without it answers every
// indexing request with "active project-system PAT not found".
func createSystemUser(ctx context.Context, p *Provisioner, state *provisionState) error {
	email := systemUserEmail(state.projectID)
	name := systemUserName(state.projectID)

	// pylon probes for an existing user first and skips creation. ON CONFLICT
	// does the same thing without the race, and without the defect pylon's
	// early return carries (it returns a bare int rather than a dict, so the
	// next step loses its argument and re-provisioning fails outright).
	var userID int64
	if err := p.pool.QueryRow(ctx, `
WITH created AS (
    INSERT INTO public.auth_core__user (email, name)
    VALUES ($1, $2)
    ON CONFLICT (email) DO NOTHING
    RETURNING id
)
SELECT id FROM created
UNION ALL
SELECT id FROM public.auth_core__user WHERE email = $1
LIMIT 1`,
		email, name,
	).Scan(&userID); err != nil {
		return fmt.Errorf("create system user: %w", err)
	}
	state.systemUserID = userID

	// The role assignment GetActiveProjectSystemPAT joins through.
	if _, err := p.pool.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT $1, $2, role.id
FROM public.auth_core__project_role AS role
WHERE role.project_id = $1 AND role.name = $3
ON CONFLICT (project_id, user_id, role_id) DO NOTHING`,
		state.projectID, userID, systemProjectRole,
	); err != nil {
		return fmt.Errorf("assign system role: %w", err)
	}
	return nil
}

func removeSystemUser(ctx context.Context, p *Provisioner, state *provisionState) error {
	if state.systemUserID == 0 {
		return nil
	}
	// auth_core__token and auth_core__project_user_role both cascade from the
	// user row, so this also removes the PAT created by the next step.
	if _, err := p.pool.Exec(ctx,
		`DELETE FROM public.auth_core__user WHERE id = $1`, state.systemUserID,
	); err != nil {
		return fmt.Errorf("delete system user: %w", err)
	}
	return nil
}

func systemUserEmail(projectID int64) string {
	// PROJECT_USER_EMAIL_TEMPLATE, and the literal GetActiveProjectSystemPAT
	// reconstructs in SQL. Changing either without the other breaks the join.
	return fmt.Sprintf("system_user_%d@centry.user", projectID)
}

func systemUserName(projectID int64) string {
	// PROJECT_USER_NAME_TEMPLATE. middleware/project_resolver.go recognises this
	// prefix when mapping a system principal back to its project.
	return fmt.Sprintf(":system:project:%d:", projectID)
}

/* ── system_token ──────────────────────────────────────────────────────── */

// createSystemToken mints the project system PAT.
//
// auth_core__token stores no secret material: the row's `uuid` is the identity,
// and authsvc signs a bearer form from it on demand. So provisioning inserts a
// row and never handles a credential value.
func createSystemToken(ctx context.Context, p *Provisioner, state *provisionState) error {
	if state.systemUserID == 0 {
		return errors.New("system user was not created")
	}
	// pylon reuses the first existing token rather than minting a second. The
	// same rule, expressed as a conditional insert so that a re-run converges.
	// No expiry: GetActiveProjectSystemPAT accepts a NULL `expires`, and pylon
	// leaves the argument commented out.
	if _, err := p.pool.Exec(ctx, `
INSERT INTO public.auth_core__token (uuid, expires, user_id, name)
SELECT $1, NULL, $2, $3
WHERE NOT EXISTS (
    SELECT 1 FROM public.auth_core__token
    WHERE user_id = $2 AND name = $3
)`,
		uuid.NewString(), state.systemUserID, systemTokenName,
	); err != nil {
		return fmt.Errorf("create system token: %w", err)
	}
	return nil
}

func removeSystemToken(ctx context.Context, p *Provisioner, state *provisionState) error {
	if state.systemUserID == 0 {
		return nil
	}
	if _, err := p.pool.Exec(ctx,
		`DELETE FROM public.auth_core__token WHERE user_id = $1 AND name = $2`,
		state.systemUserID, systemTokenName,
	); err != nil {
		return fmt.Errorf("delete system token: %w", err)
	}
	return nil
}

/* ── project_secrets ───────────────────────────────────────────────────── */

// createProjectSecrets gives the project an empty secrets vault (#373).
//
// RESTORED, after being dropped. The disposition note at the top of this file
// used to record this step as deliberately dropped, on the reasoning that the
// Go vault mints a project's Fernet key lazily on first write and reports an
// absent vault as an empty list, so provisioning one would create state nothing
// reads. The first half was true; the second was not, and #373 was what the
// gap cost:
//
//   - infra/storage/postgres_secret_vault.go's load() turned pgx.ErrNoRows into
//     the generic ErrContentUnavailable;
//   - infra/storage/model_defaults.go's Load failed the WHOLE read on it;
//   - api/v2/configurations/models.go turned that into a 500.
//
// The model picker asks that route for its catalogue, so a project with no
// vault rows presented to its owner as "the product has no models". A customer
// given a new project could not pick a model and so could not start a chat
// turn, which made project creation incomplete for a pylon-free deployment.
//
// THAT CHAIN IS NOW BROKEN AT ITS FIRST LINK, and this step is not what holds
// it. An absent vault is a distinct answer (storage.ErrVaultAbsent) and each
// reader that consults the vault for a DEFAULT reads it as "never set", so a
// project with no vault answers 200. Do not read the step as the thing that
// keeps the model picker working, and do not delete it on the strength of that:
// it stays because pylon provisions the vault with the project, and because it
// keeps every vault minted by the one minter (api/v2/secrets.Handler).
//
// The vault is created empty and is then given ONE value: the project's
// `secrets_header_value` (#408). pylon additionally stores an approle blob in
// centry.project.secrets_json; that column stays dead here, its only occurrence
// in Go being the sqlc struct field, so this reproduces the half with a reader.
//
// The write is delegated rather than reimplemented. secrets.Handler is the only
// code that mints a vault key, and it alone knows whether SECRETS_MASTER_KEY
// wraps it; a second minter would write vaults that handler cannot open, and it
// never overwrites an unreadable vault, so the project would 500 for ever.
func createProjectSecrets(ctx context.Context, p *Provisioner, state *provisionState) error {
	if p.vault == nil {
		// Provision refuses a Provisioner with no vault, so this is unreachable
		// through it. Deprovision reaches the remove half with the same field,
		// and a step that silently did nothing is the shape #373 was.
		return errors.New("project vault bootstrapper is not configured")
	}
	if err := p.vault.EnsureProjectVault(ctx, state.projectIDString()); err != nil {
		return fmt.Errorf("create project secrets vault: %w", err)
	}
	// The `X-SECRET` value (#408). The vault is created empty, and an empty
	// vault is what made the value guessable: pylon's check_secret_header
	// reads `secrets.get("secrets_header_value", "secret")`, so a project that
	// never set one accepts the literal string "secret" on the version-details
	// route. Writing a random value here removes that state for every project
	// created from now on; secrets.BackfillProjectSecretsHeaderValues removes
	// it for the projects that already exist.
	//
	// IT IS PART OF THIS STEP AND NOT A STEP OF ITS OWN. The value has no
	// meaning without the vault that holds it, and one step means one
	// compensation: removeProjectSecrets deletes the vault rows, so the value
	// goes with them.
	written, err := p.vault.EnsureProjectSecretsHeaderValue(ctx, state.projectIDString())
	if err != nil {
		return fmt.Errorf("create project secrets header value: %w", err)
	}
	if !written {
		// Only a re-run over a vault that already holds a value reaches this,
		// and the value is kept rather than replaced. Say so: a project that
		// inherited a value is not the same as one that was given its own.
		p.logger.InfoContext(ctx, "the project vault already holds an X-SECRET value; keeping it",
			"project_id", state.projectIDString())
	}
	seedProjectDefaultModel(ctx, p, state)
	return nil
}

// seedProjectDefaultModel copies the platform default model into the vault
// this step just created (#6826). It is part of this step for the reason the
// X-SECRET value is: it has no meaning without the vault, and
// removeProjectSecrets removes it with the vault.
//
// A failure is logged and does not fail the step. The project still reads the
// platform default at request time, so the seed is a convenience and not a
// requirement; failing a whole tenant over it would be the worse outcome.
func seedProjectDefaultModel(ctx context.Context, p *Provisioner, state *provisionState) {
	if p.defaultModels == nil {
		return
	}
	if err := p.defaultModels.SeedProjectModelDefault(ctx, state.projectID); err != nil {
		p.logger.WarnContext(ctx, "the platform default model was not copied to the new project; it reads the platform default at request time",
			"project_id", state.projectIDString(), "err", err)
	}
}

// removeProjectSecrets deletes the vault, and with it the `X-SECRET` value
// createProjectSecrets sealed in it (#408).
//
// The delete is what compensates BOTH halves of the step. The value lives in
// centry.secrets_data, which RemoveProjectVault deletes in the same
// transaction as centry.secrets_key, so a compensated project has no vault and
// no value rather than a vault holding a credential nothing can reach.
//
// Removing an absent vault is success, so this is safe for a step that failed
// before it created anything.
func removeProjectSecrets(ctx context.Context, p *Provisioner, state *provisionState) error {
	if p.vault == nil || state.projectID == 0 {
		return nil
	}
	if err := p.vault.RemoveProjectVault(ctx, state.projectIDString()); err != nil {
		return fmt.Errorf("remove project secrets vault: %w", err)
	}
	return nil
}

/* ── artifact_buckets ──────────────────────────────────────────────────── */

// createArtifactBuckets creates the project's `reports` and `tasks` buckets.
//
// internal/application/artifactbootstrap already ports this step exactly, and
// its own doc comment records that nothing calls it because no project-creation
// path existed: "every project created after this migration ships has no system
// buckets". This is that call site.
//
// The bucket names are metadata rows rather than physical MinIO buckets — the
// S3 backend keys one configured bucket by project prefix — but the artifacts
// HTTP API answers 404 for a bucket with no row, so the rows are what make a new
// project's artifact surface reachable.
//
// The dependency is optional: a deployment with artifacts disabled configures no
// bootstrapper, and the step then reports success without doing anything rather
// than failing provisioning over a subsystem that deployment does not run.
func createArtifactBuckets(ctx context.Context, p *Provisioner, state *provisionState) error {
	if p.buckets == nil {
		return nil
	}
	if err := p.buckets.BootstrapProjectBuckets(ctx, state.projectIDString()); err != nil {
		return fmt.Errorf("bootstrap project buckets: %w", err)
	}
	return nil
}

func removeArtifactBuckets(ctx context.Context, p *Provisioner, state *provisionState) error {
	if state.projectID == 0 {
		return nil
	}
	if p.buckets == nil {
		// No object store is configured, so nothing can purge the bytes. A
		// project with no live bucket has nothing to purge. A project WITH one
		// must not report this step as done: its bytes stay in storage.
		live, err := p.liveBucketCount(ctx, state.projectID)
		if err != nil {
			return err
		}
		if live > 0 {
			return fmt.Errorf("teardown project buckets: %d live bucket(s) and no object store to purge them", live)
		}
		return nil
	}
	if err := p.buckets.TeardownProjectBuckets(ctx, state.projectIDString()); err != nil {
		return fmt.Errorf("teardown project buckets: %w", err)
	}
	return nil
}

// liveBucketCount counts the project's artifact buckets that are not
// soft-deleted. A deployment without the storage schema has none.
func (p *Provisioner) liveBucketCount(ctx context.Context, projectID int64) (int, error) {
	if p.pool == nil {
		return 0, nil
	}
	var present bool
	if err := p.pool.QueryRow(ctx,
		`SELECT to_regclass('elitea_storage.buckets') IS NOT NULL`,
	).Scan(&present); err != nil {
		return 0, fmt.Errorf("resolve elitea_storage.buckets: %w", err)
	}
	if !present {
		return 0, nil
	}
	var live int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM elitea_storage.buckets WHERE project_id = $1 AND deleted_at IS NULL`,
		projectID,
	).Scan(&live); err != nil {
		return 0, fmt.Errorf("count live buckets: %w", err)
	}
	return live, nil
}

/* ── project_pgvector ──────────────────────────────────────────────────── */

// createProjectVectorStore provisions the project's vector store (#371).
//
// It converges three things, and all three are needed before the project can
// index: the per-project PgVector role and database, the `pgvector_project_*`
// pair in the project's encrypted vault, and the `vectorstorage` configuration
// row in the tenant. internal/application/vectorstore.ProjectPgvectorService
// already owns that sequence and its retry ordering; this is its call site.
//
// WHY ALL THREE, and not just the row. Configuration expansion resolves
// `pgvector_configuration` by elitea_title in the project's own tenant, so the
// row is what an index run looks for. But the row stores only the placeholder
// `{{secret.pgvector_project_connstr}}`, and the unsecreter leaves an
// unresolved placeholder VERBATIM rather than failing
// (infra/storage/expansion_unsecreter.go). A row without its vault entry
// therefore passes every validator on the index path and fails much later as a
// connection error against a host literally named
// "{{secret.pgvector_project_connstr}}". Writing the row alone would move the
// defect, not fix it.
//
// WHY IT SITS HERE, before project_admin. pylon does this from a listener on
// the `project_created` event, which it fires only after create_success is
// committed — so in the reference a pgvector failure leaves a project that
// looks complete and cannot index. A step is compensated instead. It is placed
// before project_admin so that the one step which can fail on caller input (an
// administrator address that matches no account) still rolls the vector store
// back.
//
// THE DEPENDENCY IS OPTIONAL, and that is a real limitation rather than a
// design choice: a deployment configures no vector store when it has no
// PgVector bootstrap to provision from. The step then reports success without
// doing anything, exactly as createArtifactBuckets does. The difference matters
// and is stated here so nobody has to rediscover it: such a project CAN be
// created and CANNOT index. It is the state every project is in before this
// step existed, so its absence is not a regression — but it is not a working
// project either.
func createProjectVectorStore(ctx context.Context, p *Provisioner, state *provisionState) error {
	if p.vectorStore == nil {
		return nil
	}
	if err := p.vectorStore.ProvisionProjectVectorStore(ctx, state.projectID); err != nil {
		return fmt.Errorf("provision project vector store: %w", err)
	}
	return nil
}

// removeProjectVectorStore undoes what the step wrote to this platform.
//
// It removes the configuration row and the two vault entries. It does NOT drop
// the PgVector role or database. That is deliberate and bounded:
// internal/infra/pgvector has no drop path at all, pylon's own project delete
// has no pgvector step either, and dropping a database that may already hold a
// project's vectors is a destructive operation that belongs to an explicit
// decision rather than to a compensation. The role and database are converged
// idempotently, so a retry reuses them rather than leaking a second pair.
//
// The compensation still leaves nothing behind on THIS side, which is what the
// create path needs: no row an index run could resolve, and no vault entry
// naming a project that will not exist.
func removeProjectVectorStore(ctx context.Context, p *Provisioner, state *provisionState) error {
	if p.vectorStore == nil || state.projectID == 0 {
		return nil
	}
	if err := p.vectorStore.RemoveProjectVectorStore(ctx, state.projectID); err != nil {
		return fmt.Errorf("remove project vector store: %w", err)
	}
	return nil
}

/* ── project_admin ─────────────────────────────────────────────────────── */

// createProjectAdmin gives the project's administrators membership.
//
// NARROWED FROM THE REFERENCE, deliberately. pylon's
// `add_user_to_project_or_create` CREATES an account — in Keycloak and in
// auth_core — for an address it does not recognise, with a hardcoded temporary
// password. Minting an identity as a side effect of creating a project is not a
// behaviour this route should carry, and this platform has a dedicated invite
// surface for it (internal/api/v2/eliteacore's UsersCreate). So an address that
// matches no existing user FAILS the step.
//
// Failing is the point. Silently skipping an unknown address would answer 201
// while the administrator the caller named has no access — the "answers 200,
// provisioned nothing" shape this codebase keeps re-shipping. The whole create
// rolls back instead, and the caller sees which step failed.
//
// THE EMPTY-LIST RULE (#375). A request that names NO administrator used to
// return at once, which answered 201 for a project that had a schema, roles, a
// quota, a system user and a system token — and no human member at all.
// queries/auth_projects.sql's ListCurrentUserProjects inner-joins membership,
// so the project was absent from its own maker's list; legacyrbac's
// projectPermissions returns nothing without an assignment, so every
// default-mode route on it answered 403. The project could not be opened by
// anybody, including the person who had just created it.
//
// Issue #375 offered two rules and asked for one to be chosen and recorded.
// THE RULE CHOSEN IS: the MAKER of the project becomes its administrator when
// the request names nobody else. The alternative — refuse a create with no
// `project_admin_email` — was rejected for two reasons. It would turn a
// currently-accepted request into an error, which breaks a caller to fix a
// defect that a default can fix instead; and centry.project.owner_id already
// records the maker as the owner, so granting that same person the `admin`
// project role makes the membership table agree with a fact the project row
// already states rather than inventing a new one.
//
// The fallback applies ONLY to the empty case. A request that names
// administrators is left exactly as it was: the maker does not silently join a
// project it created for somebody else, which is the behaviour a platform
// operator onboarding a customer depends on.
//
// A request that names administrators but no ROLES takes the fallback too. The
// HTTP handler always sends `admin`, so that shape can only arrive from a
// programmatic caller; it used to grant nobody anything, and the invariant this
// step now holds is that a provisioned project always has an administrator.
func createProjectAdmin(ctx context.Context, p *Provisioner, state *provisionState) error {
	if len(state.request.AdminEmails) == 0 {
		return createOwnerMembership(ctx, p, state)
	}
	roles := state.request.AdminRoles
	if len(roles) == 0 {
		return createOwnerMembership(ctx, p, state)
	}

	for _, email := range state.request.AdminEmails {
		// Matched case-insensitively, as pylon lowercases before comparing.
		tag, err := p.pool.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT $1, account.id, role.id
FROM public.auth_core__user AS account
JOIN public.auth_core__project_role AS role
  ON role.project_id = $1 AND role.name = ANY($3::text[])
WHERE lower(account.email) = lower($2)
  AND account.suspended = false
ON CONFLICT (project_id, user_id, role_id) DO NOTHING`,
			state.projectID, email, roles,
		)
		if err != nil {
			return fmt.Errorf("assign project administrator: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Either the address matches no active account, or the assignment
			// already existed. Distinguish them, so a re-run stays idempotent
			// while a typo still fails.
			var member bool
			if err := p.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__user AS account
    JOIN public.auth_core__project_user_role AS assignment
      ON assignment.user_id = account.id AND assignment.project_id = $1
    WHERE lower(account.email) = lower($2)
)`, state.projectID, email).Scan(&member); err != nil {
				return fmt.Errorf("verify project administrator: %w", err)
			}
			if !member {
				return fmt.Errorf("%w: %s", ErrUnknownAdminEmail, email)
			}
		}
	}
	return nil
}

// ownerProjectRole is the project role the maker of a project receives when the
// request names no administrator.
//
// It is `admin` and not one of state.request.AdminRoles: those roles are what
// the caller asked for the people it NAMED, and this branch runs precisely when
// it named nobody. The role has to be the administrative one, because the
// purpose of the grant is that the maker can administer the project it just
// made — a viewer could not add the first member.
const ownerProjectRole = "admin"

// createOwnerMembership makes the maker of the project its administrator.
//
// It is the empty-`project_admin_email` branch of createProjectAdmin; see that
// function for the rule and why it was chosen.
func createOwnerMembership(ctx context.Context, p *Provisioner, state *provisionState) error {
	if state.request.OwnerID <= 0 {
		// Provision rejects a non-positive owner before any step runs, so this
		// cannot be reached through it.
		return ErrOwnerRequired
	}
	// Matched on the id rather than on an address: the owner is the
	// authenticated caller that Provision was given, never a body field.
	tag, err := p.pool.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT $1, account.id, role.id
FROM public.auth_core__user AS account
JOIN public.auth_core__project_role AS role
  ON role.project_id = $1 AND role.name = $3
WHERE account.id = $2
  AND account.suspended = false
ON CONFLICT (project_id, user_id, role_id) DO NOTHING`,
		state.projectID, state.request.OwnerID, ownerProjectRole,
	)
	if err != nil {
		return fmt.Errorf("assign project owner membership: %w", err)
	}
	if tag.RowsAffected() != 0 {
		return nil
	}
	// Either the owner has no active account, or the assignment already
	// existed. Distinguish them, so a re-run stays idempotent while an owner
	// that cannot be resolved still fails — a project whose maker cannot open
	// it is the whole defect this branch exists to prevent.
	var member bool
	if err := p.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__project_user_role AS assignment
    WHERE assignment.project_id = $1 AND assignment.user_id = $2
)`, state.projectID, state.request.OwnerID).Scan(&member); err != nil {
		return fmt.Errorf("verify project owner membership: %w", err)
	}
	if !member {
		return fmt.Errorf("%w: %d", ErrUnknownOwner, state.request.OwnerID)
	}
	return nil
}

func removeProjectAdmin(ctx context.Context, p *Provisioner, state *provisionState) error {
	if state.projectID == 0 {
		return nil
	}
	if len(state.request.AdminEmails) == 0 || len(state.request.AdminRoles) == 0 {
		// The branch createOwnerMembership took. Nothing to undo for a
		// Deprovision, which carries no request and therefore no owner.
		if state.request.OwnerID <= 0 {
			return nil
		}
		if _, err := p.pool.Exec(ctx,
			`DELETE FROM public.auth_core__project_user_role
             WHERE project_id = $1 AND user_id = $2`,
			state.projectID, state.request.OwnerID,
		); err != nil {
			return fmt.Errorf("remove project owner membership: %w", err)
		}
		return nil
	}
	// Only the assignments this step could have made. The system user's own
	// assignment is removed by removeSystemUser.
	if _, err := p.pool.Exec(ctx, `
DELETE FROM public.auth_core__project_user_role AS assignment
USING public.auth_core__user AS account
WHERE assignment.project_id = $1
  AND assignment.user_id = account.id
  AND lower(account.email) = ANY($2::text[])`,
		state.projectID, lowerAll(state.request.AdminEmails),
	); err != nil {
		return fmt.Errorf("remove project administrators: %w", err)
	}
	return nil
}

// ErrUnknownAdminEmail reports a project_admin_email that matches no active
// account. The handler maps it to 400.
var ErrUnknownAdminEmail = errors.New("projectprovisioning: no active account for administrator email")

// ErrUnknownOwner reports an owner id with no active account (#375).
//
// It is NOT mapped to 400. The owner is the authenticated caller rather than a
// body field, so an owner that resolves to no account is an inconsistency
// inside the deployment and not something the caller can correct by sending a
// different request. The create is rolled back and reported as a 500.
var ErrUnknownOwner = errors.New("projectprovisioning: no active account for project owner")

func lowerAll(values []string) []string {
	lowered := make([]string, 0, len(values))
	for _, value := range values {
		lowered = append(lowered, strings.ToLower(value))
	}
	return lowered
}
