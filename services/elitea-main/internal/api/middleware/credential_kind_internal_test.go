package middleware

// The credential kind in this package (ADR-0059 decision 7; R1.2a): what the
// maintenance bypass and the administration route gates do with it.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

// credentialTableResolver is the production rule (legacyrbac.AdministrationSet)
// over an owner who holds `admin`, so the answer is decided by the credential.
type credentialTableResolver struct{}

func (credentialTableResolver) ResolvePermissions(
	_ context.Context, principal auth.User, _ string, _ string,
) (auth.PermissionResolution, error) {
	owner := []string{adminaccess.AdminAuthUsers, adminaccess.RuntimeAdminPublishedAgents, adminaccess.RuntimePlugins}
	return auth.PermissionResolution{
		UserID:      1,
		Permissions: legacyrbac.AdministrationSet(principal.CredentialKind, legacyrbac.TokenAdminAccess{}, owner),
	}, nil
}

// A native device session of an administrator keeps the maintenance bypass
// (ADR-0059 decision 7, "Native sessions"); a run token and a `none` token of
// the same administrator do not.
func TestANativeSessionOfAnAdministratorPassesTheMaintenanceBypass(t *testing.T) {
	gate := onWindow(credentialTableResolver{})
	for _, tc := range []struct {
		user   auth.User
		admits bool
	}{
		{auth.User{ID: "1", UserID: "1", TokenID: "5", NativeClientID: "desktop", CredentialKind: auth.CredentialKindNative}, true},
		{auth.User{ID: "1", UserID: "1", CredentialKind: auth.CredentialKindSession}, true},
		{auth.User{ID: "1", UserID: "1", TokenID: "6", CredentialKind: auth.CredentialKindRun}, false},
		{auth.User{ID: "1", UserID: "1", TokenID: "7", CredentialKind: auth.CredentialKindToken}, false},
		{auth.User{ID: "1", UserID: "1"}, false},
	} {
		status, reached := serve(gate, "/api/v2/elitea_core/applications/prompt_lib/1", &tc.user)
		if reached != tc.admits {
			t.Errorf("kind %q: status %d, reached %v; want reached %v", tc.user.CredentialKind, status, reached, tc.admits)
		}
	}
}

// No administration-mode ROUTE gate admits a native session, even on a name
// it holds; the developer and default modes are not affected.
func TestNoAdministrationRouteGateAdmitsANativeSession(t *testing.T) {
	native := auth.User{ID: "1", UserID: "1", TokenID: "5", NativeClientID: "desktop", CredentialKind: auth.CredentialKindNative}
	session := auth.User{ID: "1", UserID: "1", CredentialKind: auth.CredentialKindSession}
	held := stubResolver{permissions: []string{adminaccess.RuntimePlugins}}
	for _, tc := range []struct {
		user auth.User
		mode string
		want int
	}{
		{native, auth.PermissionModeAdministration, http.StatusForbidden},
		{session, auth.PermissionModeAdministration, http.StatusOK},
		{native, auth.PermissionModeDeveloper, http.StatusOK},
	} {
		handler := RequireCentralPermissions(held, tc.mode, adminaccess.RuntimePlugins)(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		request := httptest.NewRequest(http.MethodGet, "/gated", nil)
		request = request.WithContext(auth.ContextWithUser(request.Context(), tc.user))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.want {
			t.Errorf("kind %q in %s mode: %d, want %d", tc.user.CredentialKind, tc.mode, response.Code, tc.want)
		}
	}
}

// The kind each credential path puts on the principal: set after the
// principal validator ran, so a validator that rebuilds the principal cannot
// change it.
func TestEveryCredentialPathSetsItsKind(t *testing.T) {
	rebuild := rebuildingPrincipals{}
	for _, tc := range []struct {
		name      string
		validated auth.User
		header    [2]string
		want      auth.CredentialKind
	}{
		{"personal access token", auth.User{ID: "1", UserID: "1", TokenID: "5", AuthType: "token",
			CredentialKind: auth.CredentialKindToken}, [2]string{"Authorization", "Bearer pat"}, auth.CredentialKindToken},
		{"api key", auth.User{ID: "1", UserID: "1", TokenID: "5", AuthType: "token",
			CredentialKind: auth.CredentialKindToken}, [2]string{"X-API-Key", "pat"}, auth.CredentialKindToken},
		{"project system token", auth.User{ID: "3", UserID: "3", TokenID: "9", AuthType: "token",
			CredentialKind: auth.CredentialKindSystem}, [2]string{"Authorization", "Bearer pat"}, auth.CredentialKindSystem},
		{"native", auth.User{ID: "1", UserID: "1", TokenID: "5", AuthType: "token", NativeClientID: "desktop"},
			[2]string{"Authorization", "Bearer elnat_x"}, auth.CredentialKindNative},
		// A validator that claims a browser kind for a bearer is not believed.
		{"bearer claiming a session", auth.User{ID: "1", UserID: "1", TokenID: "5", AuthType: "token",
			CredentialKind: auth.CredentialKindSession}, [2]string{"Authorization", "Bearer pat"}, auth.CredentialKindNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got auth.User
			handler := Auth(AuthConfig{
				Validator:          fixedTokens{user: tc.validated},
				PrincipalValidator: rebuild,
			})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ = auth.UserFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			}))
			request := httptest.NewRequest(http.MethodGet, "/x", nil)
			request.Header.Set(tc.header[0], tc.header[1])
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d (%s)", response.Code, response.Body.String())
			}
			if got.CredentialKind != tc.want {
				t.Fatalf("kind %q, want %q", got.CredentialKind, tc.want)
			}
		})
	}
}

// The two browser planes this package reads besides the server-side session
// (which internal/api's browser_session_route_test.go drives through the
// router): the Form session carries its row's reference and the kind `form`;
// the legacy signed cookie is a `session` with no row, so no reference and no
// elevation.
func TestBrowserPlanesSetTheirKindAndReference(t *testing.T) {
	const secret = "session-secret"
	for _, tc := range []struct {
		name      string
		cfg       AuthConfig
		cookie    *http.Cookie
		want      auth.CredentialKind
		wantRef   int64
		elevation int64
	}{
		{"form session", AuthConfig{
			FormSessions:       formSessionWithRef{ref: 33},
			PrincipalValidator: rebuildingPrincipals{},
		}, &http.Cookie{Name: "elitea_browser_auth", Value: "x"}, auth.CredentialKindForm, 33, 33},
		{"legacy signed cookie", AuthConfig{
			SessionSecret:      secret,
			PrincipalValidator: rebuildingPrincipals{},
		}, &http.Cookie{Name: "elitea_session", Value: legacySessionCookie(secret, "7", time.Now().Add(time.Hour))},
			auth.CredentialKindSession, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got auth.User
			handler := Auth(tc.cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ = auth.UserFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			}))
			request := httptest.NewRequest(http.MethodGet, "/x", nil)
			request.AddCookie(tc.cookie)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d (%s)", response.Code, response.Body.String())
			}
			if got.CredentialKind != tc.want || got.BrowserSessionRef != tc.wantRef {
				t.Fatalf("kind %q ref %d, want %q ref %d", got.CredentialKind, got.BrowserSessionRef, tc.want, tc.wantRef)
			}
			if ref := got.Credential().ElevationSessionRef(); ref != tc.elevation {
				t.Fatalf("elevation ref %d, want %d", ref, tc.elevation)
			}
		})
	}
}

// formSessionWithRef is a Form plane whose every cookie names session ref.
type formSessionWithRef struct{ ref int64 }

func (f formSessionWithRef) AuthenticateFormSession(*http.Request) (auth.User, error) {
	return auth.User{ID: "7", UserID: "7", AuthType: "session", BrowserSessionRef: f.ref,
		CredentialKind: auth.CredentialKindForm}, nil
}

type fixedTokens struct{ user auth.User }

func (f fixedTokens) ValidateToken(context.Context, string) (auth.User, error) { return f.user, nil }

// rebuildingPrincipals drops every field it does not read from a row, the
// way a validator that rebuilds the principal would.
type rebuildingPrincipals struct{}

func (rebuildingPrincipals) ValidatePrincipal(_ context.Context, user auth.User) (auth.User, error) {
	user.CredentialKind = auth.CredentialKindNone
	user.BrowserSessionRef = 0
	return user, nil
}
