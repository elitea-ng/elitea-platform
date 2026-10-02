package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	configapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	secretsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	configapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func TestInternalTypedConfigurationPostgresDefaultAndSharedScope(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `INSERT INTO centry.project (id, name, owner_id) VALUES (2, 'Internal MCP fixture', 73)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT create_tenant_schema('p_2')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.New(pool, platformmigrations.Files).ApplyTenant(ctx, 2); err != nil {
		t.Fatal(err)
	}
	// Seed only the isolated database. Production discovery uses the shared sqlc repositories.
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.configuration (project_id, elitea_title, label, type, section, data, shared, status_ok)
		VALUES (1, 'public_shared', 'Shared model', 'llm_model', 'llm', '{"name":"shared-model"}', true, true),
		       (1, 'public_private', 'Private model', 'llm_model', 'llm', '{"name":"private-model"}', false, true);
		INSERT INTO p_2.configuration (project_id, elitea_title, label, type, section, data, status_ok)
		VALUES (2, 'own_model', 'Own model', 'llm_model', 'llm', '{"name":"own-model"}', true),
		       (2, 'own_credential', 'Own credential', 'openapi', 'credentials', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	rows, err := repos.NewCurrentConfigurationsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	types, err := configapp.NewCurrentConfigurationTypesService(rows)
	if err != nil {
		t.Fatal(err)
	}
	modelRows, err := repos.NewCurrentModelsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	vaults, err := storage.NewPostgresSecretVaultLoader(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vaults.Destroy)
	defaults, err := storage.NewCurrentModelDefaultsReader(vaults)
	if err != nil {
		t.Fatal(err)
	}
	models, err := configapp.NewCurrentModelCatalogService(modelRows, defaults)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := repos.NewCurrentSecretVaultRepository(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Destroy)
	executor := newHandlerInternalConfigurationExecutor(configapi.NewHandler(pool),
		configapi.NewCurrentConfigurationToolHandler(types, models, writer, 1))
	call := func(operation internalConfigurationOperation, args map[string]any) []byte {
		t.Helper()
		result, err := executor.Execute(ctx, 2, 73, operation, args)
		if err != nil || result.status != http.StatusOK {
			t.Fatalf("operation %s: status=%d error=%v body=%s", operation, result.status, err, result.body)
		}
		return result.body
	}
	if body := call(internalListStoredConfigurationTypes, nil); string(body) != `{"rows":["openapi"],"total":1}` {
		t.Fatalf("credential types: %s", body)
	}
	if body := call(internalListStoredConfigurationTypes, map[string]any{"section": "llm"}); string(body) != `{"rows":["llm_model"],"total":1}` {
		t.Fatalf("model types: %s", body)
	}
	for _, shared := range []bool{false, true} {
		body := call(internalListConfigurationModels, map[string]any{"include_shared": shared})
		var catalogue configapp.CurrentModelCatalogResponse
		if err := json.Unmarshal(body, &catalogue); err != nil {
			t.Fatal(err)
		}
		want := 1
		if shared {
			want = 2
		}
		if catalogue.Total != want || len(catalogue.Items) != want || strings.Contains(string(body), "private-model") {
			t.Fatalf("shared=%v catalogue: %s", shared, body)
		}
	}
	// Project provisioning owns vault creation. Model-default writes cannot replace an absent or damaged vault.
	selection := map[string]any{"name": "shared-model", "target_project_id": 1}
	absent, err := executor.Execute(ctx, 2, 73, internalSetDefaultConfigurationModel, selection)
	if err != nil || absent.status != http.StatusBadRequest {
		t.Fatalf("absent vault: status=%d error=%v", absent.status, err)
	}
	if err := secretsapi.NewHandler(pool).EnsureProjectVault(ctx, "2"); err != nil {
		t.Fatal(err)
	}
	call(internalSetDefaultConfigurationModel, selection)
	body := call(internalListConfigurationModels, map[string]any{"include_shared": true})
	var selected configapp.CurrentModelCatalogResponse
	if err := json.Unmarshal(body, &selected); err != nil {
		t.Fatal(err)
	}
	if selected.DefaultModelName == nil || *selected.DefaultModelName != "shared-model" ||
		selected.DefaultModelProjectID == nil || *selected.DefaultModelProjectID != 1 {
		t.Fatalf("persisted default: %s", body)
	}
	var endpointVaults, publicVaults int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE id = 'project-2'), count(*) FILTER (WHERE id = 'project-1') FROM centry.secrets_data`).Scan(&endpointVaults, &publicVaults); err != nil {
		t.Fatal(err)
	}
	if endpointVaults != 1 || publicVaults != 0 {
		t.Fatalf("vault scope: endpoint=%d public=%d", endpointVaults, publicVaults)
	}
}
