package adminui

// The admin shell after shared migration 0117.
//
// THE DEFECT THIS EXISTS TO PREVENT. This file's own comment already records
// an empty admin sidebar, caused by reading one credential source and not the
// other. 0117 opens the identical hole from the other side: the
// `elitea_session` cookie now carries an OPAQUE identifier and no claims, so
// the HMAC reader finds nothing in it, `Permissions` stays empty, and every
// nav item disappears again. The operator sees their own avatar and nothing
// else, and ten implemented pages are reachable only by typing a URL.
//
// Both cookie shapes are therefore held here, and so is the refusal: a session
// the store does not accept must inject NO identity, never a partial one.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

// stubSessions answers one prepared result, so this file tests what the SHELL
// does with each answer. browsersession's own suite proves which state
// produces which answer.
type stubSessions struct {
	session browsersession.Session
	err     error
}

func (s stubSessions) Validate(context.Context, string) (browsersession.Session, error) {
	if s.err != nil {
		return browsersession.Session{}, s.err
	}
	return s.session, nil
}

func adminSession() browsersession.Session {
	now := time.Now().UTC()
	return browsersession.Session{
		ID: "identifier", UserID: 42, Email: "member@example.com",
		Provider:  browsersession.ProviderOIDC,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
		IdleTimeout: time.Hour,
	}
}

func adminResolver(permissions ...string) auth.PermissionResolver {
	return resolverFunc(func(
		context.Context, auth.User, string, string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{Permissions: permissions}, nil
	})
}

func serverSessionCookie(t *testing.T) string {
	t.Helper()
	id, err := browsersession.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return browsersession.CookieValue(id)
}

// TestServeSPA_ServerSideSessionFillsTheOperatorIn is the empty-sidebar guard.
func TestServeSPA_ServerSideSessionFillsTheOperatorIn(t *testing.T) {
	t.Parallel()

	injected := serveAdminIndex(t, Config{
		Resolver: adminResolver("models.applications.detail"),
		Sessions: stubSessions{session: adminSession()},
	}, serverSessionCookie(t))

	// `user_id` is `any` on the injected object, so it round-trips as a JSON
	// number.
	if injected.UserID != float64(42) {
		t.Fatalf("user_id = %v, want 42 — an opaque cookie the shell cannot read "+
			"is the empty-sidebar defect", injected.UserID)
	}
	if injected.UserEmail != "member@example.com" || injected.UserName != "member@example.com" {
		t.Fatalf("email/name = %q/%q, want the session's address",
			injected.UserEmail, injected.UserName)
	}
	if len(injected.Permissions) != 1 || injected.Permissions[0] != "models.applications.detail" {
		t.Fatalf("permissions = %v, want the resolver's answer", injected.Permissions)
	}
}

// TestServeSPA_RefusedServerSideSessionInjectsNothing. A revoked, expired or
// unknown session is not a partial operator.
func TestServeSPA_RefusedServerSideSessionInjectsNothing(t *testing.T) {
	t.Parallel()

	for _, refusal := range []error{
		browsersession.ErrNotFound,
		browsersession.ErrRevoked,
		browsersession.ErrExpired,
		browsersession.ErrIdle,
		errors.New("connection refused"),
	} {
		injected := serveAdminIndex(t, Config{
			Resolver: adminResolver("models.applications.detail"),
			Sessions: stubSessions{err: refusal},
		}, serverSessionCookie(t))

		if injected.UserID != nil || injected.UserEmail != "" || len(injected.Permissions) != 0 {
			t.Fatalf("a session refused with %v injected %+v, want nothing", refusal, injected)
		}
	}
}

// TestServeSPA_LegacyCookieStillFillsTheOperatorIn. A deployment mid-upgrade
// holds signed cookies from the previous release; the shell must keep reading
// them, and the PREFIX is what decides which reader runs.
func TestServeSPA_LegacyCookieStillFillsTheOperatorIn(t *testing.T) {
	t.Parallel()

	injected := serveAdminIndex(t, Config{
		Resolver: adminResolver("models.applications.detail"),
		// The store is configured AND would refuse everything. A legacy cookie
		// must never reach it.
		Sessions: stubSessions{err: browsersession.ErrNotFound},
	}, validCookie(t))

	if injected.UserID != float64(42) || injected.UserEmail != "member@example.com" {
		t.Fatalf("injected %+v, want the legacy cookie's operator", injected)
	}
}
