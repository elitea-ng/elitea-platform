package api

// The auth-plane half of "share a conversation by link".
//
// The handler suite proves what each route DOES. It cannot prove which
// middleware each route runs behind, and that is the property this feature
// lives or dies on: three of the five routes must be authenticated and two of
// them must not be. Both directions are failures.
//
// A route that lost its authentication is an obvious disaster. A route that
// GAINED it is the quiet one: the shared page would bounce every anonymous
// visitor to a login screen, the owner who issued the link would never see it
// (they are signed in), and the feature would look like it worked.
//
// These tests assert on the STATUS the composed production router produces for
// an unauthenticated request, which is the only place the middleware stack is
// real.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	scimapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/scim"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/sharedchat"
)

// refusingSharedChatStore answers ErrNoLink for everything. It is what a real
// deployment answers for a token nobody issued, so a test that reaches it sees
// a 404 FROM THE HANDLER — distinguishable from the 401 an auth middleware
// would produce first, which is the whole discrimination here.
type refusingSharedChatStore struct{}

func (refusingSharedChatStore) Create(context.Context, sharedchat.CreateInput) (sharedchat.Link, error) {
	return sharedchat.Link{}, sharedchat.ErrNoLink
}

func (refusingSharedChatStore) ListByConversation(context.Context, string, int64) ([]sharedchat.Link, error) {
	return nil, nil
}

func (refusingSharedChatStore) Revoke(context.Context, string, int64, int64) error {
	return sharedchat.ErrNoLink
}

func (refusingSharedChatStore) ResolveByTokenHash(context.Context, []byte) (sharedchat.Resolved, error) {
	return sharedchat.Resolved{}, sharedchat.ErrNoLink
}

func (refusingSharedChatStore) RecordAccess(context.Context, int64) error { return nil }

func sharedChatRouter() http.Handler {
	return NewRouter(RouterConfig{
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		SharedChatStore:    refusingSharedChatStore{},
	})
}

// TestSharedChatViewIsReachableWithoutAuthentication is the positive
// assertion, and it is careful about WHICH refusal it accepts.
//
// A 404 here would be ambiguous if the route were simply not registered, so the
// test does not settle for "not 401". It first proves that an authenticated
// route in the same router DOES answer 401 for the same unauthenticated
// request — establishing that the auth middleware is live in this composition —
// and only then requires the anonymous route to answer something else.
// Otherwise a composition with no auth middleware at all would pass.
func TestSharedChatViewIsReachableWithoutAuthentication(t *testing.T) {
	router := sharedChatRouter()

	authenticated := httptest.NewRequest(http.MethodGet,
		"/api/v2/elitea_core/shared_chat_links/prompt_lib/1/2", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, authenticated)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("precondition: the owner-facing listing answered %d, want 401 — "+
			"without a live auth middleware this test proves nothing", rec.Code)
	}

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"view", http.MethodGet, "/api/v2/elitea_core/shared_chat_view/prompt_lib/AAAAAAAAAAAA", ""},
		{"unlock", http.MethodPost, "/api/v2/elitea_core/shared_chat_view_unlock/prompt_lib/AAAAAAAAAAAA/unlock", `{"password":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code == http.StatusUnauthorized {
				t.Fatalf("%s %s answered 401 for an anonymous caller — the shared page "+
					"would bounce every visitor it exists for", tc.method, tc.path)
			}
			if rec.Code == http.StatusNotFound && rec.Body.Len() == 0 {
				t.Fatalf("%s %s answered an empty 404 — the route is not registered at all",
					tc.method, tc.path)
			}
		})
	}
}

// TestSharedChatOwnerRoutesRequireAuthentication is the other direction. The
// three management routes must never become anonymous: creating a link
// publishes a conversation, and revoking one is the only way to take that back.
func TestSharedChatOwnerRoutesRequireAuthentication(t *testing.T) {
	router := sharedChatRouter()

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v2/elitea_core/shared_chat_links/prompt_lib/1/2"},
		{http.MethodPost, "/api/v2/elitea_core/shared_chat_links/prompt_lib/1/2"},
		{http.MethodDelete, "/api/v2/elitea_core/shared_chat_link/prompt_lib/1/2/3"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestSharedChatRoutesAreAbsentWithoutAStore pins the nil gate: a deployment
// that has not wired the store registers NEITHER half, rather than registering
// the anonymous half over a nil interface and panicking on the first visitor.
//
// MEASURED, not assumed: the unregistered path answers 401, NOT 404. With the
// root-level registration gone, the request falls through to the /api/v2
// subrouter, whose Auth middleware runs BEFORE chi resolves the path and
// refuses an anonymous caller without ever discovering there is no such route.
// That is the "401 before routing" behaviour this codebase has been caught by
// before, and it is why the reachability test above establishes its own
// precondition instead of reading a 401 as "authenticated route".
//
// The assertion is therefore "not served", expressed as the two statuses that
// mean it: never a 200, and never a body from the shared-chat handler.
func TestSharedChatRoutesAreAbsentWithoutAStore(t *testing.T) {
	router := NewRouter(RouterConfig{
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})
	req := httptest.NewRequest(http.MethodGet,
		"/api/v2/elitea_core/shared_chat_view/prompt_lib/AAAAAAAAAAAA", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("an unwired store served a shared conversation: %s", rec.Body.String())
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (the /api/v2 Auth middleware refusing before "+
			"chi discovers the route is unregistered)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "shared conversation not found") {
		t.Fatalf("the shared-chat handler ran with no store: %s", rec.Body.String())
	}
}

// TestSharedChatUnlockBudgetIsKeyedOnTheTrustedClientAddress proves the
// composed router hands the deployment's trusted-proxy resolver to the unlock
// route: behind a trusted proxy two callers have separate attempt budgets.
// (That a direct caller cannot pick its key with X-Forwarded-For is covered in
// the sharedchat package.)
func TestSharedChatUnlockBudgetIsKeyedOnTheTrustedClientAddress(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the real password KDF a dozen times")
	}
	resolver, err := scimapi.NewClientAddressResolver([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(RouterConfig{
		AuthValidator:       testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:  testPrincipalValidator{},
		SharedChatStore:     refusingSharedChatStore{},
		SCIMClientAddresses: resolver,
	})
	const token = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	unlock := func(peer, forwardedFor string) int {
		req := httptest.NewRequest(http.MethodPost, strings.Replace(SharedChatUnlockPath, "{token}", token, 1),
			strings.NewReader(`{"password":"x"}`))
		req.RemoteAddr = peer
		req.Header.Set("X-Forwarded-For", forwardedFor)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	// Behind the trusted proxy the forwarded client is the key.
	for i := 0; i < 10; i++ {
		if got := unlock("10.0.0.1:1", "203.0.113.200"); got != http.StatusForbidden {
			t.Fatalf("forwarded client attempt %d status = %d, want 403", i+1, got)
		}
	}
	if got := unlock("10.0.0.1:1", "203.0.113.200"); got != http.StatusTooManyRequests {
		t.Fatalf("forwarded client attempt 11 status = %d, want 429", got)
	}
	if got := unlock("10.0.0.1:1", "203.0.113.201"); got != http.StatusForbidden {
		t.Fatalf("a second client behind the same proxy status = %d, want 403 (separate budget)", got)
	}
}
