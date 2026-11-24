package configurations_test

// A holder of only llm.catalogue.view reads the platform catalogue and never
// a credential value (ADR-0059 R0.3, B-14).
//
// The view permission is new: before R0.3 every reader of these routes held
// configuration.governance, which also writes them. This test makes the
// redaction a regression guard for the read-only holder. It plants a marker
// secret as a LITERAL in a platform provider row (the shape a row imported
// from a legacy deployment can have, which sealing does not protect), links a
// platform model to it, and greps every GET response the holder can reach.
// It also proves the holder is refused the calls that use the stored
// credential.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const redactionMarker = "sk-R03-MARKER-SECRET-7f3c"

// catalogueViewResolver grants exactly llm.catalogue.view in every mode.
type catalogueViewResolver struct{}

func (catalogueViewResolver) ResolvePermissions(
	context.Context, auth.User, string, string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 1, Permissions: []string{"llm.catalogue.view"}}, nil
}

func TestACatalogueViewHolderNeverReadsACredentialValue(t *testing.T) {
	pool := newCreateEchoPool(t)
	ctx := context.Background()

	var providerID, modelID int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO p_%d.configuration
			(uuid, project_id, elitea_title, label, type, section, data, meta, shared,
			 status_ok, source, created_at)
		VALUES (gen_random_uuid(), $1, 'r03_marker_provider', 'r03_marker_provider', 'open_ai',
			'ai_credentials', $2::jsonb, '{}'::jsonb, true, true, 'user', now() AT TIME ZONE 'UTC')
		RETURNING id`, globalScopeProject),
		globalScopeProject,
		`{"api_base":"https://api.example.com/v1","api_key":"`+redactionMarker+`"}`,
	).Scan(&providerID); err != nil {
		t.Fatalf("seed the platform provider: %v", err)
	}
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO p_%d.configuration
			(uuid, project_id, elitea_title, label, type, section, data, meta, shared,
			 status_ok, source, created_at)
		VALUES (gen_random_uuid(), $1, 'r03_marker_model', 'r03_marker_model', 'llm_model', 'llm',
			$2::jsonb, '{}'::jsonb, true, true, 'user', now() AT TIME ZONE 'UTC')
		RETURNING id`, globalScopeProject),
		globalScopeProject,
		`{"name":"r03-model","ai_credentials":{"elitea_title":"r03_marker_provider"}}`,
	).Scan(&modelID); err != nil {
		t.Fatalf("seed the platform model: %v", err)
	}

	handler := configurations.NewHandler(pool, configurations.WithPublicProjectID(globalScopeProject))
	gate := func(names ...string) func(http.Handler) http.Handler {
		return apimw.RequireCentralPermissions(catalogueViewResolver{}, auth.PermissionModeAdministration, names...)
	}
	gates := configurations.CatalogueGates{
		View:   gate("llm.catalogue.view", "configuration.governance"),
		Manage: gate("llm.catalogue.manage", "configuration.governance"),
	}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "1"})))
		})
	})
	router.Route("/gateway", func(r chi.Router) {
		r.Mount("/providers", handler.GlobalProviderRoutes(gates))
		r.Mount("/platform_models", handler.GlobalModelRoutes(gates))
		r.Mount("/default_model", handler.PlatformDefaultModelRoutes(gates))
	})
	serve := func(method, target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader("{}")))
		return rec
	}

	// Every GET of the catalogue surface. The two listings must answer 200
	// and name the seeded rows, so "no marker" is not "no answer".
	reads := []struct {
		target string
		mustOK bool
		names  string
	}{
		{"/gateway/providers", true, "r03_marker_provider"},
		{"/gateway/platform_models", true, "r03_marker_model"},
		{fmt.Sprintf("/gateway/platform_models/%d/default_usage", modelID), false, ""},
		{"/gateway/default_model?section=llm", false, ""},
	}
	for _, read := range reads {
		rec := serve(http.MethodGet, read.target)
		if rec.Code == http.StatusForbidden {
			t.Errorf("GET %s = 403: a catalogue view holder is refused a read", read.target)
		}
		if read.mustOK && (rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), read.names)) {
			t.Errorf("GET %s = %d %s, want 200 naming %s", read.target, rec.Code, rec.Body, read.names)
		}
		if strings.Contains(rec.Body.String(), redactionMarker) {
			t.Errorf("GET %s returned the credential value: %s", read.target, rec.Body)
		}
	}

	// The calls that use the stored credential are writes: refused.
	for _, call := range []string{"check", "revalidate", "models"} {
		target := fmt.Sprintf("/gateway/providers/%d/%s", providerID, call)
		if rec := serve(http.MethodPost, target); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s = %d, want 403 for a catalogue view holder", target, rec.Code)
		}
	}
}
