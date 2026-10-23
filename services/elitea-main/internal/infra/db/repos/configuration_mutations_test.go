package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCurrentConfigurationMutationRepositoryCommitsSanitizedAtomicCreate(t *testing.T) {
	vaultStore := &currentVaultSharedStore{
		key:       []byte(currentVaultProjectKey),
		vault:     []byte(currentVaultToken),
		updateTag: pgconn.NewCommandTag("UPDATE 1"),
	}
	projectStore := &currentMutationProjectStore{tx: vaultStore}
	row := currentConfigurationRow(7, 21)
	row.ConfigurationUuid = "00000000-0000-0000-0000-000000000021"
	row.Data = []byte(`{"token":"{{secret.11111111111111111111111111111111}}"}`)
	row.Meta = []byte(`{}`)
	queries := &currentMutationQueriesStub{
		insertRow: sqlcgen.InsertCurrentConfigurationRow(row),
		latestErr: pgx.ErrNoRows,
	}
	repository := newCurrentConfigurationMutationRepositoryForTest(t, projectStore, queries)

	var escaped configurationapp.CurrentConfigurationMutationStore
	err := repository.WithinCurrentConfigurationMutation(context.Background(), 7, func(store configurationapp.CurrentConfigurationMutationStore) error {
		escaped = store
		created, err := store.InsertConfiguration(context.Background(), configurationapp.CurrentConfigurationCreate{
			UUID:        row.ConfigurationUuid,
			ProjectID:   7,
			Label:       row.Label,
			EliteaTitle: row.EliteaTitle,
			Type:        row.Type,
			Section:     row.Section,
			Data:        map[string]any{"token": "{{secret.11111111111111111111111111111111}}"},
			Meta:        map[string]any{},
			Shared:      row.Shared,
			StatusOK:    row.StatusOk,
			StatusLogs:  row.StatusLogs,
			Source:      row.Source,
			AuthorID:    row.AuthorID,
		})
		if err != nil {
			return err
		}
		if err := store.PutHiddenSecrets(context.Background(), []configurationapp.HiddenSecretMutation{{
			Field: "token",
			Path:  []string{"token"},
			Name:  "11111111111111111111111111111111",
			Value: "plaintext-must-not-be-durable",
		}}); err != nil {
			return err
		}
		return store.AppendLifecycleIntent(context.Background(), configurationapp.CurrentConfigurationLifecycleIntent{
			ID:        "11111111-1111-4111-8111-111111111111",
			Operation: configurationapp.CurrentConfigurationCreated,
			ActorID:   42,
			After:     currentMutationLifecycleSnapshot(created, created.Data),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !projectStore.committed || projectStore.projectID != 7 || projectStore.options.AccessMode != pgx.ReadWrite {
		t.Fatalf("transaction committed=%v project=%d options=%#v", projectStore.committed, projectStore.projectID, projectStore.options)
	}
	if queries.insertEvent.ProjectID != 7 || queries.insertEvent.ConfigurationUuid != row.ConfigurationUuid ||
		queries.insertEvent.Revision != 1 || queries.insertEvent.Operation != string(configurationapp.CurrentConfigurationCreated) ||
		queries.insertEvent.ActorID != 42 {
		t.Fatalf("lifecycle event=%#v", queries.insertEvent)
	}
	if bytes.Contains(queries.insertEvent.SanitizedSnapshot, []byte("plaintext-must-not-be-durable")) ||
		!bytes.Contains(queries.insertEvent.SanitizedSnapshot, []byte("{{secret.11111111111111111111111111111111}}")) ||
		!bytes.Contains(queries.insertEvent.SanitizedSnapshot, []byte(`"data_available":true`)) ||
		bytes.Contains(queries.insertEvent.SanitizedSnapshot, []byte("status_logs")) ||
		bytes.Contains(queries.insertEvent.SanitizedSnapshot, []byte("meta")) {
		t.Fatalf("unsafe lifecycle snapshot=%s", queries.insertEvent.SanitizedSnapshot)
	}
	digest := sha256.Sum256(queries.insertEvent.SanitizedSnapshot)
	if !bytes.Equal(queries.insertEvent.SnapshotDigest, digest[:]) {
		t.Fatalf("snapshot digest=%x want=%x", queries.insertEvent.SnapshotDigest, digest)
	}

	rewritten, ok := vaultStore.execArgs[1].([]byte)
	if !ok {
		t.Fatalf("vault rewrite=%T", vaultStore.execArgs[1])
	}
	vault, err := centrysecrets.OpenUnwrapped([]byte(currentVaultProjectKey), rewritten)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Lookup("11111111111111111111111111111111")
	if err != nil || secret.Value != "plaintext-must-not-be-durable" || !secret.Hidden {
		t.Fatalf("stored secret=%#v err=%v", secret, err)
	}
	if _, err := escaped.GetForMutation(context.Background(), 21); !errors.Is(err, ErrInvalidCurrentConfigurationTransaction) {
		t.Fatalf("escaped store remained active: %v", err)
	}
}

func TestCurrentConfigurationMutationRepositoryRollsBackIncompleteAndRedactsFailures(t *testing.T) {
	tests := []struct {
		name string
		run  func(configurationapp.CurrentConfigurationMutationStore) error
	}{
		{
			name: "missing lifecycle intent",
			run: func(store configurationapp.CurrentConfigurationMutationStore) error {
				_, err := store.InsertConfiguration(context.Background(), currentMutationCreateInput())
				return err
			},
		},
		{
			name: "dependency failure",
			run: func(store configurationapp.CurrentConfigurationMutationStore) error {
				created, err := store.InsertConfiguration(context.Background(), currentMutationCreateInput())
				if err != nil {
					return err
				}
				if err := store.PutHiddenSecrets(context.Background(), nil); err != nil {
					return err
				}
				return store.AppendLifecycleIntent(context.Background(), configurationapp.CurrentConfigurationLifecycleIntent{
					ID:        "22222222-2222-4222-8222-222222222222",
					Operation: configurationapp.CurrentConfigurationCreated,
					ActorID:   42,
					After:     currentMutationLifecycleSnapshot(created, created.Data),
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			vaultStore := &currentVaultSharedStore{
				key: []byte(currentVaultProjectKey), vault: []byte(currentVaultToken),
				updateTag: pgconn.NewCommandTag("UPDATE 1"),
			}
			projectStore := &currentMutationProjectStore{tx: vaultStore}
			row := currentConfigurationRow(7, 21)
			row.ConfigurationUuid = currentMutationCreateInput().UUID
			row.Data = []byte(`{}`)
			row.Meta = []byte(`{}`)
			queries := &currentMutationQueriesStub{
				insertRow: sqlcgen.InsertCurrentConfigurationRow(row),
				latestErr: pgx.ErrNoRows,
			}
			if test.name == "dependency failure" {
				queries.insertEventErr = errors.New("database detail with plaintext-must-not-be-durable")
			}
			repository := newCurrentConfigurationMutationRepositoryForTest(t, projectStore, queries)
			err := repository.WithinCurrentConfigurationMutation(context.Background(), 7, test.run)
			if test.name == "missing lifecycle intent" {
				if !errors.Is(err, ErrInvalidCurrentConfigurationTransaction) {
					t.Fatalf("incomplete error=%v", err)
				}
			} else if !errors.Is(err, ErrCurrentConfigurationMutationUnavailable) || strings.Contains(err.Error(), "database detail") {
				t.Fatalf("dependency error=%v", err)
			}
			if projectStore.committed {
				t.Fatal("failed mutation committed")
			}
		})
	}
}

func TestCurrentConfigurationMutationRepositoryOrdersUpdateAndDeleteRevisions(t *testing.T) {
	tests := []struct {
		name         string
		latest       int64
		operation    configurationapp.CurrentConfigurationLifecycleOperation
		run          func(context.Context, configurationapp.CurrentConfigurationMutationStore) error
		wantRevision int64
	}{
		{
			name:      "update",
			latest:    4,
			operation: configurationapp.CurrentConfigurationUpdated,
			run: func(ctx context.Context, store configurationapp.CurrentConfigurationMutationStore) error {
				existing, err := store.GetForMutation(ctx, 21)
				if err != nil {
					return err
				}
				updated, err := store.ReplaceConfiguration(ctx, configurationapp.CurrentConfigurationReplace{
					ProjectID: 7, ConfigurationID: 21, Label: existing.Label,
					EliteaTitle: existing.EliteaTitle, Data: map[string]any{"updated": true}, Meta: existing.Meta,
					Shared: existing.Shared, StatusOK: existing.StatusOK, StatusLogs: existing.StatusLogs,
				})
				if err != nil {
					return err
				}
				if err := store.PutHiddenSecrets(ctx, nil); err != nil {
					return err
				}
				return store.AppendLifecycleIntent(ctx, configurationapp.CurrentConfigurationLifecycleIntent{
					ID: "44444444-4444-4444-8444-444444444444", Operation: configurationapp.CurrentConfigurationUpdated, ActorID: 42,
					Before: currentMutationLifecycleSnapshot(existing, existing.Data), After: currentMutationLifecycleSnapshot(updated, updated.Data),
				})
			},
			wantRevision: 5,
		},
		{
			name:      "delete",
			latest:    7,
			operation: configurationapp.CurrentConfigurationDeleted,
			run: func(ctx context.Context, store configurationapp.CurrentConfigurationMutationStore) error {
				existing, err := store.GetForMutation(ctx, 21)
				if err != nil {
					return err
				}
				if err := store.DeleteConfiguration(ctx, 21); err != nil {
					return err
				}
				return store.AppendLifecycleIntent(ctx, configurationapp.CurrentConfigurationLifecycleIntent{
					ID: "55555555-5555-4555-9555-555555555555", Operation: configurationapp.CurrentConfigurationDeleted, ActorID: 42,
					Before: currentMutationLifecycleSnapshot(existing, nil),
				})
			},
			wantRevision: 8,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			vaultStore := &currentVaultSharedStore{key: []byte(currentVaultProjectKey), vault: []byte(currentVaultToken)}
			projectStore := &currentMutationProjectStore{tx: vaultStore}
			row := currentConfigurationRow(7, 21)
			row.Data = []byte(`{"updated":true}`)
			row.Meta = []byte(`{}`)
			queries := &currentMutationQueriesStub{
				lockRow:    sqlcgen.LockCurrentConfigurationForMutationRow(row),
				replaceRow: sqlcgen.ReplaceCurrentConfigurationRow(row),
				deleteID:   21,
				latest:     test.latest,
			}
			repository := newCurrentConfigurationMutationRepositoryForTest(t, projectStore, queries)
			if err := repository.WithinCurrentConfigurationMutation(context.Background(), 7, func(store configurationapp.CurrentConfigurationMutationStore) error {
				return test.run(context.Background(), store)
			}); err != nil {
				t.Fatal(err)
			}
			if !projectStore.committed || queries.insertEvent.Revision != test.wantRevision || queries.insertEvent.Operation != string(test.operation) {
				t.Fatalf("committed=%v event=%#v", projectStore.committed, queries.insertEvent)
			}
			if test.operation == configurationapp.CurrentConfigurationDeleted &&
				!bytes.Contains(queries.insertEvent.SanitizedSnapshot, []byte(`"data_available":false`)) {
				t.Fatalf("delete snapshot did not signal missing cleanup data: %s", queries.insertEvent.SanitizedSnapshot)
			}
			if len(vaultStore.execArgs) != 0 {
				t.Fatalf("empty hidden-secret mutation rewrote vault: %#v", vaultStore.execArgs)
			}
		})
	}
}

func TestCurrentConfigurationMutationRepositoryKeepsHistoricalRawDataOutOfUpdateEvent(t *testing.T) {
	vaultStore := &currentVaultSharedStore{key: []byte(currentVaultProjectKey), vault: []byte(currentVaultToken)}
	projectStore := &currentMutationProjectStore{tx: vaultStore}
	row := currentConfigurationRow(7, 21)
	row.Data = []byte(`{"access_token":"historical-raw-value"}`)
	row.Meta = []byte(`{}`)
	queries := &currentMutationQueriesStub{
		lockRow:    sqlcgen.LockCurrentConfigurationForMutationRow(row),
		replaceRow: sqlcgen.ReplaceCurrentConfigurationRow(row),
		latest:     1,
	}
	repository := newCurrentConfigurationMutationRepositoryForTest(t, projectStore, queries)
	err := repository.WithinCurrentConfigurationMutation(context.Background(), 7, func(store configurationapp.CurrentConfigurationMutationStore) error {
		existing, err := store.GetForMutation(context.Background(), 21)
		if err != nil {
			return err
		}
		updated, err := store.ReplaceConfiguration(context.Background(), configurationapp.CurrentConfigurationReplace{
			ProjectID: 7, ConfigurationID: 21, Label: existing.Label,
			EliteaTitle: existing.EliteaTitle, Data: existing.Data, Meta: existing.Meta,
			Shared: existing.Shared, StatusOK: existing.StatusOK, StatusLogs: existing.StatusLogs,
		})
		if err != nil {
			return err
		}
		if err := store.PutHiddenSecrets(context.Background(), nil); err != nil {
			return err
		}
		return store.AppendLifecycleIntent(context.Background(), configurationapp.CurrentConfigurationLifecycleIntent{
			ID: "66666666-6666-4666-a666-666666666666", Operation: configurationapp.CurrentConfigurationUpdated, ActorID: 42,
			Before: currentMutationLifecycleSnapshot(existing, nil),
			After:  currentMutationLifecycleSnapshot(updated, nil),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !projectStore.committed || bytes.Contains(queries.insertEvent.SanitizedSnapshot, []byte("historical-raw-value")) ||
		bytes.Count(queries.insertEvent.SanitizedSnapshot, []byte(`"data_available":false`)) != 2 {
		t.Fatalf("unsafe historical update snapshot=%s committed=%v", queries.insertEvent.SanitizedSnapshot, projectStore.committed)
	}
}

func TestCurrentLifecycleSecretVerificationUsesSchemaFieldPath(t *testing.T) {
	intent := configurationapp.CurrentConfigurationLifecycleIntent{
		After: &configurationapp.CurrentConfigurationLifecycleSnapshot{Data: map[string]any{
			"auth": map[string]any{
				"token": "{{secret.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}}",
			},
			"display_name": "a legitimate value may match secret plaintext",
		}},
	}
	mutation := []currentLifecycleHiddenMutation{{
		field: "auth.token",
		path:  []string{"auth", "token"},
		name:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}
	if !currentLifecycleSecretsAreSealed(intent, mutation) {
		t.Fatal("exact nested hidden-secret reference was rejected")
	}
	intent.After.Data["auth"].(map[string]any)["token"] = "raw<json-escaped>value"
	if currentLifecycleSecretsAreSealed(intent, mutation) {
		t.Fatal("raw nested password was accepted")
	}

	intent.After.Data = map[string]any{
		"auth.token": "{{secret.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}}",
		"auth":       map[string]any{"token": "wrong-location"},
	}
	mutation = []currentLifecycleHiddenMutation{{
		field: "auth.token",
		path:  []string{"auth.token"},
		name:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}}
	if !currentLifecycleSecretsAreSealed(intent, mutation) {
		t.Fatal("literal dotted field path was split during secret verification")
	}
}

func TestCurrentLifecycleSnapshotKeepsAdmittedPromptBounded(t *testing.T) {
	prompt := strings.Repeat("<", 1024*1024-4096)
	encoded, err := configurationapp.EncodeCurrentConfigurationLifecycleIntent(configurationapp.CurrentConfigurationLifecycleIntent{
		Operation: configurationapp.CurrentConfigurationUpdated,
		ActorID:   42,
		After: &configurationapp.CurrentConfigurationLifecycleSnapshot{
			ID: 1, UUID: "00000000-0000-0000-0000-000000000001", ProjectID: 7,
			EliteaTitle: "service_prompt", Type: "service_prompt", Section: "settings", Source: "user",
			Data: map[string]any{"prompt": prompt},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxCurrentLifecycleSnapshotBytes || bytes.Contains(encoded, []byte(`\u003c`)) {
		t.Fatalf("encoded prompt bytes=%d escaped_html=%v", len(encoded), bytes.Contains(encoded, []byte(`\u003c`)))
	}
}

func TestCurrentConfigurationLifecycleMigrationIsBoundedAndTenantIndependent(t *testing.T) {
	migration, err := os.ReadFile("../../../../migrations/shared/0040_configuration_lifecycle_outbox.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(migration)
	for _, required := range []string{
		"elitea_runtime.configuration_lifecycle_outbox",
		"UNIQUE (resource_project_id, configuration_uuid, revision)",
		"octet_length(sanitized_snapshot::text) BETWEEN 2 AND 2097152",
		"attempt_count BETWEEN 0 AND 1000",
		"configuration_lifecycle_outbox_ready_idx",
		"configuration_lifecycle_outbox_lease_expiry_idx",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("migration missing %q", required)
		}
	}
	for _, forbidden := range []string{"REFERENCES configuration", "REFERENCES p_", "plaintext"} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("migration contains forbidden tenant/secret material %q", forbidden)
		}
	}
}

func newCurrentConfigurationMutationRepositoryForTest(
	t *testing.T,
	projects projectStore,
	queries currentConfigurationMutationQueries,
) *CurrentConfigurationMutationRepository {
	t.Helper()
	repository, err := newCurrentConfigurationMutationRepository(
		projects,
		func(sqlExecutor) (currentConfigurationMutationQueries, error) { return queries, nil },
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func currentMutationCreateInput() configurationapp.CurrentConfigurationCreate {
	return configurationapp.CurrentConfigurationCreate{
		UUID:        "00000000-0000-0000-0000-000000000021",
		ProjectID:   7,
		EliteaTitle: "github-team",
		Type:        "github",
		Section:     "credentials",
		Data:        map[string]any{},
		Meta:        map[string]any{},
		Source:      "user",
	}
}

func currentMutationLifecycleSnapshot(
	configuration configurationapp.CurrentConfiguration,
	data map[string]any,
) *configurationapp.CurrentConfigurationLifecycleSnapshot {
	return &configurationapp.CurrentConfigurationLifecycleSnapshot{
		ID:          configuration.ID,
		UUID:        configuration.UUID,
		ProjectID:   configuration.ProjectID,
		EliteaTitle: configuration.EliteaTitle,
		Type:        configuration.Type,
		Section:     configuration.Section,
		Label:       configuration.Label,
		Shared:      configuration.Shared,
		StatusOK:    configuration.StatusOK,
		Source:      configuration.Source,
		AuthorID:    configuration.AuthorID,
		Data:        data,
	}
}

type currentMutationProjectStore struct {
	tx        sqlExecutor
	projectID int64
	options   pgx.TxOptions
	committed bool
	commitErr error
}

func (s *currentMutationProjectStore) WithinProjectTx(
	_ context.Context,
	projectID int64,
	options pgx.TxOptions,
	fn func(sqlExecutor) error,
) error {
	s.projectID = projectID
	s.options = options
	if err := fn(s.tx); err != nil {
		return err
	}
	if s.commitErr != nil {
		return s.commitErr
	}
	s.committed = true
	return nil
}

type currentMutationQueriesStub struct {
	lockRow        sqlcgen.LockCurrentConfigurationForMutationRow
	lockErr        error
	insertRow      sqlcgen.InsertCurrentConfigurationRow
	insertErr      error
	replaceRow     sqlcgen.ReplaceCurrentConfigurationRow
	replaceErr     error
	deleteID       int32
	deleteErr      error
	latest         int64
	latestErr      error
	insertEvent    sqlcgen.InsertConfigurationLifecycleEventParams
	insertEventErr error
}

func (s *currentMutationQueriesStub) LockCurrentConfigurationForMutation(
	context.Context,
	sqlcgen.LockCurrentConfigurationForMutationParams,
) (sqlcgen.LockCurrentConfigurationForMutationRow, error) {
	return s.lockRow, s.lockErr
}

func (s *currentMutationQueriesStub) InsertCurrentConfiguration(
	context.Context,
	sqlcgen.InsertCurrentConfigurationParams,
) (sqlcgen.InsertCurrentConfigurationRow, error) {
	return s.insertRow, s.insertErr
}

func (s *currentMutationQueriesStub) ReplaceCurrentConfiguration(
	context.Context,
	sqlcgen.ReplaceCurrentConfigurationParams,
) (sqlcgen.ReplaceCurrentConfigurationRow, error) {
	return s.replaceRow, s.replaceErr
}

func (s *currentMutationQueriesStub) DeleteCurrentConfiguration(
	context.Context,
	sqlcgen.DeleteCurrentConfigurationParams,
) (int32, error) {
	return s.deleteID, s.deleteErr
}

func (s *currentMutationQueriesStub) GetLatestConfigurationLifecycleRevision(
	context.Context,
	sqlcgen.GetLatestConfigurationLifecycleRevisionParams,
) (int64, error) {
	return s.latest, s.latestErr
}

func (s *currentMutationQueriesStub) InsertConfigurationLifecycleEvent(
	_ context.Context,
	params sqlcgen.InsertConfigurationLifecycleEventParams,
) error {
	s.insertEvent = params
	s.insertEvent.SanitizedSnapshot = append([]byte(nil), params.SanitizedSnapshot...)
	s.insertEvent.SnapshotDigest = append([]byte(nil), params.SnapshotDigest...)
	return s.insertEventErr
}
