package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	vectorstoreapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/vectorstore"
)

func TestCurrentProjectPgvectorConfigurationsRepositoryPostgresIdentityGuard(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repository, err := NewCurrentProjectPgvectorConfigurationsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	configuration := validCurrentProjectPgvectorConfigurationForTest(1)
	configurationID, err := repository.UpsertProjectPgvectorConfiguration(ctx, configuration)
	if err != nil {
		t.Fatalf("create current PgVector row: %v", err)
	}
	reusedID, err := repository.UpsertProjectPgvectorConfiguration(ctx, configuration)
	if err != nil || reusedID != configurationID {
		t.Fatalf("idempotent upsert id=%d error=%v, want id=%d", reusedID, err, configurationID)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM p_1.configuration WHERE elitea_title = $1`, configuration.Title); err != nil {
		t.Fatalf("remove valid fixture: %v", err)
	}

	conflicts := []struct {
		name      string
		uuid      string
		projectID int32
		typeName  string
		section   string
		source    string
	}{
		{
			name: "project", uuid: "00000000-0000-4000-8000-000000000101", projectID: 2,
			typeName: vectorstoreapp.ProjectPgvectorType, section: vectorstoreapp.ProjectPgvectorSection,
			source: vectorstoreapp.ProjectPgvectorSource,
		},
		{
			name: "type", uuid: "00000000-0000-4000-8000-000000000102", projectID: 1,
			typeName: "github", section: vectorstoreapp.ProjectPgvectorSection,
			source: vectorstoreapp.ProjectPgvectorSource,
		},
		{
			name: "section", uuid: "00000000-0000-4000-8000-000000000103", projectID: 1,
			typeName: vectorstoreapp.ProjectPgvectorType, section: "credentials",
			source: vectorstoreapp.ProjectPgvectorSource,
		},
		{
			name: "source", uuid: "00000000-0000-4000-8000-000000000104", projectID: 1,
			typeName: vectorstoreapp.ProjectPgvectorType, section: vectorstoreapp.ProjectPgvectorSection,
			source: "user",
		},
	}
	for _, conflict := range conflicts {
		t.Run(conflict.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `
INSERT INTO p_1.configuration (
    uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, status_logs, source, author_id
, created_at) VALUES (
    $1, $2, 'existing owner', $3, $4, $5, '{"owner":"existing"}'::jsonb,
    '{}'::jsonb, false, true, NULL, $6, NULL
, now() AT TIME ZONE 'UTC')`, conflict.uuid, conflict.projectID, configuration.Title, conflict.typeName, conflict.section, conflict.source); err != nil {
				t.Fatalf("insert conflicting fixture: %v", err)
			}

			_, err := repository.UpsertProjectPgvectorConfiguration(ctx, configuration)
			if !errors.Is(err, vectorstoreapp.ErrProjectPgvectorConflict) ||
				err.Error() != vectorstoreapp.ErrProjectPgvectorConflict.Error() {
				t.Fatalf("identity conflict error = %v", err)
			}

			var owner string
			var updated bool
			if err := pool.QueryRow(ctx, `
SELECT data->>'owner', updated_at IS NOT NULL
FROM p_1.configuration
WHERE elitea_title = $1`, configuration.Title).Scan(&owner, &updated); err != nil {
				t.Fatalf("read conflicting fixture: %v", err)
			}
			if owner != "existing" || updated {
				t.Fatalf("conflicting row was mutated: owner=%q updated=%t", owner, updated)
			}
			if _, err := pool.Exec(ctx, `DELETE FROM p_1.configuration WHERE elitea_title = $1`, configuration.Title); err != nil {
				t.Fatalf("remove conflicting fixture: %v", err)
			}
		})
	}
}
