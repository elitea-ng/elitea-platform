package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentConfigurationMutationRepositoryPostgresAtomicityAndOrdering(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	prepareCurrentMutationVault(t, pool)

	repository, err := NewCurrentConfigurationMutationRepository(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repository.Destroy)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	committedSecret := "committed-secret-value"
	created, err := createCurrentConfigurationAtomically(ctx, repository, "00000000-0000-0000-0000-000000000201", "11111111-1111-4111-8111-111111111111", "committed_config", "11111111111111111111111111111111", committedSecret)
	if err != nil {
		t.Fatalf("commit atomic configuration: %v", err)
	}
	if created.ID <= 0 {
		t.Fatalf("created configuration=%#v", created)
	}

	var storedTitle string
	if err := pool.QueryRow(ctx, `SELECT elitea_title FROM p_1.configuration WHERE uuid = $1`, created.UUID).Scan(&storedTitle); err != nil || storedTitle != "committed_config" {
		t.Fatalf("stored title=%q err=%v", storedTitle, err)
	}
	assertCurrentLifecycleEvent(t, pool, created.UUID, 1, committedSecret)
	assertCurrentVaultSecret(t, pool, "11111111111111111111111111111111", committedSecret, true)

	injected := errors.New("fault after all atomic writes")
	rollbackUUID := "00000000-0000-0000-0000-000000000202"
	rollbackSecretName := "22222222222222222222222222222222"
	rollbackSecret := "rollback-secret-value"
	err = repository.WithinCurrentConfigurationMutation(ctx, 1, func(store configurationapp.CurrentConfigurationMutationStore) error {
		created, err := store.InsertConfiguration(ctx, currentPostgresCreateInput(rollbackUUID, "rollback_config", rollbackSecretName))
		if err != nil {
			return err
		}
		if err := store.PutHiddenSecrets(ctx, []configurationapp.HiddenSecretMutation{{Field: "token", Path: []string{"token"}, Name: rollbackSecretName, Value: rollbackSecret}}); err != nil {
			return err
		}
		if err := store.AppendLifecycleIntent(ctx, configurationapp.CurrentConfigurationLifecycleIntent{
			ID:        "22222222-2222-4222-8222-222222222222",
			Operation: configurationapp.CurrentConfigurationCreated,
			ActorID:   42,
			After:     currentMutationLifecycleSnapshot(created, created.Data),
		}); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("fault error=%v", err)
	}
	var rolledBackRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.configuration WHERE uuid = $1`, rollbackUUID).Scan(&rolledBackRows); err != nil || rolledBackRows != 0 {
		t.Fatalf("rolled-back configuration count=%d err=%v", rolledBackRows, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.configuration_lifecycle_outbox WHERE configuration_uuid = $1`, rollbackUUID).Scan(&rolledBackRows); err != nil || rolledBackRows != 0 {
		t.Fatalf("rolled-back lifecycle count=%d err=%v", rolledBackRows, err)
	}
	assertCurrentVaultSecret(t, pool, rollbackSecretName, rollbackSecret, false)

	concurrentCurrentConfigurationUpdates(t, ctx, pool, repository)
	assertCurrentLifecycleSchema(t, ctx, pool)
}

func createCurrentConfigurationAtomically(
	ctx context.Context,
	repository *CurrentConfigurationMutationRepository,
	configurationUUID string,
	eventID string,
	title string,
	secretName string,
	secretValue string,
) (configurationapp.CurrentConfiguration, error) {
	var created configurationapp.CurrentConfiguration
	err := repository.WithinCurrentConfigurationMutation(ctx, 1, func(store configurationapp.CurrentConfigurationMutationStore) error {
		var err error
		created, err = store.InsertConfiguration(ctx, currentPostgresCreateInput(configurationUUID, title, secretName))
		if err != nil {
			return err
		}
		if err := store.PutHiddenSecrets(ctx, []configurationapp.HiddenSecretMutation{{Field: "token", Path: []string{"token"}, Name: secretName, Value: secretValue}}); err != nil {
			return err
		}
		return store.AppendLifecycleIntent(ctx, configurationapp.CurrentConfigurationLifecycleIntent{
			ID:        eventID,
			Operation: configurationapp.CurrentConfigurationCreated,
			ActorID:   42,
			After:     currentMutationLifecycleSnapshot(created, created.Data),
		})
	})
	return created, err
}

func currentPostgresCreateInput(configurationUUID, title, secretName string) configurationapp.CurrentConfigurationCreate {
	label := "Integration lifecycle configuration"
	authorID := int32(42)
	return configurationapp.CurrentConfigurationCreate{
		UUID:        configurationUUID,
		ProjectID:   1,
		Label:       &label,
		EliteaTitle: title,
		Type:        "github",
		Section:     "credentials",
		Data:        map[string]any{"token": "{{secret." + secretName + "}}"},
		Meta:        map[string]any{},
		StatusOK:    true,
		Source:      "user",
		AuthorID:    &authorID,
	}
}

func prepareCurrentMutationVault(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Two Exec calls, not one: pgx always uses the extended/prepared query
	// protocol once placeholder arguments are involved, and Postgres's
	// extended protocol allows exactly one statement per prepared
	// statement — a single multi-statement string with $1/$2 args fails
	// with "cannot insert multiple commands into a prepared statement".
	// The CREATE TABLEs have no args, so they can stay combined (pgx uses
	// the simple protocol for an arg-free Exec, which does allow multiple
	// statements); each parameterized INSERT needs its own call.
	if _, err := pool.Exec(ctx, `
CREATE TABLE centry.secrets_key (
    id TEXT PRIMARY KEY,
    data BYTEA
);
CREATE TABLE centry.secrets_data (
    id TEXT PRIMARY KEY,
    data BYTEA
);`); err != nil {
		t.Fatalf("prepare current project vault schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO centry.secrets_key (id, data) VALUES ('project-1', $1)`, []byte(currentVaultProjectKey)); err != nil {
		t.Fatalf("prepare current project vault key: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO centry.secrets_data (id, data) VALUES ('project-1', $1)`, []byte(currentVaultToken)); err != nil {
		t.Fatalf("prepare current project vault data: %v", err)
	}
}

func assertCurrentLifecycleEvent(t *testing.T, pool *pgxpool.Pool, configurationUUID string, revision int64, forbiddenSecret string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var snapshot, digest []byte
	var storedRevision int64
	if err := pool.QueryRow(ctx, `
SELECT revision, sanitized_snapshot, snapshot_digest
FROM elitea_runtime.configuration_lifecycle_outbox
WHERE resource_project_id = 1 AND configuration_uuid = $1`, configurationUUID).Scan(&storedRevision, &snapshot, &digest); err != nil {
		t.Fatalf("read lifecycle event: %v", err)
	}
	if storedRevision != revision || bytes.Contains(snapshot, []byte(forbiddenSecret)) {
		t.Fatalf("revision=%d snapshot=%s", storedRevision, snapshot)
	}
	wantDigest := sha256.Sum256(snapshot)
	if !bytes.Equal(digest, wantDigest[:]) {
		t.Fatalf("stored digest=%x want=%x", digest, wantDigest)
	}
}

func assertCurrentVaultSecret(t *testing.T, pool *pgxpool.Pool, name, value string, shouldExist bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var encryptedProjectKey, encryptedVault []byte
	if err := pool.QueryRow(ctx, `
SELECT k.data, d.data
FROM centry.secrets_key AS k
JOIN centry.secrets_data AS d ON d.id = k.id
WHERE k.id = 'project-1'`).Scan(&encryptedProjectKey, &encryptedVault); err != nil {
		t.Fatalf("read current vault: %v", err)
	}
	vault, err := centrysecrets.OpenUnwrapped(encryptedProjectKey, encryptedVault)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Lookup(name)
	if shouldExist {
		if err != nil || secret.Value != value || !secret.Hidden {
			t.Fatalf("secret %q=%#v err=%v", name, secret, err)
		}
		return
	}
	if err == nil {
		t.Fatalf("rolled-back secret %q remained: %#v", name, secret)
	}
}

func concurrentCurrentConfigurationUpdates(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *CurrentConfigurationMutationRepository,
) {
	t.Helper()
	type updateResult struct {
		marker string
		err    error
	}
	start := make(chan struct{})
	results := make(chan updateResult, 2)
	eventIDs := []string{
		"33333333-3333-4333-8333-333333333331",
		"33333333-3333-4333-9333-333333333332",
	}
	var workers sync.WaitGroup
	for index := 1; index <= 2; index++ {
		marker := fmt.Sprintf("update-%d", index)
		eventID := eventIDs[index-1]
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			err := repository.WithinCurrentConfigurationMutation(ctx, 1, func(store configurationapp.CurrentConfigurationMutationStore) error {
				existing, err := store.GetForMutation(ctx, 1)
				if err != nil {
					return err
				}
				updated, err := store.ReplaceConfiguration(ctx, configurationapp.CurrentConfigurationReplace{
					ProjectID:       1,
					ConfigurationID: 1,
					Label:           &marker,
					EliteaTitle:     existing.EliteaTitle,
					Data:            map[string]any{"marker": marker},
					Meta:            existing.Meta,
					Shared:          existing.Shared,
					StatusOK:        existing.StatusOK,
					StatusLogs:      existing.StatusLogs,
				})
				if err != nil {
					return err
				}
				if err := store.PutHiddenSecrets(ctx, nil); err != nil {
					return err
				}
				return store.AppendLifecycleIntent(ctx, configurationapp.CurrentConfigurationLifecycleIntent{
					ID:        eventID,
					Operation: configurationapp.CurrentConfigurationUpdated,
					ActorID:   42,
					Before:    currentMutationLifecycleSnapshot(existing, existing.Data),
					After:     currentMutationLifecycleSnapshot(updated, updated.Data),
				})
			})
			results <- updateResult{marker: marker, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent %s failed: %v", result.marker, result.err)
		}
	}

	rows, err := pool.Query(ctx, `
SELECT revision
FROM elitea_runtime.configuration_lifecycle_outbox
WHERE resource_project_id = 1
  AND configuration_uuid = '00000000-0000-0000-0000-000000000001'
ORDER BY revision`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var revisions []int64
	for rows.Next() {
		var revision int64
		if err := rows.Scan(&revision); err != nil {
			t.Fatal(err)
		}
		revisions = append(revisions, revision)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(revisions, func(i, j int) bool { return revisions[i] < revisions[j] })
	if len(revisions) != 2 || revisions[0] != 1 || revisions[1] != 2 {
		t.Fatalf("concurrent revisions=%v", revisions)
	}
}

func assertCurrentLifecycleSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var foreignKeys int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM pg_constraint
WHERE conrelid = 'elitea_runtime.configuration_lifecycle_outbox'::regclass
  AND contype = 'f'`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 0 {
		t.Fatalf("lifecycle outbox has %d tenant/shared foreign keys", foreignKeys)
	}

	expected := []string{
		"event_id", "resource_project_id", "configuration_uuid", "revision", "operation", "actor_id",
		"sanitized_snapshot", "snapshot_digest", "state", "attempt_count", "available_at", "last_attempt_at",
		"lease_owner", "lease_expires_at", "delivered_at", "dead_at", "last_error_code", "created_at", "updated_at",
	}
	var columns []string
	if err := pool.QueryRow(ctx, `
SELECT array_agg(column_name::text ORDER BY ordinal_position)
FROM information_schema.columns
WHERE table_schema = 'elitea_runtime'
  AND table_name = 'configuration_lifecycle_outbox'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(columns) != fmt.Sprint(expected) {
		t.Fatalf("lifecycle columns=%v", columns)
	}
}
