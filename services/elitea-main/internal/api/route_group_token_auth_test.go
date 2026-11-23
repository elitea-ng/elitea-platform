package api

// Does every route GROUP accept a personal access token?
//
// Regression findings F2 and C2 (2026-10 live regression). F2: a PAT admitted
// a chat turn and was then refused `401 token_rejected` on the events stream
// of that turn, because NewProductionRuntimeRoutes dropped the group's token
// validator (#289). C2: the same 401 came back on the administration project
// delete and on the administration role drop, right after the same PAT passed
// `/social/author`. Both are the shape this file pins: an apimw.AuthConfig
// with no Validator, which admits no bearer token at all.
//
// Two tests, because the class has two halves:
//
//   - TestEveryRouteGroupAcceptsABearerToken drives one route per group through
//     the REAL router with a bearer and with an API key, and requires that
//     neither is answered 401. A wrong bearer is the control on every row: it
//     must still be 401, or the row proves only that the path has no gate.
//   - TestEveryAuthConfigLiteralCarriesATokenValidator reads the SOURCE. A route
//     group nobody has written yet cannot be in the table above; a literal that
//     omits Validator is refused here instead.
//
// AUTHENTICATED IS NOT AUTHORIZED (ADR-0059 R1.2a). The administration rows
// above prove the bearer AUTHENTICATES there (no 401). What the token may then
// do is its credential's administration set: `none` (every new token) holds
// nothing, `inherit` the owner's set, and a native device session only the two
// in-application checks. TestANativeSessionOfAnAdminKeepsOnlyTheInApplicationChecks
// pins that half.

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

type routeGroupProjectResolver struct{}

func (routeGroupProjectResolver) PersonalProjectID(context.Context, string) (int, error) {
	return 1, nil
}

func TestEveryRouteGroupAcceptsABearerToken(t *testing.T) {
	reached := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	group := apimw.AuthConfig{
		Validator:          testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	}
	runtimeRoutes, err := NewProductionRuntimeRoutes(
		reached, reached,
		group.PrincipalValidator,
		group,
	)
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(RouterConfig{
		AuthValidator:          group.Validator,
		PrincipalValidator:     group.PrincipalValidator,
		ProductionRuntime:      runtimeRoutes,
		GatewayProxy:           reached,
		GatewayProjectResolver: routeGroupProjectResolver{},
	})

	for _, route := range []struct {
		group, method, path string
	}{
		// C2: the two calls the regression cleanup could not make with a PAT.
		{"/api/v2 group: administration project delete", http.MethodDelete, "/api/v2/projects/project/administration/8"},
		{"/api/v2 group: administration role drop", http.MethodPost, "/api/v2/admin/auth_users/administration"},
		// The call the same PAT passed, as the control C2 was measured against.
		{"/api/v2 group: social author", http.MethodGet, "/api/v2/social/author/"},
		{"artifact group", http.MethodGet, "/api/v2/artifacts/buckets/1"},
		{"/llm group", http.MethodGet, "/llm/v1/models"},
		// F2: the runtime routes compose their own AuthConfig from the group's.
		{"runtime routes: execution events", http.MethodGet, "/api/v2/executions/6/c878ec191be8ebb35feb4315a65f695f/events"},
		{"runtime routes: configuration validation", http.MethodPost, "/api/v2/configurations/validation/6/revision-1"},
	} {
		t.Run(route.group, func(t *testing.T) {
			for _, credential := range []struct {
				name, header, value string
				accepted            bool
			}{
				{"bearer", "Authorization", "Bearer " + testAuthToken, true},
				{"api key", "X-API-Key", testAuthToken, true},
				{"wrong bearer", "Authorization", "Bearer not-the-token", false},
			} {
				request := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(credential.header, credential.value)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)

				refused := response.Code == http.StatusUnauthorized
				if credential.accepted && refused {
					t.Errorf("%s: %s %s answered 401 (%s): this route group has no token validator",
						credential.name, route.method, route.path, strings.TrimSpace(response.Body.String()))
				}
				if !credential.accepted && (!refused || !strings.Contains(response.Body.String(), "token_rejected")) {
					t.Errorf("%s: %s %s answered %d (%s), want 401 token_rejected: the row proves nothing "+
						"unless the path is authenticated", credential.name, route.method, route.path,
						response.Code, strings.TrimSpace(response.Body.String()))
				}
			}
		})
	}
}

// credentialKindResolver applies the credentials table of ADR-0059 decision 7
// (legacyrbac.AdministrationSet, the rule the production resolver applies) to
// an owner who holds owner, with each token's admin_access from access. It
// records every principal it is asked about.
type credentialKindResolver struct {
	owner  []string
	access map[string]legacyrbac.TokenAdminAccess
	seen   *[]auth.User
}

func (r credentialKindResolver) ResolvePermissions(
	_ context.Context, principal auth.User, mode, _ string,
) (auth.PermissionResolution, error) {
	if r.seen != nil {
		*r.seen = append(*r.seen, principal)
	}
	if mode != auth.PermissionModeAdministration {
		return auth.PermissionResolution{UserID: 1, Permissions: []string{}}, nil
	}
	return auth.PermissionResolution{
		UserID:              1,
		Permissions:         legacyrbac.AdministrationSet(principal.CredentialKind, r.access[principal.TokenID], r.owner),
		ElevationSessionRef: principal.Credential().ElevationSessionRef(),
	}, nil
}

func (r credentialKindResolver) ResolveMembershipPermissions(
	ctx context.Context, principal auth.User, mode string,
) (auth.PermissionResolution, error) {
	return r.ResolvePermissions(ctx, principal, mode, "")
}

// The administration set of an `admin`: the legacy names every admin route
// still accepts, including the two a native session keeps.
var credentialKindAdminSet = []string{
	adminaccess.AdminAuthUsers,
	adminaccess.IdentitySSOView,
	adminaccess.RuntimeAdminPublishedAgents,
	adminaccess.RuntimePlugins,
}

// A native device session of an `admin` (ADR-0059 decision 7, "Native
// sessions"; R1.2a) keeps exactly the names of the two in-application checks,
// the maintenance bypass (apimw.MaintenanceAdminPermission) and Marketplace
// moderation (eliteacore.PublishedAgentsListPermission), and no
// administration route admits it: GET /admin/identity_providers/administration
// accepts runtime.plugins, the native session holds it, and the answer is
// still 403. An `inherit` token of the same owner, through the same router, is
// admitted. internal/api/middleware (maintenance) and
// internal/api/v2/eliteacore (moderation) prove the two checks pass for it.
func TestANativeSessionOfAnAdminKeepsOnlyTheInApplicationChecks(t *testing.T) {
	native := auth.User{
		ID: "1", UserID: "1", TokenID: "5", AuthType: "token", NativeClientID: "desktop",
		// What nativeauth.AccessValidator sets; the middleware derives the
		// kind from NativeClientID either way.
		CredentialKind: auth.CredentialKindNative,
	}
	inherit := auth.User{ID: "1", UserID: "1", TokenID: "6", AuthType: "token", CredentialKind: auth.CredentialKindToken}
	none := auth.User{ID: "1", UserID: "1", TokenID: "7", AuthType: "token", CredentialKind: auth.CredentialKindToken}
	access := map[string]legacyrbac.TokenAdminAccess{
		"6": {Access: legacyrbac.TokenAdminAccessInherit},
		"7": {Access: legacyrbac.TokenAdminAccessNone},
	}

	serve := func(principal auth.User, method, path string) (*httptest.ResponseRecorder, []auth.User) {
		var seen []auth.User
		router := NewRouter(RouterConfig{
			AuthValidator:      testTokenValidator{user: principal},
			PrincipalValidator: testPrincipalValidator{},
			PermissionResolver: credentialKindResolver{owner: credentialKindAdminSet, access: access, seen: &seen},
		})
		request := httptest.NewRequest(method, path, nil)
		request.Header.Set("Authorization", "Bearer "+testAuthToken)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response, seen
	}

	response, seen := serve(native, http.MethodGet, "/api/v2/auth/permissions/administration")
	if response.Code != http.StatusOK {
		t.Fatalf("the native session's own administration list answered %d (%s)",
			response.Code, response.Body.String())
	}
	var listed []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	names := make([]string, 0, len(listed))
	for _, permission := range listed {
		names = append(names, permission.Name)
	}
	want := []string{eliteacore.PublishedAgentsListPermission, apimw.MaintenanceAdminPermission}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("a native session of an admin resolves %v, want exactly the maintenance bypass and "+
			"Marketplace moderation names %v", names, want)
	}
	if len(seen) == 0 || seen[0].CredentialKind != auth.CredentialKindNative {
		t.Fatalf("the resolver was asked about %+v, want a native credential", seen)
	}

	const identityProviders = "/api/v2/admin/identity_providers/administration"
	if response, _ := serve(native, http.MethodGet, identityProviders); response.Code != http.StatusForbidden {
		t.Fatalf("a native session of an admin got %d on %s, want 403", response.Code, identityProviders)
	}
	if response, _ := serve(none, http.MethodGet, identityProviders); response.Code != http.StatusForbidden {
		t.Fatalf("a `none` token of an admin got %d on %s, want 403", response.Code, identityProviders)
	}
	// The control: the gate admits the owner's set through a token that
	// carries it, so the 403 above is the credential, not the route.
	if response, _ := serve(inherit, http.MethodGet, identityProviders); response.Code == http.StatusForbidden ||
		response.Code == http.StatusUnauthorized {
		t.Fatalf("an `inherit` token of an admin got %d on %s, want the gate to admit it",
			response.Code, identityProviders)
	}
}

// TestEveryAuthConfigLiteralCarriesATokenValidator is the structural half.
//
// cmd/elitea-main's TestNoPrivateAuthConfigLiteralsInMain allows ONE
// composition root there. This package and the route packages below it still
// build literals of their own — router.go copies RouterConfig's fields into
// three, and production_runtime.go builds one — and the #289 defect was one of
// those, short by exactly the Validator field. So the rule here is per field:
// a literal that names any field must name Validator. The empty literal
// (`apimw.AuthConfig{}`) is exempt: it is the "admits nothing" composition a
// deployment with no credential plane gets, and it names nothing at all.
func TestEveryAuthConfigLiteralCarriesATokenValidator(t *testing.T) {
	roots := []string{"..", filepath.Join("..", "..", "cmd")}
	fileSet := token.NewFileSet()
	scanned, literals := 0, 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(fileSet, path, nil, 0)
			if parseErr != nil {
				return parseErr
			}
			scanned++
			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.CompositeLit)
				if !ok || !isAuthConfigType(literal.Type) {
					return true
				}
				literals++
				if len(literal.Elts) == 0 {
					return true
				}
				if !literalNamesField(literal, "Validator") {
					t.Errorf("%s builds an apimw.AuthConfig with no Validator. A nil Validator "+
						"refuses every bearer token and API key with 401 token_rejected "+
						"(#289, regression findings F2 and C2). Copy the group's Validator.",
						fileSet.Position(literal.Pos()))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	// "Nothing found" must not read as "nothing wrong".
	if scanned < 100 || literals < 4 {
		t.Fatalf("scanned %d files and %d AuthConfig literals: the guard did not reach the code it guards",
			scanned, literals)
	}
}

func isAuthConfigType(expr ast.Expr) bool {
	switch typed := expr.(type) {
	case *ast.SelectorExpr:
		return typed.Sel != nil && typed.Sel.Name == "AuthConfig"
	case *ast.Ident:
		return typed.Name == "AuthConfig"
	default:
		return false
	}
}

func literalNamesField(literal *ast.CompositeLit, field string) bool {
	for _, element := range literal.Elts {
		keyed, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := keyed.Key.(*ast.Ident); ok && key.Name == field {
			return true
		}
	}
	return false
}
