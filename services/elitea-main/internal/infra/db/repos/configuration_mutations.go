package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxCurrentLifecycleSnapshotBytes = 2 * 1024 * 1024

var (
	ErrInvalidCurrentConfigurationTransaction  = errors.New("invalid current configuration transaction")
	ErrCurrentConfigurationMutationUnavailable = errors.New("current configuration mutation is unavailable")
)

type currentConfigurationMutationQueries interface {
	LockCurrentConfigurationForMutation(context.Context, sqlcgen.LockCurrentConfigurationForMutationParams) (sqlcgen.LockCurrentConfigurationForMutationRow, error)
	InsertCurrentConfiguration(context.Context, sqlcgen.InsertCurrentConfigurationParams) (sqlcgen.InsertCurrentConfigurationRow, error)
	ReplaceCurrentConfiguration(context.Context, sqlcgen.ReplaceCurrentConfigurationParams) (sqlcgen.ReplaceCurrentConfigurationRow, error)
	DeleteCurrentConfiguration(context.Context, sqlcgen.DeleteCurrentConfigurationParams) (int32, error)
	GetLatestConfigurationLifecycleRevision(context.Context, sqlcgen.GetLatestConfigurationLifecycleRevisionParams) (int64, error)
	InsertConfigurationLifecycleEvent(context.Context, sqlcgen.InsertConfigurationLifecycleEventParams) error
}

type currentConfigurationMutationQueryFactory func(sqlExecutor) (currentConfigurationMutationQueries, error)

// CurrentConfigurationMutationRepository is the persistence owner for one
// current configuration mutation. It installs the authorized tenant
// search_path, locks the existing qualified project vault, and keeps the
// configuration row, encrypted vault, and lifecycle outbox changes in the same
// PostgreSQL transaction.
type CurrentConfigurationMutationRepository struct {
	projects  projectStore
	queries   currentConfigurationMutationQueryFactory
	masterKey []byte
}

func NewCurrentConfigurationMutationRepository(
	pool *pgxpool.Pool,
	masterKey []byte,
) (*CurrentConfigurationMutationRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentConfigurationMutationRepository(projects, newCurrentConfigurationMutationQueries, masterKey)
}

func newCurrentConfigurationMutationRepository(
	projects projectStore,
	queries currentConfigurationMutationQueryFactory,
	masterKey []byte,
) (*CurrentConfigurationMutationRepository, error) {
	if projects == nil || queries == nil {
		return nil, errors.New("current configuration mutation database is required")
	}
	if len(masterKey) != 0 && !validEncodedCurrentFernetKey(masterKey) {
		return nil, errors.New("current configuration mutation master key is invalid")
	}
	return &CurrentConfigurationMutationRepository{
		projects:  projects,
		queries:   queries,
		masterKey: append([]byte(nil), masterKey...),
	}, nil
}

func newCurrentConfigurationMutationQueries(tx sqlExecutor) (currentConfigurationMutationQueries, error) {
	executor, ok := tx.(pgxExecutor)
	if !ok || executor.queryer == nil {
		return nil, errors.New("current configuration mutation transaction does not support generated queries")
	}
	return sqlcgen.New(executor.queryer), nil
}

func (r *CurrentConfigurationMutationRepository) WithinCurrentConfigurationMutation(
	ctx context.Context,
	projectID int32,
	fn func(configurationapp.CurrentConfigurationMutationStore) error,
) error {
	return r.withinCurrentConfigurationMutation(ctx, projectID, false, fn)
}

// WithinSecretFreeConfigurationMutation is WithinCurrentConfigurationMutation
// for a configuration type whose schema holds no secret: the same row lock,
// revision and lifecycle outbox event in one transaction, but a project that
// has no vault yet (a fresh install provisions none) is written too. Its
// store refuses every hidden secret, so PutHiddenSecrets takes only an empty
// list.
func (r *CurrentConfigurationMutationRepository) WithinSecretFreeConfigurationMutation(
	ctx context.Context,
	projectID int32,
	fn func(configurationapp.CurrentConfigurationMutationStore) error,
) error {
	return r.withinCurrentConfigurationMutation(ctx, projectID, true, fn)
}

func (r *CurrentConfigurationMutationRepository) withinCurrentConfigurationMutation(
	ctx context.Context,
	projectID int32,
	secretFree bool,
	fn func(configurationapp.CurrentConfigurationMutationStore) error,
) error {
	if r == nil || r.projects == nil || r.queries == nil {
		return ErrCurrentConfigurationMutationUnavailable
	}
	if ctx == nil || projectID <= 0 || fn == nil {
		return ErrInvalidCurrentConfigurationTransaction
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var callbackErr error
	err := r.projects.WithinProjectTx(ctx, int64(projectID), pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadWrite,
	}, func(tx sqlExecutor) error {
		queries, err := r.queries(tx)
		if err != nil {
			return ErrCurrentConfigurationMutationUnavailable
		}
		vaultID := "project-" + strconv.FormatInt(int64(projectID), 10)
		vault, err := lockCurrentSecretVault(ctx, tx, vaultID)
		if secretFree && errors.Is(err, errCurrentVaultAbsent) {
			// No vault to lock and nothing to write to one: the store
			// refuses every hidden secret.
			vault, err = &lockedCurrentSecretVault{id: vaultID}, nil
		}
		if err != nil {
			return err
		}
		store := &currentConfigurationMutationStore{
			ctx:        ctx,
			projectID:  projectID,
			tx:         tx,
			queries:    queries,
			vault:      vault,
			masterKey:  r.masterKey,
			active:     true,
			locked:     make(map[int32]configurationapp.CurrentConfiguration),
			secretFree: secretFree,
		}
		defer store.destroy()

		callbackErr = fn(store)
		if callbackErr != nil {
			return callbackErr
		}
		if !store.complete() {
			callbackErr = ErrInvalidCurrentConfigurationTransaction
			return callbackErr
		}
		return nil
	})
	if callbackErr != nil {
		return callbackErr
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if errors.Is(err, ErrCurrentVaultUnavailable) || errors.Is(err, ErrInvalidCurrentVaultMutation) {
			return err
		}
		return ErrCurrentConfigurationMutationUnavailable
	}
	return nil
}

// Destroy clears the repository-owned master-key copy. It is a shutdown
// operation and must not race with an active mutation.
func (r *CurrentConfigurationMutationRepository) Destroy() {
	if r == nil {
		return
	}
	clearCurrentVaultBytes(r.masterKey)
	r.masterKey = nil
	r.projects = nil
	r.queries = nil
}

type currentConfigurationMutationStore struct {
	ctx       context.Context
	projectID int32
	tx        sqlExecutor
	queries   currentConfigurationMutationQueries
	vault     *lockedCurrentSecretVault
	masterKey []byte
	active    bool
	// secretFree refuses every hidden secret (WithinSecretFreeConfigurationMutation).
	secretFree bool

	locked            map[int32]configurationapp.CurrentConfiguration
	rowOperation      configurationapp.CurrentConfigurationLifecycleOperation
	rowConfiguration  configurationapp.CurrentConfiguration
	rowMutationCount  int
	putSecretsCalls   int
	hiddenMutations   []currentLifecycleHiddenMutation
	lifecycleAppended bool
}

func (s *currentConfigurationMutationStore) GetForMutation(
	ctx context.Context,
	configurationID int32,
) (configurationapp.CurrentConfiguration, error) {
	if err := s.validate(ctx); err != nil {
		return configurationapp.CurrentConfiguration{}, err
	}
	if configurationID <= 0 || s.rowMutationCount != 0 {
		return configurationapp.CurrentConfiguration{}, ErrInvalidCurrentConfigurationTransaction
	}
	row, err := s.queries.LockCurrentConfigurationForMutation(ctx, sqlcgen.LockCurrentConfigurationForMutationParams{
		ConfigurationID: configurationID,
		ProjectID:       s.projectID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return configurationapp.CurrentConfiguration{}, configurationapp.ErrCurrentConfigurationNotFound
	}
	if err != nil {
		return configurationapp.CurrentConfiguration{}, currentConfigurationStorageError(ctx, err)
	}
	configuration, err := mapCurrentConfiguration(sqlcgen.GetCurrentConfigurationRow(row))
	if err != nil || configuration.ProjectID != s.projectID || configuration.ID != configurationID {
		return configurationapp.CurrentConfiguration{}, ErrCurrentConfigurationMutationUnavailable
	}
	s.locked[configurationID] = configuration
	return cloneCurrentConfigurationForPersistence(configuration), nil
}

func (s *currentConfigurationMutationStore) InsertConfiguration(
	ctx context.Context,
	input configurationapp.CurrentConfigurationCreate,
) (configurationapp.CurrentConfiguration, error) {
	if err := s.validate(ctx); err != nil {
		return configurationapp.CurrentConfiguration{}, err
	}
	if s.rowMutationCount != 0 || input.ProjectID != s.projectID {
		return configurationapp.CurrentConfiguration{}, ErrInvalidCurrentConfigurationTransaction
	}
	data, meta, err := encodeCurrentConfigurationJSON(input.Data, input.Meta)
	if err != nil {
		return configurationapp.CurrentConfiguration{}, ErrInvalidCurrentConfigurationTransaction
	}
	row, err := s.queries.InsertCurrentConfiguration(ctx, sqlcgen.InsertCurrentConfigurationParams{
		ConfigurationUuid: input.UUID,
		ProjectID:         input.ProjectID,
		Label:             input.Label,
		EliteaTitle:       input.EliteaTitle,
		ConfigurationType: input.Type,
		Section:           input.Section,
		Data:              data,
		Meta:              meta,
		Shared:            input.Shared,
		StatusOk:          input.StatusOK,
		StatusLogs:        input.StatusLogs,
		Source:            input.Source,
		AuthorID:          input.AuthorID,
	})
	if err != nil {
		return configurationapp.CurrentConfiguration{}, currentConfigurationWriteError(ctx, err, false)
	}
	configuration, err := mapCurrentConfiguration(sqlcgen.GetCurrentConfigurationRow(row))
	if err != nil || configuration.ProjectID != s.projectID || configuration.UUID != input.UUID {
		return configurationapp.CurrentConfiguration{}, ErrCurrentConfigurationMutationUnavailable
	}
	s.recordRowMutation(configurationapp.CurrentConfigurationCreated, configuration)
	return cloneCurrentConfigurationForPersistence(configuration), nil
}

func (s *currentConfigurationMutationStore) ReplaceConfiguration(
	ctx context.Context,
	input configurationapp.CurrentConfigurationReplace,
) (configurationapp.CurrentConfiguration, error) {
	locked, ok := s.locked[input.ConfigurationID]
	if err := s.validate(ctx); err != nil {
		return configurationapp.CurrentConfiguration{}, err
	}
	if s.rowMutationCount != 0 || input.ProjectID != s.projectID || !ok {
		return configurationapp.CurrentConfiguration{}, ErrInvalidCurrentConfigurationTransaction
	}
	data, meta, err := encodeCurrentConfigurationJSON(input.Data, input.Meta)
	if err != nil {
		return configurationapp.CurrentConfiguration{}, ErrInvalidCurrentConfigurationTransaction
	}
	row, err := s.queries.ReplaceCurrentConfiguration(ctx, sqlcgen.ReplaceCurrentConfigurationParams{
		Label:           input.Label,
		EliteaTitle:     input.EliteaTitle,
		Data:            data,
		Meta:            meta,
		Shared:          input.Shared,
		StatusOk:        input.StatusOK,
		StatusLogs:      input.StatusLogs,
		ConfigurationID: input.ConfigurationID,
		ProjectID:       input.ProjectID,
	})
	if err != nil {
		return configurationapp.CurrentConfiguration{}, currentConfigurationWriteError(ctx, err, true)
	}
	configuration, err := mapCurrentConfiguration(sqlcgen.GetCurrentConfigurationRow(row))
	if err != nil || configuration.ProjectID != s.projectID || configuration.ID != locked.ID || configuration.UUID != locked.UUID {
		return configurationapp.CurrentConfiguration{}, ErrCurrentConfigurationMutationUnavailable
	}
	s.recordRowMutation(configurationapp.CurrentConfigurationUpdated, configuration)
	return cloneCurrentConfigurationForPersistence(configuration), nil
}

func (s *currentConfigurationMutationStore) DeleteConfiguration(ctx context.Context, configurationID int32) error {
	locked, ok := s.locked[configurationID]
	if err := s.validate(ctx); err != nil {
		return err
	}
	if s.rowMutationCount != 0 || configurationID <= 0 || !ok {
		return ErrInvalidCurrentConfigurationTransaction
	}
	deletedID, err := s.queries.DeleteCurrentConfiguration(ctx, sqlcgen.DeleteCurrentConfigurationParams{
		ConfigurationID: configurationID,
		ProjectID:       s.projectID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return configurationapp.ErrCurrentConfigurationNotFound
	}
	if err != nil {
		return currentConfigurationStorageError(ctx, err)
	}
	if deletedID != configurationID {
		return ErrCurrentConfigurationMutationUnavailable
	}
	s.recordRowMutation(configurationapp.CurrentConfigurationDeleted, locked)
	return nil
}

func (s *currentConfigurationMutationStore) PutHiddenSecrets(
	ctx context.Context,
	mutations []configurationapp.HiddenSecretMutation,
) error {
	if err := s.validate(ctx); err != nil {
		return err
	}
	if s.putSecretsCalls != 0 || len(mutations) > maxCurrentVaultMutations || (s.secretFree && len(mutations) != 0) {
		return ErrInvalidCurrentConfigurationTransaction
	}
	s.putSecretsCalls++
	if len(mutations) == 0 {
		return nil
	}
	vaultMutations := make([]centrysecrets.Mutation, len(mutations))
	lifecycleMutations := make([]currentLifecycleHiddenMutation, len(mutations))
	for index, mutation := range mutations {
		if mutation.Field == "" || len(mutation.Path) == 0 || mutation.Name == "" || mutation.Value == "" {
			clearCurrentLifecycleMutations(vaultMutations)
			return ErrInvalidCurrentConfigurationTransaction
		}
		vaultMutations[index] = centrysecrets.Mutation{
			Collection: centrysecrets.HiddenSecrets,
			Name:       mutation.Name,
			Value:      mutation.Value,
		}
		lifecycleMutations[index] = currentLifecycleHiddenMutation{
			field: mutation.Field,
			path:  append([]string(nil), mutation.Path...),
			name:  mutation.Name,
		}
	}
	if err := s.vault.mutate(ctx, s.tx, s.masterKey, vaultMutations); err != nil {
		clearCurrentLifecycleMutations(vaultMutations)
		return err
	}
	clearCurrentLifecycleMutations(vaultMutations)
	s.hiddenMutations = lifecycleMutations
	return nil
}

func (s *currentConfigurationMutationStore) AppendLifecycleIntent(
	ctx context.Context,
	intent configurationapp.CurrentConfigurationLifecycleIntent,
) error {
	if err := s.validate(ctx); err != nil {
		return err
	}
	if s.lifecycleAppended || s.rowMutationCount != 1 {
		return ErrInvalidCurrentConfigurationTransaction
	}
	if s.rowOperation != configurationapp.CurrentConfigurationDeleted && s.putSecretsCalls != 1 {
		return ErrInvalidCurrentConfigurationTransaction
	}
	configurationUUID, err := s.validateIntent(intent)
	if err != nil {
		return err
	}
	snapshot, err := configurationapp.EncodeCurrentConfigurationLifecycleIntent(intent)
	if err != nil || len(snapshot) == 0 || len(snapshot) > maxCurrentLifecycleSnapshotBytes {
		return ErrInvalidCurrentConfigurationTransaction
	}
	// The application schema sanitizer owns existing-field classification. This
	// boundary independently proves that every new plaintext vault mutation is
	// represented at its schema-derived field path by the exact sealed reference.
	if !currentLifecycleSecretsAreSealed(intent, s.hiddenMutations) {
		clearCurrentVaultBytes(snapshot)
		return ErrInvalidCurrentConfigurationTransaction
	}
	digest := sha256.Sum256(snapshot)

	revision, err := s.queries.GetLatestConfigurationLifecycleRevision(ctx, sqlcgen.GetLatestConfigurationLifecycleRevisionParams{
		ProjectID:         s.projectID,
		ConfigurationUuid: configurationUUID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		revision = 0
	} else if err != nil {
		clearCurrentVaultBytes(snapshot)
		return currentConfigurationStorageError(ctx, err)
	}
	if revision < 0 || revision == math.MaxInt64 {
		clearCurrentVaultBytes(snapshot)
		return ErrCurrentConfigurationMutationUnavailable
	}
	revision++
	err = s.queries.InsertConfigurationLifecycleEvent(ctx, sqlcgen.InsertConfigurationLifecycleEventParams{
		EventID:           intent.ID,
		ProjectID:         s.projectID,
		ConfigurationUuid: configurationUUID,
		Revision:          revision,
		Operation:         string(intent.Operation),
		ActorID:           intent.ActorID,
		SanitizedSnapshot: snapshot,
		SnapshotDigest:    digest[:],
	})
	clearCurrentVaultBytes(snapshot)
	if err != nil {
		return currentConfigurationStorageError(ctx, err)
	}
	s.lifecycleAppended = true
	return nil
}

func (s *currentConfigurationMutationStore) validateIntent(
	intent configurationapp.CurrentConfigurationLifecycleIntent,
) (string, error) {
	if !validCurrentPersistenceUUID(intent.ID, true) || intent.ActorID <= 0 || intent.Operation != s.rowOperation {
		return "", ErrInvalidCurrentConfigurationTransaction
	}
	row := s.rowConfiguration
	if !validCurrentPersistenceUUID(row.UUID, false) || row.ProjectID != s.projectID || row.ID <= 0 {
		return "", ErrCurrentConfigurationMutationUnavailable
	}
	sameIdentity := func(snapshot *configurationapp.CurrentConfigurationLifecycleSnapshot) bool {
		return snapshot != nil && snapshot.ID == row.ID && snapshot.UUID == row.UUID &&
			snapshot.ProjectID == row.ProjectID && snapshot.Type == row.Type &&
			snapshot.Section == row.Section && snapshot.Source == row.Source
	}
	switch intent.Operation {
	case configurationapp.CurrentConfigurationCreated:
		if intent.Before != nil || !sameIdentity(intent.After) || intent.After.Data == nil {
			return "", ErrInvalidCurrentConfigurationTransaction
		}
	case configurationapp.CurrentConfigurationUpdated:
		// A current historical row can still contain raw secret material. Its
		// metadata remains updatable, but both lifecycle snapshots deliberately
		// carry data_available=false rather than copying that material here.
		if !sameIdentity(intent.Before) || !sameIdentity(intent.After) {
			return "", ErrInvalidCurrentConfigurationTransaction
		}
	case configurationapp.CurrentConfigurationDeleted:
		if !sameIdentity(intent.Before) || intent.After != nil {
			return "", ErrInvalidCurrentConfigurationTransaction
		}
	default:
		return "", ErrInvalidCurrentConfigurationTransaction
	}
	return row.UUID, nil
}

func (s *currentConfigurationMutationStore) recordRowMutation(
	operation configurationapp.CurrentConfigurationLifecycleOperation,
	configuration configurationapp.CurrentConfiguration,
) {
	s.rowOperation = operation
	s.rowConfiguration = cloneCurrentConfigurationForPersistence(configuration)
	s.rowMutationCount++
}

func (s *currentConfigurationMutationStore) validate(ctx context.Context) error {
	if s == nil || !s.active || s.ctx == nil || s.tx == nil || s.queries == nil || s.vault == nil || ctx == nil {
		return ErrInvalidCurrentConfigurationTransaction
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

func (s *currentConfigurationMutationStore) complete() bool {
	return s != nil && s.active && s.rowMutationCount == 1 && s.lifecycleAppended
}

func (s *currentConfigurationMutationStore) destroy() {
	if s == nil {
		return
	}
	s.active = false
	if s.vault != nil {
		s.vault.destroy()
	}
	clearCurrentLifecycleHiddenMutations(s.hiddenMutations)
	s.hiddenMutations = nil
	s.masterKey = nil
	s.tx = nil
	s.queries = nil
	s.locked = nil
	s.ctx = nil
}

type currentLifecycleHiddenMutation struct {
	field string
	path  []string
	name  string
}

func currentLifecycleSecretsAreSealed(
	intent configurationapp.CurrentConfigurationLifecycleIntent,
	mutations []currentLifecycleHiddenMutation,
) bool {
	if len(mutations) == 0 {
		return true
	}
	if intent.After == nil || intent.After.Data == nil {
		return false
	}
	for _, mutation := range mutations {
		if mutation.field == "" || mutation.name == "" || len(mutation.path) == 0 {
			return false
		}
		var value any = intent.After.Data
		for _, segment := range mutation.path {
			object, ok := value.(map[string]any)
			if !ok || segment == "" {
				return false
			}
			value, ok = object[segment]
			if !ok {
				return false
			}
		}
		reference, ok := value.(string)
		if !ok || reference != "{{secret."+mutation.name+"}}" {
			return false
		}
	}
	return true
}

func currentConfigurationWriteError(ctx context.Context, err error, notFound bool) error {
	if notFound && errors.Is(err, pgx.ErrNoRows) {
		return configurationapp.ErrCurrentConfigurationNotFound
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23505" {
		return configurationapp.ErrCurrentConfigurationConflict
	}
	return currentConfigurationStorageError(ctx, err)
}

func currentConfigurationStorageError(ctx context.Context, err error) error {
	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrCurrentConfigurationMutationUnavailable
}

func validCurrentPersistenceUUID(value string, requireV4 bool) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	if requireV4 && (value[14] != '4' || (value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b')) {
		return false
	}
	for index := 0; index < len(value); index++ {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		character := value[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func cloneCurrentConfigurationForPersistence(value configurationapp.CurrentConfiguration) configurationapp.CurrentConfiguration {
	value.Label = cloneCurrentStringForPersistence(value.Label)
	value.StatusLogs = cloneCurrentStringForPersistence(value.StatusLogs)
	value.AuthorID = cloneCurrentInt32ForPersistence(value.AuthorID)
	value.Data = cloneCurrentObjectForPersistence(value.Data)
	value.Meta = cloneCurrentObjectForPersistence(value.Meta)
	if value.UpdatedAt != nil {
		updatedAt := *value.UpdatedAt
		value.UpdatedAt = &updatedAt
	}
	return value
}

func cloneCurrentObjectForPersistence(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var cloned map[string]any
	if err := decoder.Decode(&cloned); err != nil {
		return nil
	}
	return cloned
}

func cloneCurrentStringForPersistence(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneCurrentInt32ForPersistence(value *int32) *int32 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func clearCurrentLifecycleMutations(mutations []centrysecrets.Mutation) {
	for index := range mutations {
		mutations[index].Value = ""
		mutations[index].Name = ""
		mutations[index].IntegerValue = nil
	}
}

func clearCurrentLifecycleHiddenMutations(mutations []currentLifecycleHiddenMutation) {
	for index := range mutations {
		mutations[index].field = ""
		for pathIndex := range mutations[index].path {
			mutations[index].path[pathIndex] = ""
		}
		mutations[index].path = nil
		mutations[index].name = ""
	}
}

var _ configurationapp.CurrentConfigurationMutationRepository = (*CurrentConfigurationMutationRepository)(nil)
var _ configurationapp.CurrentConfigurationMutationStore = (*currentConfigurationMutationStore)(nil)
