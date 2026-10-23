package repos

import (
	"context"
	"errors"
	"fmt"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ConfigurationTargetsRepository struct {
	projects projectStore
}

func NewConfigurationTargetsRepository(pool *pgxpool.Pool) (*ConfigurationTargetsRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return &ConfigurationTargetsRepository{projects: projects}, nil
}

func newConfigurationTargetsRepository(projects projectStore) (*ConfigurationTargetsRepository, error) {
	if projects == nil {
		return nil, errors.New("configuration target database is required")
	}
	return &ConfigurationTargetsRepository{projects: projects}, nil
}

// ResolveValidationTarget reads only from the authorized resource project's
// transaction-local tenant schema. ProjectionProjectID and public path values
// can never select a database schema.
func (r *ConfigurationTargetsRepository) ResolveValidationTarget(ctx context.Context, identity executionapp.AdmissionIdentity, revisionID string) (configurationapp.ValidationTarget, error) {
	if identity.TenantID == "" || identity.ResourceProjectID == "" || identity.ProjectionProjectID == "" || identity.ActorID == "" || revisionID == "" {
		return configurationapp.ValidationTarget{}, configurationapp.ErrInvalidValidationAdmission
	}
	resourceProjectID, err := parseProjectID(identity.ResourceProjectID)
	if err != nil {
		return configurationapp.ValidationTarget{}, configurationapp.ErrInvalidValidationAdmission
	}
	var target configurationapp.ValidationTarget
	var catalogDigest, schemaDigest, settingsDigest []byte
	err = r.projects.WithinProjectTx(ctx, resourceProjectID, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly}, func(tx sqlExecutor) error {
		if err := tx.QueryRow(ctx, `
SELECT configuration_type,
       catalog_revision,
       catalog_digest,
       schema_id,
       schema_revision,
       schema_digest,
       settings_entry_id,
       settings_entry_version,
       settings_content_digest
FROM configuration_revisions
WHERE revision_id = $1`, revisionID).Scan(
			&target.ConfigurationType,
			&target.CatalogRevision,
			&catalogDigest,
			&target.SchemaID,
			&target.SchemaRevision,
			&schemaDigest,
			&target.SettingsEntryID,
			&target.SettingsVersion,
			&settingsDigest,
		); errors.Is(err, pgx.ErrNoRows) {
			return configurationapp.ErrInvalidValidationAdmission
		} else if err != nil {
			return fmt.Errorf("load immutable configuration revision: %w", err)
		}
		return nil
	})
	if err != nil {
		return configurationapp.ValidationTarget{}, err
	}
	if target.CatalogDigest, err = storedDigest(catalogDigest); err != nil {
		return configurationapp.ValidationTarget{}, fmt.Errorf("stored configuration catalog digest: %w", err)
	}
	if target.SchemaDigest, err = storedDigest(schemaDigest); err != nil {
		return configurationapp.ValidationTarget{}, fmt.Errorf("stored configuration schema digest: %w", err)
	}
	if target.ExpectedSettingsDigest, err = storedDigest(settingsDigest); err != nil {
		return configurationapp.ValidationTarget{}, fmt.Errorf("stored configuration settings digest: %w", err)
	}
	if err := target.Validate(); err != nil {
		return configurationapp.ValidationTarget{}, err
	}
	return target, nil
}

var _ configurationapp.ValidationTargetResolver = (*ConfigurationTargetsRepository)(nil)
