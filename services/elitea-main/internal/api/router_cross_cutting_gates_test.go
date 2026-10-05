package api

// Every /api/v2 route gets the API group's cross-cutting middleware, wherever
// it is mounted.
//
// THE DEFECT. mountReviewedProductionRoutes registered its ~50 routes on the
// ROOT router, beside — not inside — the r.Group that applies NoStore,
// Maintenance, the ADR-0025 minimum client version and Audit. The routes carry their
// own Auth, so nothing looked wrong: they authenticated, they authorized, they
// answered. They also answered GET /api/v2/projects/project/default/1 and the
// notification list without `Cache-Control: no-store`, served a native client
// below min_client_version instead of answering 426, and stayed open to every
// caller during a maintenance window; the configuration writes among them
// changed project credentials with no audit event. The artifact routes,
// mounted the same way, had the first three gaps (/api/v2/artifacts is not
// an audited surface). Found by an E2E run, not by any suite: each
// gate's own test sends its request to a route INSIDE the group.
//
// WHY BEHAVIOURAL. chi.Walk hands over a route's middleware as anonymous
// closures, and the reviewed routes carry their gates inside the handler (see
// apimw.AfterAuthentication), where no walk can see them. So this asks each
// route the question directly, the way router_unauthenticated_surface_test.go
// does for authentication.
//
// THE NEXT ROOT MOUNT FAILS HERE. The walk covers every route the composed
// router registers under /api/v2. A route added to the root mux, outside the
// group and outside the gated reviewed group, answers without the directive
// and without the gates, and this test names it. The exemptions below are the
// surfaces that are outside the group ON PURPOSE, each with the reason.
//
// The maintenance half needs the switch in PostgreSQL, so it is in
// router_cross_cutting_gates_postgres_integration_test.go and walks the same
// route list with the same exemptions.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	applicationskillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applicationskills"
	configurationapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	indextypesapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indextypes"
	inventoryapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/inventory"
	notificationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/notifications"
	projectinfoapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projectinfo"
	v2projects "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projects"
	promptcontextreadsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/promptcontextreads"
	v2social "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/nativepolicy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
)

// gateNativeClient is the registered client the native principal carries.
const gateNativeClient = "ai.elitea.ios"

// gateValidator authenticates `native-*` bearers as a native principal and
// anything else as a personal access token — the two shapes the client
// version gate tells apart.
type gateValidator struct{}

func (gateValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	user := auth.User{ID: "7", UserID: "7", TokenID: "70", Email: "gates@example.test", AuthType: "token"}
	if strings.HasPrefix(token, "native-") {
		user.NativeClientID = gateNativeClient
	}
	return user, nil
}

// gatePermissions grants nothing. No request in this file reaches a
// permission check that matters: every probe is answered by a gate that runs
// before it, or by authentication.
type gatePermissions struct{}

func (gatePermissions) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{}, nil
}

func (gatePermissions) ResolveMembershipPermissions(context.Context, auth.User, string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{}, nil
}

type gatePeerVerifier struct{}

func (gatePeerVerifier) VerifyForwardedIdentityPeer(*http.Request) error { return http.ErrNoCookie }

// outsideTheGroupByDesign are /api/v2 surfaces deliberately mounted outside
// the API group. They are exempt from EVERY check here.
//
// Not a dumping ground: an entry asserts that the surface must answer without
// a principal (so neither gate can judge it) and chooses its own caching.
var outsideTheGroupByDesign = []struct{ prefix, reason string }{
	{"/api/v2/branding", "the brand pack and assets a browser loads before it has a session; it chooses its own revalidating Cache-Control."},
	{SharedChatViewPath, "share-by-link, the ANONYMOUS half (shared_chat_routes.go); no principal to gate."},
	{SharedChatUnlockPath, "share-by-link unlock, anonymous for the same reason."},
	{"/api/v2/pipeline_trigger/", "the inbound pipeline trigger; its only credential is the per-pipeline secret, so there is no principal to gate."},
}

// ownCredentialPlane are surfaces that carry no-store but authenticate a
// caller the two gates do not describe. Exempt from the gate checks only.
var ownCredentialPlane = []struct{ prefix, reason string }{
	{"/api/v2/scim/", "SCIM clients, not users: no administration permission to resolve, and maintenance is deliberately not mounted (router.go, the SCIM group's comment)."},
	{"/api/v2/auth/native/", "the native authorization server: it applies the minimum version INLINE (nativeauth token.go) and /api/v2/auth is on the maintenance allowlist."},
}

func hasPrefixIn(pattern string, list []struct{ prefix, reason string }) bool {
	for _, entry := range list {
		if pattern == entry.prefix || strings.HasPrefix(pattern, entry.prefix) {
			return true
		}
	}
	return false
}

// gatedAPIRoute is one route the walk found.
type gatedAPIRoute struct{ method, pattern string }

// apiRoutesUnderTest walks the router and returns every /api/v2 route the
// checks apply to, plus those also subject to the two principal gates.
func apiRoutesUnderTest(t *testing.T, router chi.Router) (all, gated []gatedAPIRoute) {
	t.Helper()
	err := chi.Walk(router, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(pattern, "/api/v2/") {
			return nil
		}
		if method == http.MethodConnect || method == http.MethodTrace {
			return nil
		}
		// The doubled-prefix compatibility shim re-dispatches to the real
		// route; it is plumbing, not a route of its own.
		if strings.HasPrefix(pattern, "/api/v2/api/v2/") {
			return nil
		}
		if hasPrefixIn(pattern, outsideTheGroupByDesign) {
			return nil
		}
		route := gatedAPIRoute{method: method, pattern: pattern}
		all = append(all, route)
		if !hasPrefixIn(pattern, ownCredentialPlane) {
			gated = append(gated, route)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}
	return all, gated
}

// gatesRouterConfig composes the widest /api/v2 surface, with EVERY reviewed
// route built the way the composition root builds it: wrapped in its own
// apimw.Auth over the group's AuthConfig. A reviewed route given a stub that
// skipped authentication would prove nothing about where the gates run.
func gatesRouterConfig(t *testing.T) RouterConfig {
	t.Helper()
	group := apimw.AuthConfig{
		Validator:                 gateValidator{},
		PrincipalValidator:        testPrincipalValidator{},
		ForwardedIdentityVerifier: gatePeerVerifier{},
	}
	permissions := gatePermissions{}
	reached := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"reached":true}`)
	})
	authenticated := apimw.Auth(group)(reached)

	cfg := fullSurfaceRouterConfig(t)
	cfg.AuthValidator = group.Validator
	cfg.PrincipalValidator = group.PrincipalValidator
	cfg.Auth.ForwardedIdentityVerifier = group.ForwardedIdentityVerifier

	policy := platformconfig.DefaultNativeClientPolicy()
	policy.MinClientVersion = "1.0.0"
	cfg.NativePolicy = nativepolicy.NewWithLoader(func(context.Context) (platformconfig.NativeClientPolicy, error) {
		return policy, nil
	}, nil)

	// Every reviewed field typed as a bare http.Handler, by reflection, so a
	// Current* field added later is composed here without anyone editing
	// this file.
	value := reflect.ValueOf(&cfg).Elem()
	handlerType := reflect.TypeOf((*http.Handler)(nil)).Elem()
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		if field.Type == handlerType && strings.HasPrefix(field.Name, "Current") {
			value.Field(i).Set(reflect.ValueOf(authenticated))
		}
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var err error
	cfg.ProductionRuntime, err = NewProductionRuntimeRoutes(reached, reached, group.PrincipalValidator, group.ForwardedIdentityVerifier, group)
	must(err)
	cfg.CurrentProjectList, err = v2projects.NewCurrentProjectListRoute(
		struct {
			v2projects.CurrentProjectLister
		}{}, group, permissions)
	must(err)
	cfg.CurrentProjectInfo, err = projectinfoapi.NewCurrentProjectInfoRoute(
		struct {
			projectinfoapi.CurrentProjectInfoReader
		}{}, group, permissions)
	must(err)
	cfg.CurrentIndexTypes, err = indextypesapi.NewCurrentIndexTypesRoute(
		struct {
			indextypesapi.CurrentIndexTypesReader
		}{}, group, permissions)
	must(err)
	cfg.CurrentApplicationSkills, err = applicationskillsapi.NewCurrentApplicationSkillsRoute(
		struct {
			applicationskillsapi.CurrentApplicationSkillsReader
		}{}, group, permissions)
	must(err)
	cfg.CurrentPromptContextReads, err = promptcontextreadsapi.NewCurrentRoutes(
		struct {
			promptcontextreadsapi.CurrentChatConfigReader
		}{},
		struct {
			promptcontextreadsapi.CurrentProjectContextReader
		}{},
		group, permissions)
	must(err)
	cfg.CurrentSocialAuthors, err = v2social.NewCurrentAuthorsRoute(
		struct{ v2social.CurrentAuthorsReader }{}, group, permissions)
	must(err)
	cfg.CurrentSocialAvatar, err = v2social.NewCurrentAvatarRoute(
		struct{ v2social.CurrentAvatarStore }{}, nil, group, permissions)
	must(err)
	// The provider facade, built for real (it refuses to exist without an
	// mTLS client): its routes authenticate through facade.Guard, a path the
	// stubs above do not take. DeepWiki is built on the same facade
	// (providerhost/routes) and needs a credential plane besides, so the
	// inventory facade stands for both.
	cfg.DeepWiki = nil
	cfg.Inventory, err = inventoryapi.NewRoute(gateFacadeConfig(t), group, permissions, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	must(err)
	return cfg
}

// gateFacadeConfig is an mTLS client configuration the facade accepts. Nothing
// dials it: every probe is answered before the facade forwards.
func gateFacadeConfig(t *testing.T) facade.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "gates.internal"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"gates.internal"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, blockType string, bytes []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: bytes}), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	certificate := write("client.pem", "CERTIFICATE", der)
	return facade.Config{
		Enabled:        true,
		BaseURL:        "https://gates.internal:1",
		ClientCertFile: certificate,
		ClientKeyFile:  write("client-key.pem", "EC PRIVATE KEY", keyDER),
		CAFile:         certificate,
		IdentitySecret: strings.Repeat("s", 32),
		Timeout:        time.Second,
	}
}

// requireRootMountedRoutesWalked proves the walk reached the surfaces this
// defect lived on, so a config that silently stopped composing them cannot
// turn this into a pass over the group alone.
func requireRootMountedRoutesWalked(t *testing.T, routes []gatedAPIRoute) {
	t.Helper()
	seen := map[string]bool{}
	for _, route := range routes {
		seen[route.method+" "+route.pattern] = true
	}
	for _, want := range []string{
		"GET " + v2projects.CurrentProjectListPath,
		"GET " + notificationsapi.CurrentNotificationsPath,
		"GET " + projectinfoapi.CurrentProjectInfoPath,
		"GET " + inventoryapi.SlotsPath,
		"GET /api/v2" + runtimeEventsPath,
		"GET /api/v2/artifacts/buckets/{projectID}",
	} {
		if !seen[want] {
			t.Errorf("the walk did not reach %s: the guard config no longer composes it", want)
		}
	}
	// The floor, measured: this config registers ~520 /api/v2 routes. Far
	// below that, the router did not compose and the checks measured nothing.
	const minimum = 450
	if len(routes) < minimum {
		t.Fatalf("only %d /api/v2 routes walked (floor %d)", len(routes), minimum)
	}
}

func failRoutes(t *testing.T, what, remedy string, failures []string) {
	t.Helper()
	if len(failures) == 0 {
		return
	}
	sort.Strings(failures)
	t.Fatalf("%d /api/v2 route(s) %s:\n  %s\n\n%s", len(failures), what, strings.Join(failures, "\n  "), remedy)
}

const gateRemedy = "Mount the route inside the /api/v2 group in router.go, or — for a handler that carries " +
	"its own apimw.Auth — in the gated group mountReviewedProductionRoutes builds (NoStore + " +
	"apimw.AfterAuthentication(apiGates..., Audit)). A surface that is outside the group on purpose goes in " +
	"outsideTheGroupByDesign / ownCredentialPlane in this file WITH the reason."

// TestEveryAPIRouteAnswersNoStore: the directive is on every answer, including
// the 401 a caller with no credential gets — NoStore sits in front of Auth in
// both the group and the reviewed group, so the cheapest request that reaches
// a route proves it is mounted there.
func TestEveryAPIRouteAnswersNoStore(t *testing.T) {
	router := NewRouter(gatesRouterConfig(t))
	all, _ := apiRoutesUnderTest(t, router)
	requireRootMountedRoutesWalked(t, all)

	var failures []string
	for _, route := range all {
		request := httptest.NewRequest(route.method, concretePath(route.pattern), nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if got := response.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
			failures = append(failures, fmt.Sprintf("%s %s -> %d, Cache-Control %q", route.method, route.pattern, response.Code, got))
		}
	}
	failRoutes(t, "answered without Cache-Control: no-store", gateRemedy, failures)
	t.Logf("%d /api/v2 routes answered with no-store", len(all))
}

// TestEveryAPIRouteIsSubjectToTheClientVersionGate: a native principal stating
// a version below the minimum is answered 426 on every route, before any
// handler runs — and a personal access token stating the same version is not,
// which is the control that proves the 426 came from the gate's own rule.
func TestEveryAPIRouteIsSubjectToTheClientVersionGate(t *testing.T) {
	router := NewRouter(gatesRouterConfig(t))
	all, gated := apiRoutesUnderTest(t, router)
	requireRootMountedRoutesWalked(t, all)

	var failures []string
	for _, route := range gated {
		if route.pattern == "/api/v2/auth/native/revoke" {
			continue
		}
		request := httptest.NewRequest(route.method, concretePath(route.pattern), strings.NewReader("{}"))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer native-gates")
		request.Header.Set(apimw.ClientVersionHeader, "0.0.1")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusUpgradeRequired {
			failures = append(failures, fmt.Sprintf("%s %s -> %d", route.method, route.pattern, response.Code))
		}
	}
	failRoutes(t, "served a native client below min_client_version instead of answering 426", gateRemedy, failures)
	t.Logf("%d /api/v2 routes answered an outdated native client 426", len(gated))

	// The control, on one root-mounted route: a PAT is exempt (decision 13).
	request := httptest.NewRequest(http.MethodGet, concretePath(notificationsapi.CurrentNotificationsPath), nil)
	request.Header.Set("Authorization", "Bearer pat-gates")
	request.Header.Set(apimw.ClientVersionHeader, "0.0.1")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code == http.StatusUpgradeRequired {
		t.Fatal("a personal access token was answered 426: the gate no longer tells native principals apart")
	}
}

// TestEveryAuditedMutatingAPIRouteIsAudited: the group's last cross-cutting
// gate is apimw.Audit, below Auth and the two principal gates. A write to an
// audited surface (apimw.AuditedSurfaces — admin, credentials, configurations,
// projects, ...) leaves an audit event whichever way it ends, so every
// mutating route on those surfaces must produce one for an authenticated
// caller. A root-mounted route outside the group writes nothing: before this
// test, POST /api/v2/configurations/configurations/{projectID} — where a
// project's ai_credentials are written — created, overwrote and deleted
// credentials with no centry.audit_events row.
func TestEveryAuditedMutatingAPIRouteIsAudited(t *testing.T) {
	recorder := &routerAuditRecorder{}
	cfg := gatesRouterConfig(t)
	cfg.AuditRecorder = recorder
	router := NewRouter(cfg)
	all, gated := apiRoutesUnderTest(t, router)
	requireRootMountedRoutesWalked(t, all)

	audited := func(path string) bool {
		for _, prefix := range apimw.AuditedSurfaces() {
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}
		return false
	}

	var failures []string
	checked, reviewed := 0, 0
	for _, route := range gated {
		path := concretePath(route.pattern)
		if route.method == http.MethodGet || route.method == http.MethodHead || route.method == http.MethodOptions || !audited(path) {
			continue
		}
		checked++
		if route.method == http.MethodPost && route.pattern == configurationapi.CurrentConfigurationListPath {
			reviewed++
		}
		before := len(recorder.all())
		request := httptest.NewRequest(route.method, path, strings.NewReader("{}"))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer pat-gates")
		response := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }()
			router.ServeHTTP(response, request)
		}()
		if len(recorder.all()) == before {
			failures = append(failures, fmt.Sprintf("%s %s -> %d", route.method, route.pattern, response.Code))
		}
	}
	if reviewed == 0 {
		t.Fatalf("the walk did not reach POST %s: the guard config no longer composes the reviewed configurations route", configurationapi.CurrentConfigurationListPath)
	}
	failRoutes(t, "wrote to an audited surface without an audit event", gateRemedy, failures)
	t.Logf("%d mutating routes on audited surfaces each left an audit event", checked)
}
