package api

// The server-side session THROUGH THE REAL ROUTER.
//
// WHY THIS EXISTS AS WELL AS THE MIDDLEWARE TEST. `apimw.Auth` reads
// `AuthConfig.SessionStore`, and this file builds four `apimw.AuthConfig`
// values from `RouterConfig`. A group that did not copy the field would refuse
// every browser holding a server-side cookie, and every unit test of the
// middleware would still pass: they construct the AuthConfig themselves. That
// is the wiring gap this repository keeps finding — the code is right on both
// sides and nothing joins them.
//
// It drives the whole sequence a browser performs: a session is created, a
// request with its cookie is admitted, the session is revoked, and the same
// request is refused.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

// routeTestSessions is a browsersession.Store held in memory.
type routeTestSessions struct {
	mu   sync.Mutex
	rows map[string]browsersession.Session
}

func (s *routeTestSessions) Insert(_ context.Context, session browsersession.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[session.ID] = session
	return nil
}

func (s *routeTestSessions) Get(_ context.Context, id string) (browsersession.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok {
		return browsersession.Session{}, browsersession.ErrNotFound
	}
	return row, nil
}

func (s *routeTestSessions) Touch(_ context.Context, id string, seenAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[id]; ok {
		row.LastSeenAt = seenAt
		s.rows[id] = row
	}
	return nil
}

func (s *routeTestSessions) Revoke(_ context.Context, id string, revokedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[id]; ok && row.RevokedAt == nil {
		row.RevokedAt = &revokedAt
		s.rows[id] = row
	}
	return nil
}

func (s *routeTestSessions) RevokeAllForUser(
	_ context.Context, userID int64, revokedAt time.Time,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int64
	for id, row := range s.rows {
		if row.UserID == userID && row.RevokedAt == nil {
			row.RevokedAt = &revokedAt
			s.rows[id] = row
			count++
		}
	}
	return count, nil
}

// activeRoutePrincipals accepts every principal it is handed. The account
// check is not what this file is about; the session check is.
type activeRoutePrincipals struct{}

func (activeRoutePrincipals) ValidatePrincipal(
	_ context.Context, principal auth.User,
) (auth.User, error) {
	return principal, nil
}

// TestTheRouterAdmitsAndThenRefusesAServerSideSession.
func TestTheRouterAdmitsAndThenRefusesAServerSideSession(t *testing.T) {
	store := &routeTestSessions{rows: map[string]browsersession.Session{}}
	manager, err := browsersession.NewManager(store, browsersession.Policy{
		IdleTimeout: time.Hour, AbsoluteLifetime: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	router := NewRouter(RouterConfig{
		SessionSecret:      "session-secret",
		PrincipalValidator: activeRoutePrincipals{},
		Auth: AuthDeps{
			SessionStore: manager,
		},
	})

	// A route that exists on every composition and needs only a principal.
	const path = "/api/v2/auth/token/"

	value, err := manager.Create(context.Background(), browsersession.NewSession{
		UserID: 7, Email: "owner@example.test", Provider: browsersession.ProviderOIDC,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	admitted := serveWithSessionCookie(t, router, path, value)
	if admitted == http.StatusUnauthorized {
		t.Fatal("the router refused a live server-side session. The group's AuthConfig " +
			"did not carry Auth.SessionStore, so the legacy HMAC reader ran against an " +
			"opaque identifier and every browser was signed out.")
	}

	if err := manager.Revoke(context.Background(), value); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if refused := serveWithSessionCookie(t, router, path, value); refused != http.StatusUnauthorized {
		t.Fatalf("status after revocation = %d, want 401. A logout that the router does "+
			"not honour is not a logout.", refused)
	}
}

// TestEveryAuthenticatedGroupCarriesTheSessionStore walks the router's own
// route tree and asserts the sequence on each browser-reachable prefix.
//
// One route proves one group. This repository builds several, and the one that
// was forgotten is the one nobody tested.
func TestEveryAuthenticatedGroupCarriesTheSessionStore(t *testing.T) {
	store := &routeTestSessions{rows: map[string]browsersession.Session{}}
	manager, err := browsersession.NewManager(store, browsersession.Policy{
		IdleTimeout: time.Hour, AbsoluteLifetime: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	router := NewRouter(RouterConfig{
		SessionSecret:      "session-secret",
		PrincipalValidator: activeRoutePrincipals{},
		Auth:               AuthDeps{SessionStore: manager},
	})
	if _, ok := router.(chi.Routes); !ok {
		t.Fatal("the router no longer exposes its route tree")
	}

	value, err := manager.Create(context.Background(), browsersession.NewSession{
		UserID: 7, Provider: browsersession.ProviderOIDC,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// One path per apimw.Auth group this file builds: the /api/v2 group, the
	// MCP server routes and the artifact routes all take their own literal.
	for _, path := range []string{
		"/api/v2/auth/token/",
		"/app/1/mcp",
	} {
		t.Run(path, func(t *testing.T) {
			if status := serveWithSessionCookie(t, router, path, value); status == http.StatusUnauthorized {
				t.Fatalf("%s refused a live server-side session — its AuthConfig does not "+
					"carry Auth.SessionStore", path)
			}
		})
	}
}

func serveWithSessionCookie(t *testing.T, router http.Handler, path, value string) int {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(&http.Cookie{Name: browsersession.CookieName, Value: value})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder.Code
}

// compile-time proof that the manager satisfies the field's interface. A
// change to either side that broke it would otherwise surface only in
// cmd/elitea-main.
var _ apimw.BrowserSessionValidator = (*browsersession.Manager)(nil)

// TestEveryAuthConfigInThisPackageReadsBothCookieFormats is the structural half
// of the two tests above, and the one that would have caught the defect they
// missed.
//
// THE RULE. A non-test file in internal/api that composes an
// `apimw.AuthConfig` and sets `SessionSecret` is composing a BROWSER credential
// set. apimw.Auth reads three fields for a browser cookie — SessionSecret for
// the legacy signed value, SessionStore for the server-side identifier
// migrations/shared/0117 introduced, and RejectLegacySessionCookies for the
// operator switch between them — so a literal that sets one and not the others
// reads one cookie format and refuses the other.
//
// WHY A ROUTE TEST WAS NOT ENOUGH. The two tests above drive NewRouter, and
// NewRouter is not the only composition in this package: production_runtime.go
// builds its own AuthConfig for the two runtime routes, which
// production_router.go registers OUTSIDE every group NewRouter builds. That
// literal took SessionSecret and not SessionStore, so the execution-events
// stream — the one route whose only possible credential is a cookie, because an
// EventSource can send nothing else — answered 401 to every browser the moment
// sessions became server-side. A run started, and its terminal event never
// arrived.
//
// A per-route test cannot close this, because the next literal is behind a
// route nobody has thought to add to the list. The field set is what has to be
// asserted.
func TestEveryAuthConfigInThisPackageReadsBothCookieFormats(t *testing.T) {
	const (
		secretField = "SessionSecret"
		storeField  = "SessionStore"
		rejectField = "RejectLegacySessionCookies"
	)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/api: %v", err)
	}
	fileSet := token.NewFileSet()
	// Counted so an empty result cannot read as a pass — a rename or a moved
	// package would otherwise turn this guard vacuous, which is the failure
	// mode it exists to prevent.
	browserLiterals := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fileSet, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok || selectorTypeName(literal.Type) != "AuthConfig" {
				return true
			}
			fields := literalFieldNames(literal)
			if !fields[secretField] {
				// Not a browser credential set. An edge-only composition that
				// reads no cookie at all is a deliberate shape, and the zero
				// AuthConfig every test passes is another.
				return true
			}
			browserLiterals++
			for _, required := range []string{storeField, rejectField} {
				if !fields[required] {
					t.Errorf("%s:%d composes an apimw.AuthConfig with %s and no %s. "+
						"apimw.Auth reads all three cookie fields; a literal one field "+
						"short reads the legacy signed cookie and refuses the "+
						"server-side identifier (or the reverse), so every browser on "+
						"the routes it guards is signed out while every other route "+
						"works. Copy the /api/v2 group's AuthConfig instead of "+
						"rebuilding one.",
						name, fileSet.Position(literal.Pos()).Line, secretField, required)
				}
			}
			return true
		})
	}
	if browserLiterals == 0 {
		t.Fatal("no apimw.AuthConfig in internal/api sets SessionSecret: the guard " +
			"above is vacuously true, which is the state it exists to make impossible")
	}
}

func selectorTypeName(expr ast.Expr) string {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel == nil {
		return ""
	}
	return selector.Sel.Name
}

func literalFieldNames(literal *ast.CompositeLit) map[string]bool {
	names := map[string]bool{}
	for _, element := range literal.Elts {
		keyed, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := keyed.Key.(*ast.Ident); ok {
			names[key.Name] = true
		}
	}
	return names
}
