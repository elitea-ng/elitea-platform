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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// A failed profile read is a 500, never a 200 that presents the defaults as the
// user's stored personalization and memory settings.
func TestGetAuthorReportsAFailedProfileReadAsAServerError(t *testing.T) {
	pool := newPersonalProjectSocialPool(t)
	routes := handler.NewHandler(pool).Routes()

	email := "broken-profile-read@autotest.local"
	userID := seedAuthorUser(t, pool, email, "Bea Broken")
	if _, err := pool.Exec(t.Context(), `ALTER TABLE centry.social_users RENAME TO social_users_unavailable`); err != nil {
		t.Fatalf("break the profile table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE centry.social_users_unavailable RENAME TO social_users`)
	})

	request := httptest.NewRequest(http.MethodGet, "/author/", nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{
		ID: strconv.FormatInt(userID, 10), UserID: strconv.FormatInt(userID, 10), Email: email,
	}))
	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "social_users") {
		t.Fatalf("the error body leaks the database cause: %s", recorder.Body.String())
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
