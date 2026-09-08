package social_test

// THE GAP THIS CLOSES, AT THE SURFACE AN OPERATOR TOUCHES.
//
// `identity.initial_global_admins` names a login by its stored
// `auth_core__user_provider.provider_ref`. With Azure AD or Okta the OIDC
// subject inside that reference is an opaque identifier nobody knows before the
// person signs in, so making the first global administrator of a fresh
// deployment meant `SELECT provider_ref FROM auth_core__user_provider` against
// the production database. `GET /social/author` now carries the caller's own
// references, so the operator reads the exact value the chart wants from the
// endpoint the application already serves them.
//
// THE TEST IS AT THE HTTP SURFACE ON PURPOSE. A test of the query alone would
// stay green if the field were dropped from the response struct or the resolver
// were never called from GetAuthor — which is the half that goes missing.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL). The pool fixture,
// the template and `seedAuthorUser` are shared with
// personal_project_postgres_integration_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// authorProviderRefs performs the request the SPA performs and returns the new
// field.
func authorProviderRefs(t *testing.T, routes http.Handler, userID int64, email string) []string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/author/", nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{
		ID:     strconv.FormatInt(userID, 10),
		UserID: strconv.FormatInt(userID, 10),
		Email:  email,
	}))
	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /author/ status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		ProviderRefs []string `json:"provider_refs"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode author response %s: %v", recorder.Body.String(), err)
	}
	return decoded.ProviderRefs
}

func linkAuthProvider(t *testing.T, pool *pgxpool.Pool, userID int64, providerRef string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO public.auth_core__user_provider (user_id, provider_ref) VALUES ($1, $2)`,
		userID, providerRef,
	); err != nil {
		t.Fatalf("link %s to user %d: %v", providerRef, userID, err)
	}
}

func TestGetAuthorReportsTheCallersOwnProviderReferences(t *testing.T) {
	pool := newPersonalProjectSocialPool(t)
	routes := handler.NewHandler(pool).Routes()

	email := "provider-refs@autotest.local"
	userID := seedAuthorUser(t, pool, email, "Reference Reader")

	// A person can hold more than one federated identity: one OIDC subject and
	// one SAML NameID at the same deployment. Both are values the chart may
	// name, so both are reported.
	linkAuthProvider(t, pool, userID, "oidc:00000000-1111-2222-3333-444444444444")
	linkAuthProvider(t, pool, userID, "saml:reference-reader@corp.com")

	// Somebody else's reference. The read is keyed on the authenticated
	// principal's user id, and this row must never appear in the answer.
	otherEmail := "other-person@autotest.local"
	otherID := seedAuthorUser(t, pool, otherEmail, "Somebody Else")
	linkAuthProvider(t, pool, otherID, "oidc:99999999-9999-9999-9999-999999999999")

	refs := authorProviderRefs(t, routes, userID, email)
	want := []string{
		"oidc:00000000-1111-2222-3333-444444444444",
		"saml:reference-reader@corp.com",
	}
	if len(refs) != len(want) {
		t.Fatalf("provider_refs = %q, want %q", refs, want)
	}
	for index, value := range want {
		if refs[index] != value {
			t.Fatalf("provider_refs = %q, want %q", refs, want)
		}
	}

	if other := authorProviderRefs(t, routes, otherID, otherEmail); len(other) != 1 ||
		other[0] != "oidc:99999999-9999-9999-9999-999999999999" {
		t.Fatalf("the other account read %q, want only its own reference", other)
	}
}

// An account with no federated identity — every Form password login — reports
// no references at all rather than an empty-looking value the operator might
// paste into the chart.
func TestGetAuthorOmitsProviderReferencesWhenTheAccountHasNone(t *testing.T) {
	pool := newPersonalProjectSocialPool(t)
	routes := handler.NewHandler(pool).Routes()

	email := "no-federated-identity@autotest.local"
	userID := seedAuthorUser(t, pool, email, "Password Person")

	if refs := authorProviderRefs(t, routes, userID, email); len(refs) != 0 {
		t.Fatalf("provider_refs = %q, want none", refs)
	}
}
