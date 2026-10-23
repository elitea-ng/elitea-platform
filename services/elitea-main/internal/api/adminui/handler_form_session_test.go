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

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// DEFECT: ServeSPA read the `elitea_session` cookie and nothing else.
//
// A Form-login browser does not carry that cookie. It signs in at /auth/login,
// which stores an opaque server-side session under `elitea_browser_auth`. The
// cookie lookup found nothing on every load, so the handler injected
// `permissions: []` and an empty `user_name`.
//
// The SPA shows a nav item only when the injected list carries one of its
// permissions, so the operator got a sidebar with no items at all — ten
// implemented admin pages reachable only by typing their URL — and a footer
// reading the generic fallback "Admin" instead of their address.
//
// The first fix read the session through the edge's X-Auth-* projection. That
// plane is gone (#1359): the handler now reads the Form session itself, through
// the same reader apimw.Auth uses.

type emailFunc func(context.Context, int64) (string, error)

func (f emailFunc) UserEmail(ctx context.Context, userID int64) (string, error) {
	return f(ctx, userID)
}

// serveFormSession drives the real ServeSPA. A non-empty signIn sends the Form
// session cookie authtest.FormSessions resolves to that user; there is never an
// `elitea_session` cookie.
func serveFormSession(t *testing.T, cfg Config, signIn string) adminUIConfig {
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
	if signIn != "" {
		authtest.SignIn(req, signIn)
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

func TestServeSPA_FormSessionGetsTheOperatorsPermissions(t *testing.T) {
	t.Parallel()

	granted := []string{"admin.auth.users", "models.admin.audit_trail.view"}
	var seenPrincipal auth.User
	var seenMode string
	cfg := Config{
		FormSessions: authtest.FormSessions{},
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

	injected := serveFormSession(t, cfg, "42")

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
	if seenPrincipal.UserID != "42" || seenPrincipal.AuthType != "session" {
		t.Errorf("resolver principal = %+v, want the Form session of user 42", seenPrincipal)
	}
}

// The X-Auth-* headers are no credential any more (#1359). A browser that
// sends them, with no Form cookie, names nobody — whatever reader is composed.
func TestServeSPA_ForgedXAuthHeadersGetNothing(t *testing.T) {
	t.Parallel()

	cfg := Config{
		FormSessions: authtest.FormSessions{},
		Resolver: resolverFunc(func(
			_ context.Context, _ auth.User, _, _ string,
		) (auth.PermissionResolution, error) {
			t.Error("the resolver must not run for X-Auth-* headers")
			return auth.PermissionResolution{UserID: 1, Permissions: []string{"admin.auth.users"}}, nil
		}),
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte("<html><body><!-- admin_ui_config --></body></html>"), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	cfg.StaticDir, cfg.SecretKey, cfg.BasePath = dir, testSecret, "/admin/app"

	req := httptest.NewRequest(http.MethodGet, "/admin/app", nil)
	req.Header.Set("X-Auth-Type", "user")
	req.Header.Set("X-Auth-ID", "1")
	req.Header.Set("X-Auth-User-ID", "1")
	rec := httptest.NewRecorder()
	NewHandler(cfg).ServeSPA(rec, req)

	if strings.Contains(rec.Body.String(), "admin.auth.users") {
		t.Fatalf("X-Auth-* headers earned permissions:\n%s", rec.Body.String())
	}
}

// A Form cookie the reader rejects (forged, expired, signed out), or a reader
// that is not composed, injects NOTHING.
func TestServeSPA_UnprovenFormSessionGetsNothing(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]Config{
		"no Form reader":        {},
		"rejected Form session": {FormSessions: authtest.FormSessions{Err: apimw.ErrFormSessionRejected}},
		"session store outage":  {FormSessions: authtest.FormSessions{Err: errors.New("store down")}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg.Resolver = resolverFunc(func(
				_ context.Context, _ auth.User, _, _ string,
			) (auth.PermissionResolution, error) {
				t.Error("the resolver must not run for an unproven Form session")
				return auth.PermissionResolution{UserID: 1, Permissions: []string{"admin.auth.users"}}, nil
			})

			injected := serveFormSession(t, cfg, "1")

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
// be absent or suspended.
func TestServeSPA_FormSessionRefusedByResolverInjectsNoIdentity(t *testing.T) {
	t.Parallel()

	cfg := Config{
		FormSessions: authtest.FormSessions{},
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

	injected := serveFormSession(t, cfg, "42")

	if len(injected.Permissions) != 0 {
		t.Errorf("permissions = %v, want none", injected.Permissions)
	}
	if injected.UserID != nil || injected.UserEmail != "" || injected.UserName != "" {
		t.Errorf("injected identity = %+v, want none", injected)
	}
}

// A missing address is not a failed page load: the SPA's footer falls back to
// the generic word "Admin", and the permissions still arrive.
func TestServeSPA_FormSessionSurvivesAnEmailLookupFailure(t *testing.T) {
	t.Parallel()

	cfg := Config{
		FormSessions: authtest.FormSessions{},
		Resolver: resolverFunc(func(
			_ context.Context, _ auth.User, _, _ string,
		) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 42, Permissions: []string{"admin.auth.users"}}, nil
		}),
		Emails: emailFunc(func(context.Context, int64) (string, error) {
			return "", errors.New("database unavailable")
		}),
	}

	injected := serveFormSession(t, cfg, "42")

	if len(injected.Permissions) != 1 || injected.Permissions[0] != "admin.auth.users" {
		t.Errorf("permissions = %v, want [admin.auth.users]", injected.Permissions)
	}
	if injected.UserName != "" {
		t.Errorf("user_name = %q, want empty so the SPA uses its own fallback", injected.UserName)
	}
}

// The cookie path must keep working for a deployment that authenticates
// through internal/api/v2/auth's OIDC handler instead of Form sign-in.
func TestServeSPA_SessionCookieStillResolvesWithoutAFormCookie(t *testing.T) {
	t.Parallel()

	cfg := Config{
		FormSessions: authtest.FormSessions{},
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
		StaticDir:    dir,
		BasePath:     "/admin/app",
		SecretKey:    testSecret,
		FormSessions: authtest.FormSessions{},
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
	authtest.SignIn(req, "42")
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
