package social_test

// UI-UX-1(a), AT THE SURFACE THE SPA READS.
//
// The chat greeting and every "who am I" label read `GET /social/author`'s
// `name`. An account with no centry.social_users row yet (an account made by
// an administrator, or by SCIM, that has not opened its profile) answered its
// EMAIL there, although auth_core__user held the person's name. The live
// regression greeted "Hello, regression-chat@example.test!".
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL). The pool fixture,
// the template and `seedAuthorUser` are shared with
// personal_project_postgres_integration_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

func authorName(t *testing.T, routes http.Handler, userID int64, email string) string {
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
		Name string `json:"name"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode author response %s: %v", recorder.Body.String(), err)
	}
	return decoded.Name
}

func TestGetAuthorNamesAnAccountWithoutASocialProfileByItsName(t *testing.T) {
	pool := newPersonalProjectSocialPool(t)
	routes := handler.NewHandler(pool).Routes()

	email := "greeting-name@autotest.local"
	userID := seedAuthorUser(t, pool, email, "Greta Named")

	if got := authorName(t, routes, userID, email); got != "Greta Named" {
		t.Fatalf("name = %q, want the account name, not the email", got)
	}
}

func TestGetAuthorFallsBackToTheEmailOnlyForAnUnnamedAccount(t *testing.T) {
	pool := newPersonalProjectSocialPool(t)
	routes := handler.NewHandler(pool).Routes()

	email := "unnamed-account@autotest.local"
	userID := seedAuthorUser(t, pool, email, "")

	if got := authorName(t, routes, userID, email); got != email {
		t.Fatalf("name = %q, want the email for an account with no name", got)
	}
}
