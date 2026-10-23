package configurations_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// TestCurrentModelCatalogRouteAnswersWithNoVault is the ROUTE-level pin for the
// outage: GET /api/v2/configurations/models/{projectID} on a deployment that
// holds no secret vault at all.
//
// The storage-level tests pin the defaults reader. They cannot see this seam.
// The handler maps every error it does not recognise to 500, so anything new in
// the chain between the vault loader and the route — a wrapper, a different
// composition root, a reinstated hard failure — restores an empty model picker
// while those tests stay green. So this test composes the REAL service over the
// REAL defaults reader, and stubs only the two things a unit test cannot have:
// the candidate rows and the vault loader itself.
//
// It also fixes what the credentials-screen journey could not: that test
// asserts this route answers 200 and passed against the broken code, because it
// wires neither the real defaults reader nor this route's composition.
func TestCurrentModelCatalogRouteAnswersWithNoVault(t *testing.T) {
	defaults, err := storage.NewCurrentModelDefaultsReader(absentVaultLoader{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := configurationapp.NewCurrentModelCatalogService(
		currentModelCandidateStub{}, defaults,
	)
	if err != nil {
		t.Fatal(err)
	}
	route := newCurrentModelCatalogRoute(t, service, permissionResolverFunc(
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{
				UserID:      11,
				Permissions: []string{handler.CurrentModelCatalogPermission},
			}, nil
		},
	))

	for _, section := range []string{
		"llm", "embedding", "vectorstorage", "image_generation", "asr", "tts",
	} {
		response := httptest.NewRecorder()
		route.ServeHTTP(response, currentReadRequest(
			http.MethodGet,
			"/api/v2/configurations/models/7?section="+section+"&include_shared=true",
			"10.0.0.8:43120",
		))
		if response.Code != http.StatusOK {
			t.Fatalf("section %s answered %d; the model picker is empty. Body: %s",
				section, response.Code, response.Body.String())
		}
		// The catalogue is the models, and it must actually carry them: a 200
		// with an empty list would be the same empty picker by another route.
		if !containsAll(response.Body.String(), `"total":1`, `"name":"gpt-4o-current"`) {
			t.Fatalf("section %s body=%s", section, response.Body.String())
		}
	}
}

func containsAll(body string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(body, fragment) {
			return false
		}
	}
	return true
}

// absentVaultLoader is a deployment that holds no vault: no project has stored
// a secret and nobody has written an admin one.
type absentVaultLoader struct{}

func (absentVaultLoader) LoadProjectVault(context.Context, int64) (storage.SecretVault, error) {
	return nil, storage.ErrVaultAbsent
}

func (absentVaultLoader) LoadAdminVault(context.Context) (storage.SecretVault, error) {
	return nil, storage.ErrVaultAbsent
}

// currentModelCandidateStub is one readable model row, so an empty answer can
// only come from the defaults read failing.
type currentModelCandidateStub struct{}

func (currentModelCandidateStub) List(
	_ context.Context,
	projectID int32,
	_ configurationapp.CurrentModelSection,
	sharedOnly bool,
) ([]configurationapp.CurrentModelCatalogItem, error) {
	if sharedOnly {
		return nil, nil
	}
	name := "gpt-4o-current"
	return []configurationapp.CurrentModelCatalogItem{
		{Name: name, DisplayName: &name, ProjectID: projectID},
	}, nil
}
