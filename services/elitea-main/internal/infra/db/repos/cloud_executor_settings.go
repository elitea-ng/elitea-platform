package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// CloudExecutorSettingsRepository reads the two layers of the cloud executor
// policy (ADR-0034 decision 11): the platform ceiling (centry.platform_config,
// section `cloud_executor`) and the project's `cloud_executor` configuration
// row in its tenant `configuration` table. It writes the project switch
// through the configuration mutation repository, so every write carries the
// row lock, the revision, the actor and the lifecycle outbox event that a
// write through the configurations API carries.
type CloudExecutorSettingsRepository struct {
	pool      *pgxpool.Pool
	mutations *CurrentConfigurationMutationRepository
}

var (
	_ workspacehandoff.PlatformReader       = (*CloudExecutorSettingsRepository)(nil)
	_ workspacehandoff.ProjectSettingReader = (*CloudExecutorSettingsRepository)(nil)
	_ workspacehandoff.ProjectSwitchStore   = (*CloudExecutorSettingsRepository)(nil)
)

func NewCloudExecutorSettingsRepository(pool *pgxpool.Pool) (*CloudExecutorSettingsRepository, error) {
	if pool == nil {
		return nil, errors.New("cloud executor settings database is required")
	}
	// No master key: the cloud_executor type holds no secret, and the
	// secret-free mutation refuses any.
	mutations, err := NewCurrentConfigurationMutationRepository(pool, nil)
	if err != nil {
		return nil, err
	}
	return &CloudExecutorSettingsRepository{pool: pool, mutations: mutations}, nil
}

func (r *CloudExecutorSettingsRepository) CloudExecutorSettings(ctx context.Context) (platformconfig.CloudExecutorSettings, error) {
	return platformconfig.LoadCloudExecutorSettings(ctx, r.pool)
}

// CloudExecutorSetting reads the project row. A project with none, or with a
// row whose data does not decode, narrows nothing — but an undecodable row is
// an error, never a silent "unset": a project that switched the network off
// must not get it back through a malformed save. It is on the handoff's hot
// path, so a project with no tenant table is told by the SELECT's own
// undefined_table error, with no lookup before it.
func (r *CloudExecutorSettingsRepository) CloudExecutorSetting(ctx context.Context, projectID int64) (workspacehandoff.ProjectSetting, bool, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return workspacehandoff.ProjectSetting{}, false, err
	}
	var raw []byte
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT data FROM %s.configuration WHERE type = $1 ORDER BY id LIMIT 1`, schema),
		configurationapp.CloudExecutorConfigurationType).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspacehandoff.ProjectSetting{}, false, nil
	}
	if undefinedTable(err) {
		return workspacehandoff.ProjectSetting{}, false, workspacehandoff.ErrProjectNotFound
	}
	if err != nil {
		return workspacehandoff.ProjectSetting{}, false, fmt.Errorf("cloud executor: read project setting: %w", err)
	}
	var setting workspacehandoff.ProjectSetting
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &setting); err != nil {
			return workspacehandoff.ProjectSetting{}, false, fmt.Errorf("cloud executor: decode project setting: %w", err)
		}
	}
	return setting, true, nil
}

// undefinedTable reports PostgreSQL's 42P01: the project's tenant schema or
// its configuration table does not exist.
func undefinedTable(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "42P01"
}

// errCloudExecutorSwitchUnchanged ends a mutation that has nothing to write
// (unset on a project with no row); the transaction rolls back empty.
var errCloudExecutorSwitchUnchanged = errors.New("cloud executor: nothing to write")

// cloudExecutorTitleAttempts bounds the retries of a first write whose
// generated title met an existing one.
const cloudExecutorTitleAttempts = 3

// SetCloudExecutorProjectEnabled writes the `enabled` field of the project's
// cloud_executor row and keeps every other field: an explicit value is
// merged in, nil removes the key so the project follows the platform's
// project default. Without a row, an explicit value creates one and nil
// changes nothing. The row is the one CloudExecutorSetting reads (the lowest
// id of the type, never found by title).
//
// Every write is one configuration mutation (row, revision and lifecycle
// outbox event, with actorID as its actor), under a transaction-scoped
// advisory lock that keeps two first writes from creating two rows. A new
// row gets a generated title, `cloud_executor_<12 hex>`, because elitea_title
// is UNIQUE across the table and any row of another type may already hold a
// plain `cloud_executor`; a collision retries with a new title.
func (r *CloudExecutorSettingsRepository) SetCloudExecutorProjectEnabled(ctx context.Context, projectID, actorID int64, enabled *bool) error {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil || projectID > math.MaxInt32 {
		return workspacehandoff.ErrProjectNotFound
	}
	if actorID <= 0 || actorID > math.MaxInt32 {
		return errors.New("cloud executor: the write needs the acting user")
	}
	for attempt := 1; ; attempt++ {
		err = r.mutations.WithinSecretFreeConfigurationMutation(ctx, int32(projectID),
			func(store configurationapp.CurrentConfigurationMutationStore) error {
				return writeCloudExecutorEnabled(ctx, store, schema, int32(projectID), int32(actorID), enabled)
			})
		switch {
		case err == nil, errors.Is(err, errCloudExecutorSwitchUnchanged):
			return nil
		case errors.Is(err, configurationapp.ErrCurrentConfigurationConflict) && attempt < cloudExecutorTitleAttempts:
			continue
		case errors.Is(err, workspacehandoff.ErrProjectNotFound):
			return err
		}
		// The mutation folds a missing project into "unavailable"; tell it
		// apart here, off the hot path.
		var exists bool
		if lookupErr := r.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".configuration").Scan(&exists); lookupErr == nil && !exists {
			return workspacehandoff.ErrProjectNotFound
		}
		return fmt.Errorf("cloud executor: write project setting: %w", err)
	}
}

func writeCloudExecutorEnabled(
	ctx context.Context,
	store configurationapp.CurrentConfigurationMutationStore,
	schema string,
	projectID, actorID int32,
	enabled *bool,
) error {
	mutation, ok := store.(*currentConfigurationMutationStore)
	if !ok || mutation.tx == nil {
		return ErrCurrentConfigurationMutationUnavailable
	}
	if _, err := mutation.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "cloud_executor_setting:"+schema); err != nil {
		return fmt.Errorf("cloud executor: lock project setting: %w", err)
	}
	// The tenant search_path is installed: `configuration` is the project's.
	var id int32
	err := mutation.tx.QueryRow(ctx, `SELECT id FROM configuration WHERE type = $1 ORDER BY id LIMIT 1`,
		configurationapp.CloudExecutorConfigurationType).Scan(&id)
	if undefinedTable(err) {
		return workspacehandoff.ErrProjectNotFound
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("cloud executor: find project setting: %w", err)
	}
	lifecycleID, uuidErr := uuid.NewRandom()
	if uuidErr != nil {
		return uuidErr
	}
	intent := configurationapp.CurrentConfigurationLifecycleIntent{ID: lifecycleID.String(), ActorID: actorID}

	if errors.Is(err, pgx.ErrNoRows) {
		if enabled == nil {
			return errCloudExecutorSwitchUnchanged
		}
		rowUUID, uuidErr := uuid.NewRandom()
		if uuidErr != nil {
			return uuidErr
		}
		label := "Cloud executor"
		author := actorID
		created, err := store.InsertConfiguration(ctx, configurationapp.CurrentConfigurationCreate{
			UUID: rowUUID.String(), ProjectID: projectID, Label: &label,
			EliteaTitle: configurationapp.CloudExecutorConfigurationType + "_" +
				strings.ReplaceAll(rowUUID.String(), "-", "")[:12],
			Type: configurationapp.CloudExecutorConfigurationType, Section: configurationapp.CloudExecutorSection,
			Data: map[string]any{"enabled": *enabled}, Meta: map[string]any{},
			Source: configurationapp.CurrentConfigurationSourceUser, AuthorID: &author,
		})
		if err != nil {
			return err
		}
		intent.Operation = configurationapp.CurrentConfigurationCreated
		intent.After = cloudExecutorLifecycleSnapshot(created)
	} else {
		existing, err := store.GetForMutation(ctx, id)
		if err != nil {
			return err
		}
		data := cloneCurrentObjectForPersistence(existing.Data)
		if data == nil {
			data = map[string]any{}
		}
		if enabled == nil {
			delete(data, "enabled")
		} else {
			data["enabled"] = *enabled
		}
		updated, err := store.ReplaceConfiguration(ctx, configurationapp.CurrentConfigurationReplace{
			ProjectID: projectID, ConfigurationID: id, Label: existing.Label, EliteaTitle: existing.EliteaTitle,
			Data: data, Meta: existing.Meta, Shared: existing.Shared, StatusOK: existing.StatusOK,
			StatusLogs: existing.StatusLogs,
		})
		if err != nil {
			return err
		}
		intent.Operation = configurationapp.CurrentConfigurationUpdated
		intent.Before = cloudExecutorLifecycleSnapshot(existing)
		intent.After = cloudExecutorLifecycleSnapshot(updated)
	}
	if err := store.PutHiddenSecrets(ctx, nil); err != nil {
		return err
	}
	return store.AppendLifecycleIntent(ctx, intent)
}

// cloudExecutorLifecycleSnapshot is the outbox's view of the row. The type's
// schema has no secret field, so its data is already the sanitized data.
func cloudExecutorLifecycleSnapshot(configuration configurationapp.CurrentConfiguration) *configurationapp.CurrentConfigurationLifecycleSnapshot {
	data := cloneCurrentObjectForPersistence(configuration.Data)
	if data == nil {
		data = map[string]any{}
	}
	return &configurationapp.CurrentConfigurationLifecycleSnapshot{
		ID: configuration.ID, UUID: configuration.UUID, ProjectID: configuration.ProjectID,
		EliteaTitle: configuration.EliteaTitle, Type: configuration.Type, Section: configuration.Section,
		Label: cloneCurrentStringForPersistence(configuration.Label), Shared: configuration.Shared,
		StatusOK: configuration.StatusOK, Source: configuration.Source,
		AuthorID: cloneCurrentInt32ForPersistence(configuration.AuthorID), Data: data,
	}
}
