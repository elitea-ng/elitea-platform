package runtimecomposition

// The project vector-store collaborator the provisioner's project_pgvector step
// calls (#371).
//
// It composes components that already existed and had no caller:
// newCurrentProjectPgvectorDatabaseProvisioner, the vault material repository
// beside it, and repos.CurrentProjectPgvectorConfigurationsRepository, joined by
// vectorstore.ProjectPgvectorService. Nothing here re-implements provisioning;
// this file supplies the two things the service could not supply for itself —
// the bootstrap connection, and the inverse used for compensation.
//
// WHERE THE BOOTSTRAP COMES FROM. pylon reads it from the PUBLIC project's
// `elitea-pgvector` configuration and unsecrets it against that project's vault
// (elitea_core/rpc/vectorstore.py:276-296). This does exactly the same, through
// the same two components the index path uses: FindByEliteaTitle and the vault
// unsecreter. It is resolved per provision rather than at start-up, so an
// operator who repoints the platform's vector store does not have to restart
// the service.
//
// WHEN THAT ROW IS ABSENT the deployment has no vector store at all, and no
// project — old or new — can index. The step then reports success without doing
// anything, because there is nothing to provision from and failing would take
// away project creation on a deployment that never had indexing. That is a
// bounded rule, not a silent gap: the row's presence is the deployment's own
// statement that it runs a vector store.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	vectorstoreapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/vectorstore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/pgvector"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrProjectVectorStoreBootstrap reports a public `elitea-pgvector`
// configuration that exists but cannot be used as a bootstrap connection. It is
// deliberately distinct from "absent": an absent row means the deployment runs
// no vector store, a malformed one means it is misconfigured.
var ErrProjectVectorStoreBootstrap = errors.New("runtimecomposition: project vector-store bootstrap is unusable")

// ErrProjectVectorStoreBootstrapMissing reports a drop for a project that HAD a
// vector store, on a deployment whose public elitea-pgvector bootstrap is gone.
// The database exists and cannot be reached, so the drop is not OK and not a
// skip (#1211).
var ErrProjectVectorStoreBootstrapMissing = errors.New("PgVector bootstrap not configured")

type projectVectorStoreFinder interface {
	FindByEliteaTitle(
		context.Context,
		int32,
		string,
		bool,
	) (configurationapp.CurrentExpansionConfiguration, bool, error)
}

type projectVectorStoreUnsecreter interface {
	Unsecret(context.Context, int32, map[string]any) (map[string]any, error)
}

type projectVectorStoreConfigurations interface {
	vectorstoreapp.ProjectConfigurationRepository
	DeleteProjectPgvectorConfiguration(context.Context, int64, string) (bool, error)
}

type projectVectorStoreSchemas interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// ProjectVectorStore satisfies projectprovisioning.ProjectVectorStore.
type ProjectVectorStore struct {
	publicProjectID int32
	schemas         projectVectorStoreSchemas
	finder          projectVectorStoreFinder
	unsecreter      projectVectorStoreUnsecreter
	materials       vectorstoreapp.ProjectMaterialRepository
	configurations  projectVectorStoreConfigurations
	logger          *slog.Logger
}

func newProjectVectorStore(
	publicProjectID int32,
	schemas projectVectorStoreSchemas,
	finder projectVectorStoreFinder,
	unsecreter projectVectorStoreUnsecreter,
	materials vectorstoreapp.ProjectMaterialRepository,
	configurations projectVectorStoreConfigurations,
	logger *slog.Logger,
) (*ProjectVectorStore, error) {
	if publicProjectID <= 0 || schemas == nil || finder == nil || unsecreter == nil ||
		materials == nil || configurations == nil {
		return nil, errors.New("project vector-store dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ProjectVectorStore{
		publicProjectID: publicProjectID,
		schemas:         schemas,
		finder:          finder,
		unsecreter:      unsecreter,
		materials:       materials,
		configurations:  configurations,
		logger:          logger,
	}, nil
}

// ProvisionProjectVectorStore converges the project's PgVector role and
// database, its vault material, and its `vectorstorage` configuration row.
func (s *ProjectVectorStore) ProvisionProjectVectorStore(ctx context.Context, projectID int64) error {
	if s == nil {
		return errors.New("project vector store is not configured")
	}
	if projectID <= 0 {
		return vectorstoreapp.ErrInvalidProjectPgvectorRequest
	}

	bootstrap, configured, err := s.resolveBootstrap(ctx)
	if err != nil {
		return err
	}
	if !configured {
		s.logger.WarnContext(ctx,
			"project created without a vector store: this deployment has no public elitea-pgvector configuration, so no project can index",
			"project_id", projectID, "public_project_id", s.publicProjectID)
		return nil
	}

	// The vault already exists. The project_secrets provisioning step creates
	// it, and it runs before project_pgvector (#399). This step used to create
	// one too, under a second master key, which produced a vault the material
	// writer could not open — see ProjectVaultSecrets for the whole account.
	databases, err := newCurrentProjectPgvectorDatabaseProvisioner(bootstrap)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrProjectVectorStoreBootstrap, err)
	}
	service, err := vectorstoreapp.NewProjectPgvectorService(databases, s.materials, s.configurations)
	if err != nil {
		return err
	}
	if _, err := service.Provision(ctx, vectorstoreapp.ProvisionRequest{
		ProjectID: projectID,
		// The documented production mode: one database and one login role per
		// project, which is what pylon's create_pgvector_credentials creates
		// when use_existing_pgvector_user is false — its default.
		Mode:               vectorstoreapp.IsolationDatabaseRole,
		Intent:             vectorstoreapp.ProvisionIfMissing,
		ConfigurationTitle: vectorstoreapp.DefaultProjectPgvectorTitle,
	}); err != nil {
		return err
	}
	return nil
}

// RemoveProjectVectorStore removes the project's `vectorstorage` configuration
// row.
//
// It does NOT drop the PgVector role or database: it is the create-failure
// rollback. An explicit delete drops the tenant schema (and this row with it)
// and calls DropProjectVectorStore from its cleanup journal.
//
// IT NO LONGER REMOVES THE VAULT (#399). The project_secrets step owns the
// vault, and removeProjectSecrets removes it. Both callers of this method reach
// that step too: compensation walks the step list in reverse, and Deprovision
// refuses to run without the vault bootstrapper. So the vault still goes with
// the project, through its one owner.
func (s *ProjectVectorStore) RemoveProjectVectorStore(ctx context.Context, projectID int64) error {
	if s == nil {
		return errors.New("project vector store is not configured")
	}
	if projectID <= 0 {
		return vectorstoreapp.ErrInvalidProjectPgvectorRequest
	}

	// The tenant may already be gone: Deprovision runs every remove, and a
	// project deleted before this step existed has no row to remove either.
	present, err := tenantConfigurationPresent(ctx, s.schemas, projectID)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if _, err := s.configurations.DeleteProjectPgvectorConfiguration(
		ctx, projectID, vectorstoreapp.DefaultProjectPgvectorTitle,
	); err != nil {
		return fmt.Errorf("delete project pgvector configuration: %w", err)
	}
	return nil
}

// ProjectHasVectorStore reports whether the project has its `vectorstorage`
// configuration row. A project whose tenant schema is already gone has none.
//
// It reads through query, which the project delete passes as its deciding
// transaction (#1211): the answer comes from the snapshot that removes the
// project, and no second pool connection is taken while the project row is
// locked.
func (s *ProjectVectorStore) ProjectHasVectorStore(ctx context.Context, query projectprovisioning.Querier, projectID int64) (bool, error) {
	if s == nil {
		return false, errors.New("project vector store is not configured")
	}
	if projectID <= 0 {
		return false, vectorstoreapp.ErrInvalidProjectPgvectorRequest
	}
	if query == nil {
		return false, errors.New("project vector-store probe needs a querier")
	}
	present, err := tenantConfigurationPresent(ctx, query, projectID)
	if err != nil || !present {
		return false, err
	}
	var has bool
	// The schema name is built from an int64 id, never from caller input.
	if err := query.QueryRow(ctx, fmt.Sprintf(`
SELECT EXISTS (
    SELECT 1 FROM p_%d.configuration
    WHERE project_id = $1::integer
      AND elitea_title = $2::text
      AND type = 'pgvector'
      AND section = 'vectorstorage'
      AND source = 'system')`, projectID),
		projectID, vectorstoreapp.DefaultProjectPgvectorTitle,
	).Scan(&has); err != nil {
		return false, fmt.Errorf("read project vector-store configuration: %w", err)
	}
	return has, nil
}

// DropProjectVectorStore drops the project's PgVector database and login role
// (#1211). Callers: only an explicit project delete, from its cleanup journal. The create-failure rollback
// calls RemoveProjectVectorStore, which never reaches this.
//
// It is idempotent (a missing database or role is a no-op). hadStore is whether
// the project had a vector store before its removal (recorded in the cleanup
// journal by the transaction that removed the project row). With hadStore false it returns a skip (database "") without
// connecting to the PgVector server. With no public elitea-pgvector
// configuration and hadStore true it is ErrProjectVectorStoreBootstrapMissing,
// because the database exists and cannot be reached.
func (s *ProjectVectorStore) DropProjectVectorStore(ctx context.Context, projectID int64, hadStore bool) (string, error) {
	if s == nil {
		return "", errors.New("project vector store is not configured")
	}
	if projectID <= 0 {
		return "", vectorstoreapp.ErrInvalidProjectPgvectorRequest
	}
	// A project that never had a vector store has no database to drop. Return
	// before touching the PgVector server at all: connecting to it (and its admin
	// credentials) to confirm an absence would make a delete of a project that
	// never indexed depend on a server it does not use, and fail when that server
	// is down.
	if !hadStore {
		return "", nil
	}
	database := pgvector.ProjectDatabaseName(projectID)
	bootstrap, configured, err := s.resolveBootstrap(ctx)
	if err != nil {
		return database, err
	}
	if !configured {
		return database, ErrProjectVectorStoreBootstrapMissing
	}
	databases, err := newCurrentProjectPgvectorDatabaseProvisioner(bootstrap)
	if err != nil {
		return database, fmt.Errorf("%w: %s", ErrProjectVectorStoreBootstrap, err)
	}
	result, err := databases.Drop(ctx, projectID)
	if err != nil {
		return database, err
	}
	s.logger.InfoContext(ctx, "dropped project vector store",
		"project_id", projectID,
		"database_dropped", result.DatabaseDropped, "role_dropped", result.RoleDropped)
	return database, nil
}

// tenantConfigurationPresent reports whether the project's tenant configuration
// table exists. The delete runs inside a tenant transaction, which refuses a
// schema that is not there.
func tenantConfigurationPresent(ctx context.Context, query projectVectorStoreSchemas, projectID int64) (bool, error) {
	var present bool
	// The name is derived from an int64 id, never from caller input, and it is
	// bound as one text argument rather than concatenated in SQL.
	if err := query.QueryRow(ctx,
		`SELECT to_regclass($1::text) IS NOT NULL`,
		fmt.Sprintf("p_%d.configuration", projectID),
	).Scan(&present); err != nil {
		return false, fmt.Errorf("resolve project tenant configuration table: %w", err)
	}
	return present, nil
}

// resolveBootstrap reads the platform's PgVector bootstrap from the public
// project, exactly where pylon reads it.
//
// configured is false only when the row is absent. A row that is present but
// unusable is an error, so a misconfigured deployment is reported rather than
// silently treated as one that runs no vector store.
func (s *ProjectVectorStore) resolveBootstrap(ctx context.Context) (*pgx.ConnConfig, bool, error) {
	configuration, found, err := s.finder.FindByEliteaTitle(
		ctx,
		s.publicProjectID,
		vectorstoreapp.DefaultProjectPgvectorTitle,
		false,
	)
	if err != nil {
		return nil, false, fmt.Errorf("read project vector-store bootstrap: %w", err)
	}
	if !found {
		return nil, false, nil
	}

	raw, ok := configuration.Data["connection_string"].(string)
	if !ok || raw == "" {
		return nil, false, fmt.Errorf("%w: no connection string", ErrProjectVectorStoreBootstrap)
	}
	// Redeem only when there is something to redeem. The unsecreter loads the
	// owning project's vault and FAILS when that vault does not exist, and the
	// public project has no vault until something writes a secret there — so
	// calling it unconditionally would reject a bootstrap stored in the clear,
	// which is how the standalone stack and most single-tenant deployments
	// store it. The check is deliberately coarser than the placeholder syntax
	// it guards: it can only cause a redemption that was not needed, never skip
	// one that was.
	if strings.Contains(raw, "{{") {
		data, err := s.unsecreter.Unsecret(ctx, s.publicProjectID, configuration.Data)
		if err != nil {
			return nil, false, fmt.Errorf("%w: redeem bootstrap secrets", ErrProjectVectorStoreBootstrap)
		}
		raw, ok = data["connection_string"].(string)
		if !ok || raw == "" {
			return nil, false, fmt.Errorf("%w: no connection string", ErrProjectVectorStoreBootstrap)
		}
	}
	// The stored form is SQLAlchemy's `postgresql+psycopg://`, which pgx cannot
	// parse. This is the same normalisation the index path applies.
	dsn, ok := pgvector.NormalizeConnectionString(raw)
	if !ok {
		return nil, false, fmt.Errorf("%w: connection string is not a PostgreSQL URL", ErrProjectVectorStoreBootstrap)
	}
	// A placeholder that no vault entry redeemed reaches here verbatim, and
	// would otherwise become a host literally named "{{secret.…}}".
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, false, fmt.Errorf("%w: connection string is malformed", ErrProjectVectorStoreBootstrap)
	}
	return config, true, nil
}

// NewProjectVectorStore composes the project vector-store collaborator from the
// Configurations runtime, which owns the finder and the unsecreter this needs.
//
// The vault material is NOT taken from that runtime (#399). The runtime keys
// its vault loader and its vault writer off ELITEA_VAULT_MASTER_KEY_FILE, and
// no deployment sets that variable. The vault this step writes into is made by
// the secrets handler under SECRETS_MASTER_KEY, which deployments do set. So
// the caller injects the handler, and the creator and the writer hold one key.
func (runtime *CurrentConfigurationsRuntime) NewProjectVectorStore(
	pool *pgxpool.Pool,
	secrets ProjectVaultSecrets,
	logger *slog.Logger,
) (*ProjectVectorStore, error) {
	if runtime == nil || runtime.rows == nil || runtime.unsecreter == nil ||
		secrets == nil || pool == nil {
		return nil, errors.New("project vector-store composition is incomplete")
	}
	configurations, err := repos.NewCurrentProjectPgvectorConfigurationsRepository(pool)
	if err != nil {
		return nil, err
	}
	materials, err := newCurrentProjectPgvectorMaterialRepository(secrets)
	if err != nil {
		return nil, err
	}
	return newProjectVectorStore(
		runtime.publicProjectID,
		pool,
		runtime.rows,
		runtime.unsecreter,
		materials,
		configurations,
		logger,
	)
}
