package configurations_test

// #1286, the resolver half, against a real PostgreSQL and the real vaults.
//
// Main's OAuth proxy decides whether a stored client secret may follow a token
// endpoint the toolkit's editor wrote by comparing two resolutions of the same
// toolkit: the claim-mode one, and the project-owned twin whose unsecreter
// never falls back to the admin vault. The proxy's rule is unit-tested in
// internal/api/v2/eliteacore with stubbed resolutions; this file proves the
// COMPOSED twin really differs exactly where a global secret was redeemed, and
// agrees everywhere else.
//
// It lives beside the stored-check harness because that harness already builds
// the centry vault tables and two tenant schemas.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

func TestTheProjectOwnedTwinLeavesOnlyGlobalSecretsUnredeemed(t *testing.T) {
	pool := newStoredCheckPool(t)
	ctx := context.Background()
	const toolkitProject = storedCheckOther

	// One global (admin-vault) secret, one project secret.
	secrets := v2secrets.NewHandler(pool)
	adminRequest := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"secret":"admin-value"}`))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("name", "platform_oauth")
	adminRequest = adminRequest.WithContext(context.WithValue(adminRequest.Context(), chi.RouteCtxKey, routeContext))
	adminRecorder := httptest.NewRecorder()
	secrets.AdminCreate(adminRecorder, adminRequest)
	if adminRecorder.Code != http.StatusOK {
		t.Fatalf("seed the admin secret: %d %s", adminRecorder.Code, adminRecorder.Body.String())
	}
	if err := secrets.EnsureProjectVault(ctx, fmt.Sprint(toolkitProject)); err != nil {
		t.Fatalf("create the project vault: %v", err)
	}
	writer, err := dbrepos.NewCurrentSecretVaultRepository(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.MutateProject(ctx, toolkitProject, []centrysecrets.Mutation{{
		Collection: centrysecrets.RegularSecrets, Name: "project_oauth", Value: "project-value",
	}}); err != nil {
		t.Fatalf("seed the project secret: %v", err)
	}

	// The project's own github credential references both.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO p_%d.configuration (uuid, project_id, label, elitea_title, type, section, data, meta, shared,
			status_ok, source, created_at)
		VALUES (gen_random_uuid(), $1, 'gh', 'gh', 'github', 'credentials', $2::jsonb, '{}'::jsonb, false,
			true, 'user', (now() AT TIME ZONE 'UTC'))`, toolkitProject),
		toolkitProject, `{"base_url":"https://api.github.com",`+
			`"access_token":"{{secret.platform_oauth}}","app_private_key":"{{secret.project_oauth}}"}`); err != nil {
		t.Fatalf("seed the configuration: %v", err)
	}

	runtime, err := runtimecomposition.NewCurrentConfigurationsRuntime(pool, storedCheckProject, "", nil)
	if err != nil {
		t.Fatalf("compose the Configurations runtime: %v", err)
	}
	t.Cleanup(runtime.Destroy)
	full, err := runtime.NewToolkitSettingsValidator(pool)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := runtime.NewProjectOwnedToolkitSettingsResolver(pool)
	if err != nil {
		t.Fatal(err)
	}

	request := configurationapp.CurrentToolkitSettingsRequest{
		ToolkitType: "github",
		Settings: map[string]any{
			"repository":           "octocat/hello-world",
			"github_configuration": map[string]any{"elitea_title": "gh", "private": false},
		},
		ProjectID: toolkitProject,
		UserID:    1,
		Mode:      configurationapp.CurrentToolkitSettingsClaimMode,
	}
	fullSettings, err := full.Resolve(ctx, request)
	if err != nil {
		t.Fatalf("claim-mode resolution: %v", err)
	}
	ownedSettings, err := owned.Resolve(ctx, request)
	if err != nil {
		t.Fatalf("project-owned resolution: %v", err)
	}
	fullConfiguration := fullSettings["github_configuration"].(map[string]any)
	ownedConfiguration := ownedSettings["github_configuration"].(map[string]any)

	if fullConfiguration["access_token"] != "admin-value" {
		t.Fatalf("the claim-mode resolution did not redeem the global secret: %#v", fullConfiguration["access_token"])
	}
	if ownedConfiguration["access_token"] != "{{secret.platform_oauth}}" {
		t.Fatalf("the project-owned twin redeemed a global secret: %#v", ownedConfiguration["access_token"])
	}
	if fullConfiguration["app_private_key"] != "project-value" ||
		ownedConfiguration["app_private_key"] != "project-value" {
		t.Fatalf("a project secret must resolve identically in both: %#v / %#v",
			fullConfiguration["app_private_key"], ownedConfiguration["app_private_key"])
	}
	if fullConfiguration["configuration_project_id"] != ownedConfiguration["configuration_project_id"] {
		t.Fatalf("owner stamps differ: %#v / %#v",
			fullConfiguration["configuration_project_id"], ownedConfiguration["configuration_project_id"])
	}
}
