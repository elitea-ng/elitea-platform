// Package projectprovisioning creates a project and its tenant, which is the
// one capability that kept legacy pylon on the critical path (#333). A
// pylon-free deployment could serve the projects it already had; it could not
// take a new one.
//
// The reference is legacy/plugins/projects/utils/project_steps.py. This package
// reproduces that pipeline's SEMANTICS — the ordered steps, the per-step status
// records, the reverse-order compensation, and `create_success` as the marker
// that provisioning finished — not its transaction shape, which does not
// survive the port. See steps.go for the per-step port/drop disposition.
//
// WHY THIS IS NOT ONE TRANSACTION. migrate.Runner.ApplyTenant re-reads
// centry.project on a connection it takes from the pool itself, and opens its
// own transaction to apply the corpus. So the project row must be COMMITTED
// before the tenant corpus can be applied to it, and no single transaction can
// span both. pylon has the same shape for a different reason (it commits after
// every step), so the compensation model is a port rather than an invention.
//
// THE INVARIANT THAT MATTERS. `create_success` is written TRUE only after every
// step succeeded. It is not decoration: cmd/elitea-migrate's `-all-tenants`
// preflight hard-errors when ANY project with `create_success = TRUE` has no
// `p_<id>` schema, and that error fails migration for the whole deployment, not
// for the one project. So a half-provisioned project must never carry
// `create_success = TRUE`. Every failure path in this file preserves that,
// including the path where compensation itself fails.
package projectprovisioning

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TenantMigrator applies the embedded tenant history to one project's schema.
// It is an interface at the consumer so that provisioning can be tested without
// the real corpus, and so this package does not depend on the migrate runner's
// concrete *pgxpool.Pool constructor.
type TenantMigrator interface {
	ApplyTenant(ctx context.Context, projectID int64) error
}

// ErrNameRequired reports an empty project name. pylon's ProjectCreatePD
// declares `name: constr(min_length=1)`, so an empty name is a 400 there too.
var ErrNameRequired = errors.New("projectprovisioning: project name is required")

// ErrOwnerRequired reports a missing or non-positive owner. pylon takes the
// owner from `g.auth.id` and never from the request body; so does the handler.
var ErrOwnerRequired = errors.New("projectprovisioning: owner id must be positive")

// Limits carries the per-project ceilings written to centry.project_quota.
// Field for field legacy/plugins/projects/models/pd/project.py's ProjectCreatePD,
// including its defaults — see DefaultLimits.
type Limits struct {
	DataRetentionLimit     int32
	TestDurationLimit      int32
	CPULimit               int32
	MemoryLimit            int32
	VCUHardLimit           int32
	VCUSoftLimit           int32
	VCULimitTotalBlock     bool
	StorageHardLimit       int32
	StorageSoftLimit       int32
	StorageLimitTotalBlock bool
}

// DefaultLimits are ProjectCreatePD's field defaults, transcribed exactly.
// A caller that omits a limit gets the same ceiling pylon would have given it.
func DefaultLimits() Limits {
	return Limits{
		DataRetentionLimit:     1_000_000_000,
		TestDurationLimit:      -1,
		CPULimit:               -1,
		MemoryLimit:            -1,
		VCUHardLimit:           5000,
		VCUSoftLimit:           4700,
		VCULimitTotalBlock:     false,
		StorageHardLimit:       10,
		StorageSoftLimit:       9,
		StorageLimitTotalBlock: false,
	}
}

// Request is one project-create instruction. Name and OwnerID are mandatory.
type Request struct {
	Name string
	// Plugins is stored verbatim on centry.project.plugins. pylon defaults it
	// to the empty list and never validates the entries.
	Plugins []string
	// OwnerID is the authenticated caller. It is never read from a request body.
	OwnerID int64
	// AdminEmails receive membership in the new project, with AdminRoles.
	// pylon's ProjectAdmin step does exactly this and skips silently when the
	// list is empty.
	AdminEmails []string
	// AdminRoles are the project roles AdminEmails are granted. The HTTP
	// handler hardcodes {"admin"}, as pylon's AdminAPI.post does.
	AdminRoles []string
	Limits     Limits
}

// Result is the outcome of one Provision call. Steps and RollbackSteps carry
// one record per step ATTEMPTED, in attempt order, which is the shape
// AdminAPI.post returns.
type Result struct {
	ProjectID     int64
	Steps         []StepStatus
	RollbackSteps []StepStatus
}

// ArtifactBootstrapper creates and removes a project's system buckets. It is
// satisfied by internal/application/artifactbootstrap.Bootstrapper, and is
// optional: a deployment that runs no artifact store configures none.
type ArtifactBootstrapper interface {
	BootstrapProjectBuckets(ctx context.Context, projectID string) error
	TeardownProjectBuckets(ctx context.Context, projectID string) error
}

// ProjectVaultBootstrapper creates and removes a project's secrets vault. It is
// satisfied by internal/api/v2/secrets.Handler.
//
// UNLIKE ArtifactBootstrapper, THIS ONE IS REQUIRED. The artifact bootstrapper
// is optional because a deployment can genuinely run no object store; there is
// no deployment that runs no vault, because the vault tables are in the same
// database as the project row and the model catalogue reads them on every page
// that shows a model. A Provisioner built without this dependency refuses to
// provision rather than creating projects with the #373 gap in them.
type ProjectVaultBootstrapper interface {
	EnsureProjectVault(ctx context.Context, projectID string) error
	// EnsureProjectSecretsHeaderValue seals the project's `X-SECRET` value in
	// the vault EnsureProjectVault created, and reports whether it wrote one
	// (#408). It is on this interface, and not done by the provisioner, for
	// the reason the whole interface exists: one minter owns every vault
	// write, because it alone knows how the key is wrapped.
	EnsureProjectSecretsHeaderValue(ctx context.Context, projectID string) (bool, error)
	RemoveProjectVault(ctx context.Context, projectID string) error
}

// ProjectVectorStore creates and removes a project's vector-store credentials
// and its `vectorstorage` configuration row — the state an index run resolves
// before it can write a vector (#371).
//
// It is satisfied by runtimecomposition's adapter over
// internal/application/vectorstore.ProjectPgvectorService, and it is optional
// for the same reason ArtifactBootstrapper is: a deployment that runs no
// PgVector bootstrap still creates projects. Unlike the bucket bootstrapper,
// though, its absence is NOT a no-op that leaves a working project — see
// createProjectVectorStore.
type ProjectVectorStore interface {
	ProvisionProjectVectorStore(ctx context.Context, projectID int64) error
	RemoveProjectVectorStore(ctx context.Context, projectID int64) error
}

// Provisioner runs the project-create pipeline.
type Provisioner struct {
	pool        *pgxpool.Pool
	migrator    TenantMigrator
	buckets     ArtifactBootstrapper
	vault       ProjectVaultBootstrapper
	vectorStore ProjectVectorStore
	logger      *slog.Logger
}

// Option configures a Provisioner at construction time.
type Option func(*Provisioner)

// WithArtifactBuckets supplies the bucket bootstrapper. Without it the
// artifact_buckets step is inert — see createArtifactBuckets.
func WithArtifactBuckets(buckets ArtifactBootstrapper) Option {
	return func(p *Provisioner) { p.buckets = buckets }
}

// WithProjectVault supplies the secrets-vault bootstrapper (#373). It is an
// Option for symmetry with the rest of this constructor, not because it is
// optional: Provision refuses to run without it. See ProjectVaultBootstrapper.
func WithProjectVault(vault ProjectVaultBootstrapper) Option {
	return func(p *Provisioner) { p.vault = vault }
}

// WithVectorStore supplies the project vector-store provisioner. Without it the
// project_pgvector step is inert — see createProjectVectorStore.
//
// A TYPED NIL IS TREATED AS ABSENT (#377). A composition root that declares its
// variable as the concrete pointer type and assigns it into this interface hands
// over a value that is not nil as an interface and is nil as a pointer. Every
// `!= nil` check upstream then passes, this option stores the collaborator, and
// the project_pgvector step calls a nil receiver — which answers "project vector
// store is not configured" and fails the WHOLE create pipeline, rolling a fully
// built tenant back. Measured on the end-to-end stack: a deployment that
// composes no Configurations runtime could not create a project at all, and the
// operator read a 500.
//
// The composition root is corrected as well (cmd/elitea-main/main.go declares
// the interface type), because passing a typed nil is the defect. This guard is
// here because the constructor is public: the next caller cannot be made to read
// that comment first, and the failure it produces names a step rather than the
// wiring that caused it.
func WithVectorStore(vectorStore ProjectVectorStore) Option {
	return func(p *Provisioner) {
		if isNilPointer(vectorStore) {
			return
		}
		p.vectorStore = vectorStore
	}
}

// isNilPointer reports a nil pointer wrapped in a non-nil interface.
//
// The same helper internal/infra/authattempt uses, for the same reason.
func isNilPointer(value any) bool {
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}

func New(pool *pgxpool.Pool, migrator TenantMigrator, logger *slog.Logger, options ...Option) *Provisioner {
	if logger == nil {
		logger = slog.Default()
	}
	provisioner := &Provisioner{pool: pool, migrator: migrator, logger: logger}
	for _, option := range options {
		option(provisioner)
	}
	return provisioner
}

// Provision creates the project row, its tenant schema, its tenant migration
// state and its RBAC rows, then marks it created.
//
// On any step failure it compensates every ATTEMPTED step in reverse — the one
// that failed included, because a step can fail halfway through its own work —
// and returns the error together with both status lists, so the caller can
// report what was attempted and what was undone. That is the information
// AdminAPI.post's 400 branch carries.
func (p *Provisioner) Provision(ctx context.Context, request Request) (Result, error) {
	if strings.TrimSpace(request.Name) == "" {
		return Result{}, ErrNameRequired
	}
	if request.OwnerID <= 0 {
		return Result{}, ErrOwnerRequired
	}
	// The vault is checked here beside the pool and the migrator, and for the
	// same reason: a project provisioned without one is not a project with a
	// missing extra, it is a project whose owner cannot pick a model (#373).
	// Failing before the first insert is the only failure that leaves nothing
	// behind.
	if p.pool == nil || p.migrator == nil || p.vault == nil {
		return Result{}, errors.New("projectprovisioning: provisioner is not configured")
	}

	state := &provisionState{request: request}
	result := Result{}

	for _, step := range createSteps() {
		status := StepStatus{Step: step.name, Initialized: true}
		err := step.create(ctx, p, state)
		if err != nil {
			status.setFailed(safeStepMessage(step.name))
			result.Steps = append(result.Steps, status)
			result.ProjectID = state.projectID
			p.logger.ErrorContext(ctx, "project provisioning step failed",
				"step", step.name, "project_id", state.projectID, "err", err)

			result.RollbackSteps = p.compensate(ctx, state, result.Steps)
			return result, fmt.Errorf("projectprovisioning: step %s: %w", step.name, err)
		}
		status.setOK()
		result.Steps = append(result.Steps, status)
	}

	// The completion marker, written last and on its own. pylon sets
	// create_success outside the step list too, for the same reason: it is not a
	// provisioning action, it is the assertion that provisioning finished.
	//
	// If this write fails the tenant is fully built but unmarked, and every
	// reader that filters on create_success would ignore it — a project that
	// exists and is invisible. Compensating is the honest outcome.
	if err := p.markCreated(ctx, state.projectID); err != nil {
		p.logger.ErrorContext(ctx, "project provisioning could not be marked complete",
			"project_id", state.projectID, "err", err)
		result.ProjectID = state.projectID
		result.RollbackSteps = p.compensate(ctx, state, result.Steps)
		return result, fmt.Errorf("projectprovisioning: mark created: %w", err)
	}

	result.ProjectID = state.projectID
	return result, nil
}

// markCreated flips create_success once every step has succeeded.
func (p *Provisioner) markCreated(ctx context.Context, projectID int64) error {
	tag, err := p.pool.Exec(ctx,
		`UPDATE centry.project SET create_success = true WHERE id = $1`, projectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("project %d disappeared during provisioning", projectID)
	}
	return nil
}

// compensate undoes the steps that ran, in reverse order.
//
// EVERY ATTEMPTED STEP IS COMPENSATED, including the one that failed. This is
// not defensive padding — it is the difference between a clean rollback and a
// surviving tenant. project_schema creates the schema and THEN applies the
// corpus; when the corpus fails, the schema is already there while the step
// reports failure. Compensating only the steps that reported success left that
// schema behind, which the integration test caught. pylon appends each step to
// its progress list BEFORE calling create for the same reason, so this matches
// the reference as well as being correct. Every remove is written to tolerate a
// step that never ran.
//
// Unlike pylon's rollback loop — which has no per-step try/except, so the first
// undo that raises aborts every remaining undo and discards the progress report
// — every compensation here is isolated. One failing undo cannot strand the
// rest.
//
// THE SCHEMA DROP IS THE EXCEPTION, AND IT RUNS LAST (#374). Reverse order puts
// project_schema before project_model, so the schema went first and the row
// went second. A row delete that then failed left a project row whose schema is
// gone — the one state cmd/elitea-migrate's preflight refuses to run against,
// and that refusal stops migration for EVERY tenant in the deployment. So
// project_schema is lifted out of the loop and runs after it, and it runs only
// when the project row is proved gone. See removeTenantSchemaWhenTheRowIsGone.
//
// The remaining order is unchanged: every other undo runs in reverse, and the
// last of them removes the project row.
func (p *Provisioner) compensate(ctx context.Context, state *provisionState, attempted []StepStatus) []StepStatus {
	// Compensation must still run when the request context is already done —
	// otherwise a client disconnect during provisioning leaves the tenant
	// half-created. WithoutCancel keeps the deadline off but the values on.
	ctx = context.WithoutCancel(ctx)

	touched := make(map[string]struct{}, len(attempted))
	for _, status := range attempted {
		touched[status.Step] = struct{}{}
	}

	steps := createSteps()
	rollback := make([]StepStatus, 0, len(steps))
	var schemaStep *step
	for i := len(steps) - 1; i >= 0; i-- {
		step := steps[i]
		if _, ok := touched[step.name]; !ok {
			continue
		}
		if step.name == StepProjectSchema {
			schemaStep = &steps[i]
			continue
		}
		status := StepStatus{Step: step.name, Initialized: true}
		if err := step.remove(ctx, p, state); err != nil {
			status.setFailed(safeStepMessage(step.name))
			p.logger.ErrorContext(ctx, "project provisioning compensation failed",
				"step", step.name, "project_id", state.projectID, "err", err)
		} else {
			status.setOK()
		}
		rollback = append(rollback, status)
	}
	if schemaStep != nil {
		rollback = append(rollback, p.removeTenantSchemaWhenTheRowIsGone(ctx, state, *schemaStep))
	}
	return rollback
}

// removeTenantSchemaWhenTheRowIsGone drops the tenant schema, but only after it
// proves that no centry.project row names it.
//
// The tenant data is the part that cannot be rebuilt. So the schema is the LAST
// thing the delete removes, and it is held back on any doubt: a project row
// that is still there, and a read that cannot say whether it is still there,
// both keep the schema. The step is then reported as not run, the caller learns
// which step was held back, and the delete converges on a retry.
//
// Holding the schema back is always the recoverable choice. A project row with
// its schema is a usable project. A project row without its schema stops
// cmd/elitea-migrate for the whole deployment.
func (p *Provisioner) removeTenantSchemaWhenTheRowIsGone(
	ctx context.Context, state *provisionState, schemaStep step,
) StepStatus {
	status := StepStatus{Step: schemaStep.name, Initialized: true}
	if state.projectID != 0 {
		var survived bool
		if err := p.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1)`, state.projectID,
		).Scan(&survived); err != nil {
			status.setFailed(heldBackStepMessage(schemaStep.name))
			p.logger.ErrorContext(ctx, "tenant schema kept because the project row could not be read",
				"step", schemaStep.name, "project_id", state.projectID, "err", err)
			return status
		}
		if survived {
			status.setFailed(heldBackStepMessage(schemaStep.name))
			p.logger.ErrorContext(ctx, "tenant schema kept because the project row is still there",
				"step", schemaStep.name, "project_id", state.projectID)
			return status
		}
	}
	if err := schemaStep.remove(ctx, p, state); err != nil {
		status.setFailed(safeStepMessage(schemaStep.name))
		p.logger.ErrorContext(ctx, "project provisioning compensation failed",
			"step", schemaStep.name, "project_id", state.projectID, "err", err)
		return status
	}
	status.setOK()
	return status
}

// provisionState carries values between steps. It replaces pylon's mutable
// `context` kwargs bag, which is the source of two defects this port does not
// reproduce: a step that returns a scalar instead of a dict silently fails to
// populate the next step's argument, and a compensation invoked before its
// step ran raises TypeError on the missing key.
type provisionState struct {
	request      Request
	projectID    int64
	systemUserID int64
}

// projectIDString is the decimal project id, for the interfaces that take one.
func (s *provisionState) projectIDString() string {
	return strconv.FormatInt(s.projectID, 10)
}

// tenantSchema is the tenant schema name for the project being provisioned.
// The `p_<id>` convention is fixed across this repository — see
// migrate.Runner.ApplyTenant and tenant.BindProject, which derive it the same
// way. The id is an int64 from a SERIAL column, so it cannot carry an
// identifier-breaking character; the DDL path still quotes it.
func (s *provisionState) tenantSchema() string {
	return "p_" + strconv.FormatInt(s.projectID, 10)
}

// safeStepMessage is what the caller is told about a failed step.
//
// pylon puts `str(exception)` in the status `msg`, which crosses a trust
// boundary with a raw database error in it. AGENTS.md forbids that, and the
// cause is logged with the project id instead. The step NAME is the part a
// caller can act on, and it is preserved exactly.
func safeStepMessage(step string) string {
	return "step " + step + " did not complete"
}

// heldBackStepMessage is what the caller is told about a step that did not run
// because running it would have made the damage worse.
//
// It is a different message from safeStepMessage on purpose. "did not complete"
// and "was not started, to keep the tenant data" call for different operator
// actions, and one message for both states would hide which one happened.
func heldBackStepMessage(step string) string {
	return "step " + step + " was not started, because the project row is still there"
}

// deleteTenantLedger removes the tenant migration ledger rows for a project
// whose schema is being dropped.
//
// Without this the ledger would claim the corpus is applied to a schema that no
// longer exists. Nothing reuses a SERIAL id in practice, so this is hygiene
// rather than a correctness fix — but a ledger that describes an absent target
// is exactly the kind of stale record that makes a later diagnosis wrong.
func (p *Provisioner) deleteTenantLedger(ctx context.Context, projectID int64) error {
	// The ledger schema is created on demand by the migrate runner, so on a
	// failure before the first ApplyTenant it may not exist at all.
	var exists bool
	if err := p.pool.QueryRow(ctx,
		`SELECT to_regclass('elitea_runtime.schema_migrations') IS NOT NULL`,
	).Scan(&exists); err != nil {
		return fmt.Errorf("resolve migration ledger: %w", err)
	}
	if !exists {
		return nil
	}
	if _, err := p.pool.Exec(ctx, `
DELETE FROM elitea_runtime.schema_migrations
WHERE target_kind = 'tenant' AND target_id = $1`,
		strconv.FormatInt(projectID, 10),
	); err != nil {
		return fmt.Errorf("delete tenant ledger rows: %w", err)
	}
	return nil
}

// dropTenantSchema removes the project's schema and everything in it.
func (p *Provisioner) dropTenantSchema(ctx context.Context, schema string) error {
	// pgx cannot parameterise an identifier, so the name is quoted rather than
	// interpolated. It is derived from an int64 id, never from caller input.
	statement := "DROP SCHEMA IF EXISTS " + pgx.Identifier{schema}.Sanitize() + " CASCADE"
	if _, err := p.pool.Exec(ctx, statement); err != nil {
		return fmt.Errorf("drop tenant schema: %w", err)
	}
	return nil
}
