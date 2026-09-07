package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	notificationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/notifications"
	v2projects "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projects"
	notificationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/notifications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

const oidcSessionTestSecret = "oidc-only-session-secret"

// grantingPermissions answers every question with the permission the route
// requires, for the user the session names. It is the load-bearing part of
// these tests: it reproduces the real condition the issue describes, where a
// deactivated user KEEPS the RBAC rows that were granted before deactivation.
// A refusal observed through it can only come from the principal validator.
type grantingPermissions struct {
	permission string
	calls      int
}

func (p *grantingPermissions) ResolvePermissions(
	_ context.Context,
	_ auth.User,
	_ string,
	_ string,
) (auth.PermissionResolution, error) {
	p.calls++
	return auth.PermissionResolution{UserID: 42, Permissions: []string{p.permission}}, nil
}

// ResolveMembershipPermissions answers the same way, so the project-list
// route's membership gate (#830) grants exactly as freely here as the
// project-scoped gate did. The refusal these tests observe still can only come
// from the principal validator.
func (p *grantingPermissions) ResolveMembershipPermissions(
	_ context.Context,
	_ auth.User,
	_ string,
) (auth.PermissionResolution, error) {
	p.calls++
	return auth.PermissionResolution{UserID: 42, Permissions: []string{p.permission}}, nil
}

// listerStub and eventReaderStub stand in for the PostgreSQL repositories. Both
// count their calls, so a test can prove the refusal happened before the
// handler read anything.
type listerStub struct{ calls int }

func (l *listerStub) ListCurrentUserProjects(
	context.Context,
	sqlcgen.ListCurrentUserProjectsParams,
) ([]sqlcgen.ListCurrentUserProjectsRow, error) {
	l.calls++
	return nil, nil
}

type eventReaderStub struct{ calls int }

func (r *eventReaderStub) HighWater(context.Context, int64) (int64, error) {
	r.calls++
	return 0, nil
}

func (r *eventReaderStub) ListAfter(
	context.Context,
	int64,
	int64,
	int32,
) ([]notificationapp.Event, error) {
	r.calls++
	return nil, nil
}

// notificationStoreStub stands in for the PostgreSQL notification repository.
// It counts every read, so a test can prove that a refusal happened before the
// handler read a row.
type notificationStoreStub struct{ calls int }

func (s *notificationStoreStub) Count(
	context.Context,
	int64,
	notificationapp.ListFilter,
) (int64, error) {
	s.calls++
	return 0, nil
}

func (s *notificationStoreStub) List(
	context.Context,
	int64,
	notificationapp.ListFilter,
) ([]notificationapp.Notification, error) {
	s.calls++
	return nil, nil
}

func (s *notificationStoreStub) Get(
	context.Context,
	int64,
	int64,
) (notificationapp.Notification, error) {
	s.calls++
	return notificationapp.Notification{}, notificationapp.ErrNotificationNotFound
}

func (s *notificationStoreStub) MarkSeen(
	context.Context,
	int64,
	int64,
) (notificationapp.Notification, error) {
	s.calls++
	return notificationapp.Notification{}, notificationapp.ErrNotificationNotFound
}

func (s *notificationStoreStub) Delete(context.Context, int64, int64) error {
	s.calls++
	return notificationapp.ErrNotificationNotFound
}

func (s *notificationStoreStub) BulkSetSeen(
	context.Context,
	int64,
	[]int64,
	bool,
	bool,
) (int64, error) {
	s.calls++
	return 0, nil
}

func (s *notificationStoreStub) BulkDelete(context.Context, int64, []int64) (int64, error) {
	s.calls++
	return 0, nil
}

// streamingRecorder adds the two methods the notification SSE writer probes
// for. httptest.ResponseRecorder alone makes newCurrentNotificationSSEWriter
// fail, which would mask the status this test measures.
type streamingRecorder struct {
	*httptest.ResponseRecorder
}

func newStreamingRecorder() *streamingRecorder {
	return &streamingRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (recorder *streamingRecorder) SetWriteDeadline(time.Time) error { return nil }

func (recorder *streamingRecorder) Flush() { recorder.ResponseRecorder.Flush() }

// TestCurrentProjectListOIDCOnlyAuthRejectsADeactivatedSession drives the REAL
// route — the composition helper, apimw.Auth, the per-project permission gate
// and the handler — with a validly signed session cookie.
//
// Before this fix the OIDC-only branch was `apimw.AuthConfig{SessionSecret:
// ...}` with no PrincipalValidator, and apimw.validatePrincipal returns the
// session user UNCHANGED when that field is nil. A deactivated user's
// unexpired cookie therefore reached the handler with 200. Deleting the
// PrincipalValidator field from apiGroupAuthConfig's OIDC-only branch turns
// the first row red.
func TestCurrentProjectListOIDCOnlyAuthRejectsADeactivatedSession(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		principals *countingPrincipals
		wantStatus int
		wantBody   string
	}{
		{
			name:       "deactivated principal is refused",
			principals: &countingPrincipals{inner: deactivatedPrincipals{}},
			wantStatus: http.StatusUnauthorized,
			wantBody:   "authenticated principal is inactive",
		},
		{
			name:       "active principal is served",
			principals: &countingPrincipals{inner: activePrincipals{}},
			wantStatus: http.StatusOK,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			permissions := &grantingPermissions{
				permission: v2projects.CurrentProjectListPermission,
			}
			projects := &listerStub{}
			route, err := v2projects.NewCurrentProjectListRoute(
				projects,
				oidcOnlySessionAuth(testCase.principals),
				permissions,
			)
			if err != nil {
				t.Fatalf("compose current project-list route: %v", err)
			}

			request := httptest.NewRequest(
				http.MethodGet,
				v2projects.CurrentProjectListPath,
				nil,
			)
			request.AddCookie(&http.Cookie{
				Name: "elitea_session",
				Value: signedSessionCookie(
					t, oidcSessionTestSecret, "42", time.Now().Add(time.Hour),
				),
			})
			recorder := httptest.NewRecorder()
			route.ServeHTTP(recorder, request)

			assertOIDCSessionOutcome(t, oidcSessionOutcome{
				status:     recorder.Code,
				body:       recorder.Body.String(),
				wantStatus: testCase.wantStatus,
				wantBody:   testCase.wantBody,
				consulted:  testCase.principals.consulted(),
				reads:      projects.calls,
			})
			// A deactivated user's grants survive deactivation, so the
			// permission gate cannot produce this refusal. Reaching it at all
			// means the principal check already passed.
			if testCase.wantStatus != http.StatusOK && permissions.calls != 0 {
				t.Fatalf("permission resolver consulted %d times on a refused "+
					"request: the session passed the principal check (#314)",
					permissions.calls)
			}
		})
	}
}

// TestCurrentNotificationEventsOIDCOnlyAuthRejectsADeactivatedSession is the
// same proof for the notification SSE stream, the route every page that mounts
// the sidebar opens (#152).
func TestCurrentNotificationEventsOIDCOnlyAuthRejectsADeactivatedSession(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		principals *countingPrincipals
		wantStatus int
		wantBody   string
	}{
		{
			name:       "deactivated principal is refused",
			principals: &countingPrincipals{inner: deactivatedPrincipals{}},
			wantStatus: http.StatusUnauthorized,
			wantBody:   "authenticated principal is inactive",
		},
		{
			name:       "active principal is served",
			principals: &countingPrincipals{inner: activePrincipals{}},
			wantStatus: http.StatusOK,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			permissions := &grantingPermissions{
				permission: notificationsapi.CurrentNotificationEventsPermission,
			}
			events := &eventReaderStub{}
			route, err := notificationsapi.NewCurrentNotificationEventsRoute(
				events,
				oidcOnlySessionAuth(testCase.principals),
				permissions,
			)
			if err != nil {
				t.Fatalf("compose current notification events route: %v", err)
			}

			// The served row opens a stream that runs until the client goes
			// away. A deadline on the request context is that departure.
			streamContext, closeStream := context.WithTimeout(
				context.Background(), 150*time.Millisecond,
			)
			defer closeStream()
			request := httptest.NewRequest(
				http.MethodGet,
				"/api/v2/notifications/events/prompt_lib/1",
				nil,
			).WithContext(streamContext)
			request.AddCookie(&http.Cookie{
				Name: "elitea_session",
				Value: signedSessionCookie(
					t, oidcSessionTestSecret, "42", time.Now().Add(time.Hour),
				),
			})
			recorder := newStreamingRecorder()
			route.ServeHTTP(recorder, request)

			assertOIDCSessionOutcome(t, oidcSessionOutcome{
				status:     recorder.Code,
				body:       recorder.Body.String(),
				wantStatus: testCase.wantStatus,
				wantBody:   testCase.wantBody,
				consulted:  testCase.principals.consulted(),
				reads:      events.calls,
			})
			if testCase.wantStatus != http.StatusOK && permissions.calls != 0 {
				t.Fatalf("permission resolver consulted %d times on a refused "+
					"request: the session passed the principal check (#314)",
					permissions.calls)
			}
		})
	}
}

// TestCurrentNotificationListOIDCOnlyAuthServesASessionCookie is the same
// proof for the notification LIST route, the route the notifications screen
// reads (#413).
//
// The served row is the part that fails without the fix. Before it,
// NewCurrentNotificationAPIRoute demanded a ForwardedIdentityVerifier, and
// only a FormGraph supplies one. An OIDC-only deployment has no FormGraph, so
// the constructor returned ErrInvalidCurrentNotificationAPIRoute, main.go left
// CurrentNotifications nil, and production_router.go registered no path. GET
// /api/v2/notifications/notifications/prompt_lib/1 then answered chi's 404.
//
// The refused row keeps the #314 guarantee on the same route: a deactivated
// user's unexpired cookie must not read notifications.
func TestCurrentNotificationListOIDCOnlyAuthServesASessionCookie(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		principals *countingPrincipals
		wantStatus int
		wantBody   string
	}{
		{
			name:       "deactivated principal is refused",
			principals: &countingPrincipals{inner: deactivatedPrincipals{}},
			wantStatus: http.StatusUnauthorized,
			wantBody:   "authenticated principal is inactive",
		},
		{
			name:       "active principal is served",
			principals: &countingPrincipals{inner: activePrincipals{}},
			wantStatus: http.StatusOK,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			permissions := &grantingPermissions{
				permission: notificationsapi.CurrentNotificationsListPermission,
			}
			store := &notificationStoreStub{}
			route, err := notificationsapi.NewCurrentNotificationAPIRoute(
				store,
				oidcOnlySessionAuth(testCase.principals),
				permissions,
			)
			if err != nil {
				t.Fatalf("compose current notification API route: %v "+
					"(an OIDC-only deployment has no FormGraph, so it can "+
					"supply no ForwardedIdentityVerifier — #413)", err)
			}

			request := httptest.NewRequest(
				http.MethodGet,
				"/api/v2/notifications/notifications/prompt_lib/1?only_new=false&limit=20&offset=0",
				nil,
			)
			request.AddCookie(&http.Cookie{
				Name: "elitea_session",
				Value: signedSessionCookie(
					t, oidcSessionTestSecret, "42", time.Now().Add(time.Hour),
				),
			})
			recorder := httptest.NewRecorder()
			route.ServeHTTP(recorder, request)

			assertOIDCSessionOutcome(t, oidcSessionOutcome{
				status:     recorder.Code,
				body:       recorder.Body.String(),
				wantStatus: testCase.wantStatus,
				wantBody:   testCase.wantBody,
				consulted:  testCase.principals.consulted(),
				reads:      store.calls,
			})
			if testCase.wantStatus != http.StatusOK && permissions.calls != 0 {
				t.Fatalf("permission resolver consulted %d times on a refused "+
					"request: the session passed the principal check (#314)",
					permissions.calls)
			}
		})
	}
}

type oidcSessionOutcome struct {
	status     int
	body       string
	wantStatus int
	wantBody   string
	consulted  int
	reads      int
}

func assertOIDCSessionOutcome(t *testing.T, outcome oidcSessionOutcome) {
	t.Helper()
	if outcome.status != outcome.wantStatus {
		t.Fatalf("status = %d, want %d (body %q)",
			outcome.status, outcome.wantStatus, outcome.body)
	}
	if outcome.wantBody != "" && !strings.Contains(outcome.body, outcome.wantBody) {
		t.Fatalf("body = %q, want it to contain %q", outcome.body, outcome.wantBody)
	}
	// A validator that is never called cannot enforce anything. This is what
	// separates "the config has a non-nil field" from "the session is actually
	// re-checked against the current principal".
	if outcome.consulted != 1 {
		t.Fatalf("PrincipalValidator consulted %d times, want 1: the session "+
			"was accepted without re-checking the principal (#314)",
			outcome.consulted)
	}
	if outcome.wantStatus != http.StatusOK && outcome.reads != 0 {
		t.Fatalf("repository read %d times on a refused request: the refusal "+
			"has already leaked the data it was meant to withhold", outcome.reads)
	}
	if outcome.wantStatus == http.StatusOK && outcome.reads == 0 {
		t.Fatalf("repository read 0 times on a served request: the control row " +
			"proves nothing, so the refusal above is not attributable to the " +
			"principal validator")
	}
}

// oidcOnlySessionAuth builds the credential set an OIDC-only deployment gives
// every browser-facing route: apiGroupAuthConfig with no FormGraph and the
// single-sign-on plane on.
//
// It is the SHARED composition, not a second one. oidcSessionAuthConfig used
// to build these three routes' AuthConfig separately, one call before
// apiGroupAuthConfig built the group's, from the same inputs. The two drifted:
// the shared one carries Validator (the pool-backed personal-access-token
// reader), and the private one never gained it, so a token every other
// /api/v2 route accepted answered 401 here. Driving the tests through the
// shared composition is what keeps that from returning.
func oidcOnlySessionAuth(principals apimw.PrincipalValidator) apimw.AuthConfig {
	return apiGroupAuthConfig(nil, nil, nil, principals, nil, oidcSessionTestSecret, true, nil)
}

// TestOIDCOnlyRoutesUseTheSharedAuthComposition guards the call sites. The
// composition is only worth anything if main.go still routes through it, and
// every route test composes its own AuthConfig, so nothing else in the build
// reads what production actually wires.
func TestOIDCOnlyRoutesUseTheSharedAuthComposition(t *testing.T) {
	file := parseMainFile(t)

	for _, constructor := range []string{
		"NewCurrentProjectListRoute",
		"NewCurrentNotificationEventsRoute",
		// The notification LIST route. main.go composed it only inside the
		// `authEnabled` block, so production_router.go registered no path on an
		// OIDC-only deployment and the screen read a 404 (#413).
		"NewCurrentNotificationAPIRoute",
	} {
		if !callPassesIdentifier(file, constructor, "apiGroupAuth") {
			t.Fatalf("%s no longer takes the apiGroupAuth composition — a "+
				"private AuthConfig there loses its PrincipalValidator (#314) "+
				"or its token validator, silently and per deployment shape",
				constructor)
		}
	}
}

// callPassesIdentifier reports whether any call to `outer` passes the bare
// identifier `argument` among its arguments.
func callPassesIdentifier(file *ast.File, outer, argument string) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || calleeName(call.Fun) != outer {
			return true
		}
		for _, candidate := range call.Args {
			if identifier, ok := candidate.(*ast.Ident); ok && identifier.Name == argument {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func parseMainFile(t *testing.T) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	return file
}

// countArgumentCalls reports how many calls to `callee` the file makes, and how
// many of those pass a call to `argument` in position `index`.
func countArgumentCalls(file *ast.File, callee string, index int, argument string) (int, int) {
	var calls, matching int
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || calleeName(call.Fun) != callee {
			return true
		}
		calls++
		if index >= len(call.Args) {
			return true
		}
		if nested, ok := call.Args[index].(*ast.CallExpr); ok &&
			calleeName(nested.Fun) == argument {
			matching++
		}
		return true
	})
	return calls, matching
}
