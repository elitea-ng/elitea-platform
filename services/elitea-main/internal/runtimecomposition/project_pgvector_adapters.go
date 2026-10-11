package runtimecomposition

import (
	"context"
	"errors"
	"strconv"

	secretsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	vectorstoreapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/vectorstore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/pgvector"
	"github.com/jackc/pgx/v5"
)

type currentProjectPgvectorDatabaseProvisioner struct {
	provisioner *pgvector.Provisioner
	admin       pgvector.AdminConnection
}

func newCurrentProjectPgvectorDatabaseProvisioner(
	bootstrap *pgx.ConnConfig,
) (*currentProjectPgvectorDatabaseProvisioner, error) {
	if bootstrap == nil {
		return nil, errors.New("current project pgvector bootstrap is required")
	}
	projectConnectionParams, err := pgvector.ProjectConnectionParametersFromURL(bootstrap.ConnString())
	if err != nil {
		return nil, errors.New("current project pgvector bootstrap connection parameters are invalid")
	}
	connector, err := pgvector.NewPGXConnector(bootstrap)
	if err != nil {
		return nil, err
	}
	provisioner, err := pgvector.NewProvisioner(connector)
	if err != nil {
		return nil, err
	}
	return &currentProjectPgvectorDatabaseProvisioner{
		provisioner: provisioner,
		admin: pgvector.AdminConnection{
			User:                    bootstrap.User,
			Password:                bootstrap.Password,
			Host:                    bootstrap.Host,
			Port:                    bootstrap.Port,
			Database:                bootstrap.Database,
			ProjectConnectionParams: projectConnectionParams,
		},
	}, nil
}

// Drop removes the project's PgVector database and login role under the
// bootstrap admin connection (#1211). Production always provisions in the
// database-and-role mode, so that is the mode dropped.
func (a *currentProjectPgvectorDatabaseProvisioner) Drop(
	ctx context.Context,
	projectID int64,
) (pgvector.DropResult, error) {
	if a == nil || a.provisioner == nil {
		return pgvector.DropResult{}, pgvector.ErrInvalidConnector
	}
	return a.provisioner.Drop(ctx, pgvector.DropRequest{
		ProjectID: projectID,
		Admin:     a.admin,
		Mode:      pgvector.ModeDatabaseRole,
	})
}

// Exists reports whether the project's PgVector database or role is present.
func (a *currentProjectPgvectorDatabaseProvisioner) Exists(
	ctx context.Context,
	projectID int64,
) (bool, error) {
	if a == nil || a.provisioner == nil {
		return false, pgvector.ErrInvalidConnector
	}
	return a.provisioner.Exists(ctx, pgvector.DropRequest{
		ProjectID: projectID,
		Admin:     a.admin,
		Mode:      pgvector.ModeDatabaseRole,
	})
}

func (*currentProjectPgvectorDatabaseProvisioner) NewProjectPassword() (string, error) {
	return pgvector.NewProjectPassword()
}

func (a *currentProjectPgvectorDatabaseProvisioner) Provision(
	ctx context.Context,
	request vectorstoreapp.DatabaseProvisionRequest,
	handoff func(context.Context, vectorstoreapp.DatabaseProvisionResult) error,
) (vectorstoreapp.DatabaseProvisionResult, error) {
	if a == nil || a.provisioner == nil || handoff == nil {
		return vectorstoreapp.DatabaseProvisionResult{}, pgvector.ErrInvalidConnector
	}
	if request.Mode != vectorstoreapp.IsolationDatabaseRole && request.Mode != vectorstoreapp.IsolationSchema {
		return vectorstoreapp.DatabaseProvisionResult{}, pgvector.ErrInvalidRequest
	}

	mode := pgvector.ModeDatabaseRole
	if request.Mode == vectorstoreapp.IsolationSchema {
		mode = pgvector.ModeSchema
	}
	result, err := a.provisioner.ProvisionWithHandoff(ctx, pgvector.Request{
		ProjectID:            request.ProjectID,
		Admin:                a.admin,
		Mode:                 mode,
		UseExistingAdminUser: request.UseExistingAdminUser,
		Password:             request.ProjectDatabasePassword,
	}, func(handoffContext context.Context, result pgvector.Result) error {
		return handoff(handoffContext, vectorstoreapp.DatabaseProvisionResult{
			Status:           result.Status,
			Password:         result.Password,
			ConnectionString: result.ConnectionString,
		})
	})
	if err != nil {
		return vectorstoreapp.DatabaseProvisionResult{}, err
	}
	return vectorstoreapp.DatabaseProvisionResult{
		Status:           result.Status,
		Password:         result.Password,
		ConnectionString: result.ConnectionString,
	}, nil
}

// ProjectVaultSecrets reads and writes one project's vault material. It is
// satisfied by internal/api/v2/secrets.Handler.
//
// WHY THE PORT IS THIS SHAPE AND NOT A LOADER PLUS A MUTATOR (#399). The read
// half and the write half must hold the SAME master key, because they open the
// same ciphertext. Two dependencies can be given two key sources; one cannot.
// The earlier pair took storage.SecretVaultLoader and a MutateProject mutator,
// both built by the Configurations runtime from ELITEA_VAULT_MASTER_KEY_FILE,
// which no deployment sets — while the vault they wrote into is created by the
// secrets handler under SECRETS_MASTER_KEY, which five deployments do set. So
// the writer could not open the vault the creator made, and nothing reported it.
//
// The port also does NOT create a vault. The project_secrets provisioning step
// is the one creator, and it runs before project_pgvector.
//
// LookupProjectSecret reports an absent secret with secretsapi.ErrSecretNotFound
// and NOT with a `found bool` (#416). The port carries the secrets package's one
// idiom rather than a second one of its own, so the three answers stay separable
// on this side of the boundary too — see lookupOptionalProjectSecret.
type ProjectVaultSecrets interface {
	LookupProjectSecret(ctx context.Context, projectID string, name string) (string, error)
	StoreProjectSecrets(ctx context.Context, projectID string, values map[string]string) error
}

// lookupOptionalProjectSecret separates the three answers a vault read gives.
//
// found is true only for a value the vault holds. It is false only for
// ErrSecretNotFound — the vault opened and holds no such name. EVERY other
// error is returned, including secretsapi.ErrVaultAbsent: a project with no
// vault, and a vault that will not open, are both faults the caller must see.
//
// This is the whole point of the sentinel. A read failure that reached the
// caller as `found == false, err == nil` would look exactly like a project that
// has never been indexed, and the provisioner would then mint a SECOND password
// over material it simply could not read.
func lookupOptionalProjectSecret(
	ctx context.Context,
	secrets ProjectVaultSecrets,
	projectID string,
	name string,
) (string, bool, error) {
	value, err := secrets.LookupProjectSecret(ctx, projectID, name)
	switch {
	case err == nil:
		return value, true, nil
	case errors.Is(err, secretsapi.ErrSecretNotFound):
		return "", false, nil
	default:
		return "", false, err
	}
}

type currentProjectPgvectorMaterialRepository struct {
	secrets ProjectVaultSecrets
}

func newCurrentProjectPgvectorMaterialRepository(
	secrets ProjectVaultSecrets,
) (*currentProjectPgvectorMaterialRepository, error) {
	if secrets == nil {
		return nil, errors.New("current project pgvector vault dependencies are required")
	}
	return &currentProjectPgvectorMaterialRepository{secrets: secrets}, nil
}

func (r *currentProjectPgvectorMaterialRepository) LoadProjectPgvectorMaterial(
	ctx context.Context,
	projectID int64,
) (vectorstoreapp.ProjectMaterial, error) {
	if ctx == nil || projectID <= 0 || r == nil || r.secrets == nil {
		return vectorstoreapp.ProjectMaterial{}, vectorstoreapp.ErrInvalidProjectPgvectorRequest
	}
	if err := ctx.Err(); err != nil {
		return vectorstoreapp.ProjectMaterial{}, err
	}
	vaultProjectID := strconv.FormatInt(projectID, 10)

	password, passwordFound, err := lookupOptionalProjectSecret(
		ctx,
		r.secrets,
		vaultProjectID,
		vectorstoreapp.ProjectPgvectorPasswordKey,
	)
	if err != nil {
		return vectorstoreapp.ProjectMaterial{}, currentProjectPgvectorAdapterError(ctx, err)
	}
	connectionString, connectionStringFound, err := lookupOptionalProjectSecret(
		ctx,
		r.secrets,
		vaultProjectID,
		vectorstoreapp.ProjectPgvectorConnstrKey,
	)
	if err != nil {
		return vectorstoreapp.ProjectMaterial{}, currentProjectPgvectorAdapterError(ctx, err)
	}
	return vectorstoreapp.ProjectMaterial{
		Password:              password,
		PasswordFound:         passwordFound,
		ConnectionString:      connectionString,
		ConnectionStringFound: connectionStringFound,
	}, nil
}

func (r *currentProjectPgvectorMaterialRepository) StoreProjectPgvectorMaterial(
	ctx context.Context,
	projectID int64,
	password string,
	connectionString string,
) error {
	if ctx == nil || projectID <= 0 || connectionString == "" || r == nil || r.secrets == nil {
		return vectorstoreapp.ErrInvalidProjectPgvectorRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// One rewrite for both values. A password stored without its connection
	// string is material an index run cannot use and cannot diagnose.
	if err := r.secrets.StoreProjectSecrets(ctx, strconv.FormatInt(projectID, 10), map[string]string{
		vectorstoreapp.ProjectPgvectorPasswordKey: password,
		vectorstoreapp.ProjectPgvectorConnstrKey:  connectionString,
	}); err != nil {
		return currentProjectPgvectorAdapterError(ctx, err)
	}
	return nil
}

func currentProjectPgvectorAdapterError(ctx context.Context, cause error) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if errors.Is(cause, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return vectorstoreapp.ErrProjectPgvectorVault
}

var (
	_ vectorstoreapp.DatabaseProvisioner       = (*currentProjectPgvectorDatabaseProvisioner)(nil)
	_ vectorstoreapp.ProjectMaterialRepository = (*currentProjectPgvectorMaterialRepository)(nil)
)
