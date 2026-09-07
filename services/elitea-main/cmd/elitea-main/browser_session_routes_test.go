package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	projectinfoapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projectinfo"
	socialapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	socialapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/authcomposition"
)

const browserSessionTestSecret = "browser-session-secret"

// formShapeAuth builds the credential set a deployment with
// ELITEA_AUTH_CONFIG_FILE gives every route: the shared composition's Form
// branch.
//
// A non-nil *FormGraph is the whole condition that selects that branch. Its
// methods are never reached: these tests send a cookie and no Authorization
// header, so the token validator is not consulted.
func formShapeAuth(principals apimw.PrincipalValidator) apimw.AuthConfig {
	return apiGroupAuthConfig(
		&authcomposition.FormGraph{},
		principals,
		stubForwardedIdentityVerifier{},
		nil,
		nil,
		browserSessionTestSecret,
		false, nil)
}

// browserSessionRequest builds a GET carrying a valid `elitea_session` cookie
// and NOTHING else — the exact credential shape a browser presents.
func browserSessionRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(&http.Cookie{
		Name:  "elitea_session",
		Value: signedSessionCookie(t, browserSessionTestSecret, "42", time.Now().Add(time.Hour)),
	})
	return request
}

type projectInfoReaderStub struct{ calls int }

func (r *projectInfoReaderStub) GetCurrentProjectInfo(
	context.Context,
	int32,
) (projectinfoapi.CurrentProjectInfo, error) {
	r.calls++
	return projectinfoapi.CurrentProjectInfo{TeammatesCount: 3}, nil
}

// TestCurrentProjectInfoFormAuthAcceptsABrowserSession drives the REAL route —
// the shared composition, apimw.Auth, the per-project permission gate and the
// handler — with a validly signed session cookie.
//
// `GET /api/v2/elitea_core/project_info/prompt_lib/{projectID}/project-info`
// is called by the SPA on entering a project and by nothing else. Only a
// browser calls it, and a browser's credential is the cookie. main.go composed
// it from a private AuthConfig that carried the forwarded-identity verifier and
// NO SessionSecret, so apimw.Auth's cookie branch was inert and the request
// fell through to `401 missing authorization header`.
//
// The SPA reads a 401 as an expired session (`needsReauth` in
// apps/elitea-web/src/shared/api/http.ts is 401-only, deliberately), so the
// page opened a fresh OIDC authorize window on every visit. The backend was
// healthy throughout.
//
// The deactivated row is the control. A 200 for BOTH principals would mean the
// cookie was accepted without re-checking the principal, which is the #301
// defect in a second shape: RBAC rows survive deactivation, so the permission
// gate below does not catch it.
func TestCurrentProjectInfoFormAuthAcceptsABrowserSession(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		principals *countingPrincipals
		wantStatus int
		wantReads  int
	}{
		{
			name:       "active principal is served",
			principals: &countingPrincipals{inner: activePrincipals{}},
			wantStatus: http.StatusOK,
			wantReads:  1,
		},
		{
			name:       "deactivated principal is refused",
			principals: &countingPrincipals{inner: deactivatedPrincipals{}},
			wantStatus: http.StatusUnauthorized,
			wantReads:  0,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reader := &projectInfoReaderStub{}
			route, err := projectinfoapi.NewCurrentProjectInfoRoute(
				reader,
				formShapeAuth(testCase.principals),
				&grantingPermissions{permission: projectinfoapi.CurrentProjectInfoPermission},
			)
			if err != nil {
				t.Fatalf("compose current project-info route: %v", err)
			}

			recorder := httptest.NewRecorder()
			route.ServeHTTP(recorder, browserSessionRequest(
				t, "/api/v2/elitea_core/project_info/prompt_lib/1/project-info",
			))

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)",
					recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if reader.calls != testCase.wantReads {
				t.Fatalf("repository read %d times, want %d",
					reader.calls, testCase.wantReads)
			}
			if testCase.principals.consulted() != 1 {
				t.Fatalf("PrincipalValidator consulted %d times, want 1",
					testCase.principals.consulted())
			}
		})
	}
}

type socialAuthorsReaderStub struct{ calls int }

func (r *socialAuthorsReaderStub) ListCurrentProjectAuthors(
	context.Context,
	int32,
) ([]socialapp.CurrentAuthor, error) {
	r.calls++
	return nil, nil
}

// TestCurrentSocialAuthorsFormAuthAcceptsABrowserSession is the same assertion
// for `GET /api/v2/social/authors/{projectID}` and its `default` twin.
//
// The artifacts and prompts lists read author names from it on every render.
// Its private AuthConfig also carried no SessionSecret, so the browser had no
// credential the route accepts.
func TestCurrentSocialAuthorsFormAuthAcceptsABrowserSession(t *testing.T) {
	for _, path := range []string{
		"/api/v2/social/authors/1",
		"/api/v2/social/authors/default/1",
	} {
		t.Run(path, func(t *testing.T) {
			principals := &countingPrincipals{inner: activePrincipals{}}
			reader := &socialAuthorsReaderStub{}
			route, err := socialapi.NewCurrentAuthorsRoute(
				reader,
				formShapeAuth(principals),
				&grantingPermissions{permission: socialapi.CurrentAuthorsPermission},
			)
			if err != nil {
				t.Fatalf("compose current Social authors route: %v", err)
			}

			recorder := httptest.NewRecorder()
			route.ServeHTTP(recorder, browserSessionRequest(t, path))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q): a browser session is "+
					"the only credential this route ever receives",
					recorder.Code, recorder.Body.String())
			}
			if reader.calls != 1 {
				t.Fatalf("repository read %d times, want 1", reader.calls)
			}
			if principals.consulted() != 1 {
				t.Fatalf("PrincipalValidator consulted %d times, want 1",
					principals.consulted())
			}
		})
	}
}

// TestCurrentSocialAuthorsFormAuthStillRefusesADeactivatedSession is the
// control for the test above. A cookie is not a licence: the principal must be
// re-read on every request, because a deactivated user keeps the RBAC rows the
// permission gate asks about.
func TestCurrentSocialAuthorsFormAuthStillRefusesADeactivatedSession(t *testing.T) {
	principals := &countingPrincipals{inner: deactivatedPrincipals{}}
	reader := &socialAuthorsReaderStub{}
	route, err := socialapi.NewCurrentAuthorsRoute(
		reader,
		formShapeAuth(principals),
		&grantingPermissions{permission: socialapi.CurrentAuthorsPermission},
	)
	if err != nil {
		t.Fatalf("compose current Social authors route: %v", err)
	}

	recorder := httptest.NewRecorder()
	route.ServeHTTP(recorder, browserSessionRequest(t, "/api/v2/social/authors/1"))

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %q)", recorder.Code, recorder.Body.String())
	}
	if reader.calls != 0 {
		t.Fatalf("repository read %d times on a refused request, want 0", reader.calls)
	}
}

type avatarStoreStub struct {
	calls  int
	avatar string
}

func (s *avatarStoreStub) GetCurrentAvatar(context.Context, int64) (*string, error) {
	s.calls++
	return &s.avatar, nil
}

func (s *avatarStoreStub) SetCurrentAvatar(context.Context, int64, string) error {
	s.calls++
	return nil
}

// TestCurrentSocialAvatarFormAuthAcceptsABrowserSession is the same assertion
// for `GET /api/v2/social/avatar/{projectID}`.
//
// The user menu reads it on every page of the product, so this 401 was visible
// everywhere at once.
func TestCurrentSocialAvatarFormAuthAcceptsABrowserSession(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		principals *countingPrincipals
		wantStatus int
		wantReads  int
	}{
		{
			name:       "active principal is served",
			principals: &countingPrincipals{inner: activePrincipals{}},
			wantStatus: http.StatusOK,
			wantReads:  1,
		},
		{
			name:       "deactivated principal is refused",
			principals: &countingPrincipals{inner: deactivatedPrincipals{}},
			wantStatus: http.StatusUnauthorized,
			wantReads:  0,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &avatarStoreStub{avatar: "/avatars/1/a.png"}
			route, err := socialapi.NewCurrentAvatarRoute(
				store,
				nil,
				formShapeAuth(testCase.principals),
				&grantingPermissions{permission: socialapi.CurrentAvatarGetPermission},
			)
			if err != nil {
				t.Fatalf("compose current Social avatar route: %v", err)
			}

			recorder := httptest.NewRecorder()
			route.ServeHTTP(recorder, browserSessionRequest(t, "/api/v2/social/avatar/1"))

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)",
					recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if store.calls != testCase.wantReads {
				t.Fatalf("store read %d times, want %d", store.calls, testCase.wantReads)
			}
			if testCase.principals.consulted() != 1 {
				t.Fatalf("PrincipalValidator consulted %d times, want 1",
					testCase.principals.consulted())
			}
		})
	}
}
