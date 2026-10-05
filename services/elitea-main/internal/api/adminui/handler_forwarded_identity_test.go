package adminui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// DEFECT: ServeSPA read the `elitea_session` cookie and nothing else.
//
// The runtime deployment does not issue that cookie. A browser logs in at
// /auth/login, which stores an opaque server-side session under
// `elitea_browser_auth` and projects the principal onto the upstream request as
// X-Auth-* headers (deploy/runtime/platform-edge-dynamic.yml
// `authResponseHeaders`). The cookie lookup found nothing on every load, so the
// handler injected `permissions: []` and an empty `user_name`.
//
// The SPA shows a nav item only when the injected list carries one of its
// permissions, so the operator got a sidebar with no items at all — ten
// implemented admin pages reachable only by typing their URL — and a footer
// reading the generic fallback "Admin" instead of their address.

// peerVerifier is an apimw.ForwardedIdentityPeerVerifier built from an error.
type peerVerifier struct{ err error }

func (v peerVerifier) VerifyForwardedIdentityPeer(*http.Request) error { return v.err }

type emailFunc func(context.Context, int64) (string, error)

func (f emailFunc) UserEmail(ctx context.Context, userID int64) (string, error) {
	return f(ctx, userID)
}

// serveForwarded drives the real ServeSPA with X-Auth-* headers and no cookie.
func serveForwarded(t *testing.T, cfg Config, headers map[string]string) adminUIConfig {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "index.html"),
		[]byte("<html><body><!-- admin_ui_config --></body></html>"),
		0o600,
	); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	cfg.StaticDir = dir
	cfg.SecretKey = testSecret
	cfg.BasePath = "/admin/app"

	req := httptest.NewRequest(http.MethodGet, "/admin/app", nil)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	NewHandler(cfg).ServeSPA(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	match := injectedConfig.FindStringSubmatch(rec.Body.String())
	if match == nil {
		t.Fatalf("no injected config in the served page:\n%s", rec.Body.String())
	}
	var injected adminUIConfig
	if err := json.Unmarshal([]byte(match[1]), &injected); err != nil {
		t.Fatalf("parse injected config %q: %v", match[1], err)
	}
	return injected
}

func userHeaders(userID string) map[string]string {
	return map[string]string{
		"X-Auth-Type":    "user",
		"X-Auth-ID":      userID,
		"X-Auth-User-ID": userID,
	}
}

func TestServeSPA_ForwardedIdentityGetsTheOperatorsPermissions(t *testing.T) {
	t.Parallel()

	granted := []string{"admin.auth.users", "models.admin.audit_trail.view"}
	var seenPrincipal auth.User
	var seenMode string
	cfg := Config{
		ForwardedIdentityVerifier: peerVerifier{},
		Resolver: resolverFunc(func(
			_ context.Context, principal auth.User, mode, _ string,
		) (auth.PermissionResolution, error) {
			seenPrincipal, seenMode = principal, mode
			return auth.PermissionResolution{UserID: 42, Permissions: granted}, nil
		}),
		Emails: emailFunc(func(_ context.Context, userID int64) (string, error) {
			if userID != 42 {
				t.Errorf("email lookup user = %d, want 42", userID)
			}
			return "operator@example.com", nil
		}),
	}

	injected := serveForwarded(t, cfg, userHeaders("42"))

	if len(injected.Permissions) != len(granted) {
		t.Fatalf("permissions = %v, want %v", injected.Permissions, granted)
	}
	for i, permission := range granted {
		if injected.Permissions[i] != permission {
			t.Errorf("permissions[%d] = %q, want %q", i, injected.Permissions[i], permission)
		}
	}
	if injected.UserID != float64(42) {
		t.Errorf("user_id = %v, want 42", injected.UserID)
	}
	// The footer fallback "Admin" appears only when both are empty.
	if injected.UserEmail != "operator@example.com" || injected.UserName != "operator@example.com" {
		t.Errorf("user_email/user_name = %q/%q, want operator@example.com",
			injected.UserEmail, injected.UserName)
	}
	if seenMode != auth.PermissionModeAdministration {
		t.Errorf("resolver mode = %q, want %q", seenMode, auth.PermissionModeAdministration)
	}
	if seenPrincipal.UserID != "42" || seenPrincipal.AuthType != "user" {
		t.Errorf("resolver principal = %+v, want the forwarded user 42", seenPrincipal)
	}
}

// A token principal's X-Auth-ID is the TOKEN's id, not its owner's user id.
// The whole principal must reach the resolver, which is the component that
// cross-checks the pair, and the injected user_id must be the OWNER's.
func TestServeSPA_ForwardedTokenPrincipalReachesTheResolverIntact(t *testing.T) {
	t.Parallel()

	var seenPrincipal auth.User
	cfg := Config{
		ForwardedIdentityVerifier: peerVerifier{},
		Resolver: resolverFunc(func(
			_ context.Context, principal auth.User, _, _ string,
		) (auth.PermissionResolution, error) {
			seenPrincipal = principal
			return auth.PermissionResolution{UserID: 7, Permissions: []string{"admin.auth.users"}}, nil
		}),
	}

	injected := serveForwarded(t, cfg, map[string]string{
		"X-Auth-Type":    "token",
		"X-Auth-ID":      "900",
		"X-Auth-User-ID": "7",
	})

	if seenPrincipal.TokenID != "900" || seenPrincipal.UserID != "7" {
		t.Errorf("resolver principal = %+v, want token 900 owned by user 7", seenPrincipal)
	}
	if injected.UserID != float64(7) {
		t.Errorf("user_id = %v, want the owning user 7, not the token id", injected.UserID)
	}
}

// The headers are the whole credential. Without proof that the request crossed
// the header-stripping ingress, any browser could name any user (#390).
func TestServeSPA_ForwardedIdentityWithoutPeerProofGetsNothing(t *testing.T) {
	t.Parallel()

	for name, verifier := range map[string]Config{
		"nil verifier":      {},
		"refusing verifier": {ForwardedIdentityVerifier: peerVerifier{err: errors.New("not the ingress")}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := verifier
			cfg.Resolver = resolverFunc(func(
				_ context.Context, _ auth.User, _, _ string,
			) (auth.PermissionResolution, error) {
				t.Error("the resolver must not run for an unproven forwarded identity")
				return auth.PermissionResolution{UserID: 1, Permissions: []string{"admin.auth.users"}}, nil
			})

			injected := serveForwarded(t, cfg, userHeaders("1"))

			if len(injected.Permissions) != 0 {
				t.Errorf("permissions = %v, want none", injected.Permissions)
			}
			if injected.UserID != nil {
				t.Errorf("user_id = %v, want none", injected.UserID)
			}
		})
	}
}

// A refused resolution injects NOTHING — not a partial identity. The user may
// be absent, suspended, or a token whose claimed owner does not match.
func TestServeSPA_ForwardedIdentityRefusedByResolverInjectsNoIdentity(t *testing.T) {
	t.Parallel()

	cfg := Config{
		ForwardedIdentityVerifier: peerVerifier{},
		Resolver: resolverFunc(func(
			_ context.Context, _ auth.User, _, _ string,
		) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{}, errors.New("permission denied")
		}),
		Emails: emailFunc(func(context.Context, int64) (string, error) {
			t.Error("no address may be read for a refused principal")
			return "leaked@example.com", nil
		}),
	}

	injected := serveForwarded(t, cfg, userHeaders("42"))

	if len(injected.Permissions) != 0 {
		t.Errorf("permissions = %v, want none", injected.Permissions)
	}
	if injected.UserID != nil || injected.UserEmail != "" || injected.UserName != "" {
		t.Errorf("injected identity = %+v, want none", injected)
	}
}

// A missing address is not a failed page load: the SPA's footer falls back to
// the generic word "Admin", and the permissions still arrive.
func TestServeSPA_ForwardedIdentitySurvivesAnEmailLookupFailure(t *testing.T) {
	t.Parallel()

	cfg := Config{
		ForwardedIdentityVerifier: peerVerifier{},
		Resolver: resolverFunc(func(
			_ context.Context, _ auth.User, _, _ string,
		) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 42, Permissions: []string{"admin.auth.users"}}, nil
		}),
		Emails: emailFunc(func(context.Context, int64) (string, error) {
			return "", errors.New("database unavailable")
		}),
	}

	injected := serveForwarded(t, cfg, userHeaders("42"))

	if len(injected.Permissions) != 1 || injected.Permissions[0] != "admin.auth.users" {
		t.Errorf("permissions = %v, want [admin.auth.users]", injected.Permissions)
	}
	if injected.UserName != "" {
		t.Errorf("user_name = %q, want empty so the SPA uses its own fallback", injected.UserName)
	}
}

// The cookie path must keep working for a deployment that authenticates
// through internal/api/v2/auth's OIDC handler instead of the auth edge.
func TestServeSPA_SessionCookieStillResolvesWhenNoHeadersArrive(t *testing.T) {
	t.Parallel()

	cfg := Config{
		ForwardedIdentityVerifier: peerVerifier{},
		Resolver: resolverFunc(func(
			_ context.Context, _ auth.User, _, _ string,
		) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 42, Permissions: []string{"admin.auth.users"}}, nil
		}),
	}

	injected := serveAdminIndex(t, cfg, validCookie(t))

	if len(injected.Permissions) != 1 || injected.Permissions[0] != "admin.auth.users" {
		t.Errorf("permissions = %v, want [admin.auth.users]", injected.Permissions)
	}
	if injected.UserEmail != "member@example.com" {
		t.Errorf("user_email = %q, want member@example.com", injected.UserEmail)
	}
}

// The injected page is per-operator: it carries their address and the exact
// list of administration-mode permissions they hold. A cache that stores it can
// replay one operator's console to another.
//
// This is NOT a hypothetical for this handler. Before the forwarded-identity
// source existed, the runtime deployment received a byte-identical payload on
// every load — empty permissions, empty name — so a cached copy leaked nothing.
// Personalising the response is what makes the missing directive matter.
func TestServeSPA_PersonalisedPageIsNotCacheable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "index.html"),
		[]byte("<html><body><!-- admin_ui_config --></body></html>"),
		0o600,
	); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	cfg := Config{
		StaticDir:                 dir,
		BasePath:                  "/admin/app",
		SecretKey:                 testSecret,
		ForwardedIdentityVerifier: peerVerifier{},
		Resolver: resolverFunc(func(
			_ context.Context, _ auth.User, _, _ string,
		) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 42, Permissions: []string{"admin.auth.users"}}, nil
		}),
		Emails: emailFunc(func(context.Context, int64) (string, error) {
			return "operator@example.com", nil
		}),
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/app", nil)
	for name, value := range userHeaders("42") {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	NewHandler(cfg).ServeSPA(rec, req)

	// Guard the premise: without a personalised body there is nothing to leak,
	// and this test would pass for the wrong reason.
	if !strings.Contains(rec.Body.String(), "operator@example.com") {
		t.Fatalf("the page carries no operator identity, so this test proves nothing:\n%s", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}
