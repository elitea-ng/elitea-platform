package authsvc

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

// TestPostgresPrincipalReloadKeepsTheBearerBinding proves, against the real
// schema and the real 0071 migration, that re-validating a token by row ID
// (the forwarded-projection path) yields the same project binding and project
// state as validating it by UUID (the bearer path).
func TestPostgresPrincipalReloadKeepsTheBearerBinding(t *testing.T) {
	pool := newProjectSystemPATTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, suspended)
VALUES (7, 'owner@example.test', false);

INSERT INTO centry.project (
    id, name, owner_id, secrets_json, plugins, keycloak_groups, create_success, suspended
) VALUES
    (50, 'active', 7, '{}'::json, ARRAY[]::text[], '{}'::json, true, false),
    (51, 'suspended', 7, '{}'::json, ARRAY[]::text[], '{}'::json, true, true);

INSERT INTO public.auth_core__token (uuid, expires, user_id, name)
VALUES
    ('00000000-0000-0000-0000-000000000050', NULL, 7, 'bound-active'),
    ('00000000-0000-0000-0000-000000000051', NULL, 7, 'bound-suspended'),
    ('00000000-0000-0000-0000-000000000052', NULL, 7, 'unbound');

INSERT INTO elitea_identity.token_project_binding (token_id, project_id)
SELECT id, 50 FROM public.auth_core__token WHERE uuid = '00000000-0000-0000-0000-000000000050'
UNION ALL
SELECT id, 51 FROM public.auth_core__token WHERE uuid = '00000000-0000-0000-0000-000000000051';`); err != nil {
		t.Fatal(err)
	}

	queries := sqlcgen.New(pool)
	validator := NewPrincipalValidator(pool)
	cases := []struct {
		uuid       string
		wantID     *int64
		wantActive *bool
	}{
		{uuid: "00000000-0000-0000-0000-000000000050", wantID: ptr(int64(50)), wantActive: ptr(true)},
		{uuid: "00000000-0000-0000-0000-000000000051", wantID: ptr(int64(51)), wantActive: ptr(false)},
		{uuid: "00000000-0000-0000-0000-000000000052"},
	}
	for _, testCase := range cases {
		t.Run(testCase.uuid, func(t *testing.T) {
			bearer, err := queries.GetActivePATPrincipalByUUID(ctx, testCase.uuid)
			if err != nil {
				t.Fatal(err)
			}
			bearerID := tokenProjectID(bearer.ProjectID)
			bearerActive := tokenProjectActive(bearer.ProjectID, bearer.BoundProjectActive)

			reloaded, err := validator.ValidatePrincipal(ctx, auth.User{
				ID:       strconv.Itoa(int(bearer.TokenID)),
				TokenID:  strconv.Itoa(int(bearer.TokenID)),
				UserID:   "7",
				AuthType: "token",
			})
			if err != nil {
				t.Fatal(err)
			}
			if !equalPointer(reloaded.TokenProjectID, bearerID) || !equalPointer(reloaded.TokenProjectActive, bearerActive) {
				t.Fatalf("reloaded binding (%v, %v) differs from bearer binding (%v, %v)",
					deref(reloaded.TokenProjectID), deref(reloaded.TokenProjectActive), deref(bearerID), deref(bearerActive))
			}
			if !equalPointer(reloaded.TokenProjectID, testCase.wantID) || !equalPointer(reloaded.TokenProjectActive, testCase.wantActive) {
				t.Fatalf("binding = (%v, %v), want (%v, %v)",
					deref(reloaded.TokenProjectID), deref(reloaded.TokenProjectActive), deref(testCase.wantID), deref(testCase.wantActive))
			}
		})
	}
}
