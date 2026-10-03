package secrets

// F1 / UI-PD-1: a project vault holding an INTEGER default-model project id is
// readable through every project route.
//
// The Configurations default-model write (repos.SetCurrentModelDefault) stores
// `default_<section>_model_project_id` as a JSON integer through
// centrysecrets.RewriteWrapped — deliberately, because the model-default
// reader and pylon both read an int. The project routes decoded the vault into
// `map[string]string`, refused the whole document on that one number, and
// answered 500 "project vault is unreadable" on List, Get and Create for every
// project with a default model selected. The live regression run met it as
// "cleanup sweep cannot list secrets … Received: 500".
//
// The vault is seeded through the PRODUCT writers, in the order a real project
// meets them: provisioning creates the vault, the Configurations page sets a
// default model, pgvector provisioning stores its material through this
// handler's own rewrite. The last step is the write-back case: it rewrites a
// vault that holds an integer, and the integer must still be one afterwards.
//
// Requires a PostgreSQL to create an isolated database in; skipped otherwise.

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

const (
	// Deliberately unused by every other file in this package.
	intModelProjectID     = "613"
	intModelProjectNumber = 613
	intModelTargetProject = 42
	intModelIDName        = "default_chat_model_project_id"
	intModelNameName      = "default_chat_model_name"
	pgvectorSecretName    = "pgvector_project_password"
)

func TestAVaultHoldingAnIntegerDefaultModelIDIsReadable(t *testing.T) {
	fixture := loadCentryVaultFixture(t)
	t.Setenv(MasterKeyEnvVar, fixture.MasterKeyEnvValue)
	pool := newSecretsPool(t)
	ctx := context.Background()
	handler := NewHandler(pool)
	if handler.masterKey == nil {
		t.Fatal("premise failed: the master key was not loaded, so RewriteWrapped is not exercised")
	}

	// 1. Provisioning creates the vault.
	if err := handler.EnsureProjectVault(ctx, intModelProjectID); err != nil {
		t.Fatalf("provision the project vault: %v", err)
	}
	// 2. The Configurations page selects a default model: an INTEGER id.
	vaultRepo, err := repos.NewCurrentSecretVaultRepository(pool, []byte(fixture.MasterKeyEnvValue))
	if err != nil {
		t.Fatalf("build the current secret-vault repository: %v", err)
	}
	if err := vaultRepo.SetCurrentModelDefault(ctx, configurationapp.CurrentModelDefaultSelection{
		ProjectID:       intModelProjectNumber,
		Name:            "gpt-4o",
		TargetProjectID: intModelTargetProject,
		Section:         "chat",
	}); err != nil {
		t.Fatalf("set the current model default: %v", err)
	}
	assertStoredIntegerID(t, handler, "after the model-default write")

	// 3. pgvector provisioning rewrites the same vault through this handler.
	// It used to fail here too: StoreProjectSecrets opens the vault first.
	if err := handler.StoreProjectSecrets(ctx, intModelProjectID, map[string]string{
		pgvectorSecretName: "pgvector-password",
	}); err != nil {
		t.Fatalf("store pgvector material in a vault holding an integer: %v", err)
	}
	assertStoredIntegerID(t, handler, "after the pgvector write-back")

	router := secretsRouter(t, pool, allSecretPermissions())

	// List: 200, every name present.
	list := do(t, router, http.MethodGet, projectSecretsBase+intModelProjectID, nil)
	if list.Code != http.StatusOK {
		t.Fatalf("List status = %d, want 200 (body %s)", list.Code, list.Body.String())
	}
	var items []SecretListItem
	if err := json.Unmarshal(list.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode List body %s: %v", list.Body.String(), err)
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name)
	}
	for _, want := range []string{intModelIDName, intModelNameName, pgvectorSecretName} {
		if !slices.Contains(names, want) {
			t.Fatalf("List names = %v, missing %q", names, want)
		}
	}

	secretBase := "/secrets/secret/default/" + intModelProjectID + "/"

	// Get of the integer: 200, exposed as its decimal text.
	get := do(t, router, http.MethodGet, secretBase+intModelIDName, nil)
	if get.Code != http.StatusOK {
		t.Fatalf("Get %s status = %d, want 200 (body %s)", intModelIDName, get.Code, get.Body.String())
	}
	var detail SecretDetail
	if err := json.Unmarshal(get.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode Get body: %v", err)
	}
	if detail.Value != "42" {
		t.Fatalf("Get %s value = %q, want \"42\"", intModelIDName, detail.Value)
	}

	// Get of an ordinary string secret in the same vault: 200.
	if got := do(t, router, http.MethodGet, secretBase+intModelNameName, nil); got.Code != http.StatusOK {
		t.Fatalf("Get %s status = %d, want 200 (body %s)", intModelNameName, got.Code, got.Body.String())
	}

	// Get of an unknown name: 404, not 500.
	if missing := do(t, router, http.MethodGet, secretBase+"no_such_secret", nil); missing.Code != http.StatusNotFound {
		t.Fatalf("Get unknown status = %d, want 404 (body %s)", missing.Code, missing.Body.String())
	}

	// Create: 201, and the integer survives the write-back.
	created := do(t, router, http.MethodPost, projectSecretsBase+intModelProjectID,
		map[string]string{"name": "created_after_default", "value": "v"})
	if created.Code != http.StatusCreated {
		t.Fatalf("Create status = %d, want 201 (body %s)", created.Code, created.Body.String())
	}
	assertStoredIntegerID(t, handler, "after a Create through the route")

	// Update of a string secret beside it: the integer survives too.
	updated := do(t, router, http.MethodPut, secretBase+intModelNameName,
		map[string]string{"value": "gpt-4.1"})
	if updated.Code != http.StatusOK {
		t.Fatalf("Update status = %d, want 200 (body %s)", updated.Code, updated.Body.String())
	}
	assertStoredIntegerID(t, handler, "after an Update of a neighbouring secret")
}

// assertStoredIntegerID decrypts the stored vault and checks the default-model
// id is a JSON NUMBER, and that the model-default reader the gateway and
// pylon parity rely on (centrysecrets.LookupRegularProjectID) still reads it.
func assertStoredIntegerID(t *testing.T, handler *Handler, stage string) {
	t.Helper()
	ctx := context.Background()
	plaintext, _, err := handler.openVaultContents(ctx, handler.pool, dbKey(intModelProjectID), false)
	if err != nil {
		t.Fatalf("%s: open the stored vault: %v", stage, err)
	}
	var stored struct {
		Secrets map[string]json.RawMessage `json:"secrets"`
	}
	if err := json.Unmarshal(plaintext, &stored); err != nil {
		t.Fatalf("%s: decode the stored vault: %v", stage, err)
	}
	if raw := string(stored.Secrets[intModelIDName]); raw != "42" {
		t.Fatalf("%s: stored %s = %s, want the JSON integer 42", stage, intModelIDName, raw)
	}

	var keyRow, dataRow []byte
	if err := handler.pool.QueryRow(ctx,
		`SELECT k.data, d.data FROM centry.secrets_key k JOIN centry.secrets_data d USING (id) WHERE id = $1`,
		dbKey(intModelProjectID)).Scan(&keyRow, &dataRow); err != nil {
		t.Fatalf("%s: read the vault rows: %v", stage, err)
	}
	fixture := loadCentryVaultFixture(t)
	vault, err := centrysecrets.OpenWrapped([]byte(fixture.MasterKeyEnvValue), keyRow, dataRow)
	if err != nil {
		t.Fatalf("%s: centrysecrets cannot open the vault: %v", stage, err)
	}
	secret, err := vault.LookupRegularProjectID(intModelIDName)
	if err != nil || secret.Value != "42" {
		t.Fatalf("%s: LookupRegularProjectID = %q, %v; want \"42\"", stage, secret.Value, err)
	}
}
