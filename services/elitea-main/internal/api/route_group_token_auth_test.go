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

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
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
		productionRuntimePeerVerifierFunc(func(*http.Request) error { return http.ErrNoCookie }),
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
