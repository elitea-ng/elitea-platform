package adminui

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// DEFECT: ServeSPA injected a fixed list of 37 admin permissions and
// roles ["super_admin"] for every request whose elitea_session cookie passed
// the HMAC and exp check. It ran no permission lookup at all.
//
// Evidence of the failure: a rank-and-file user resolves to NO
// administration-mode permission (the resolver reads roles with
// mode='administration' only), yet the admin console received the full set.
// The console then rendered every nav group and enabled the create, edit and
// delete controls on Users, Secrets, Roles, Projects, Schedules and
// Moderation. Each click returned 403 from the server. A suspended user with
// an unexpired cookie got the same page, because verifySession never reads
// the database.
//
// The permissions the handler injects must come from the resolver, and must
// be empty when the resolver refuses.

const testSecret = "test-secret-key"

// resolverFunc is an auth.PermissionResolver built from a function.
type resolverFunc func(context.Context, auth.User, string, string) (auth.PermissionResolution, error)

func (f resolverFunc) ResolvePermissions(
	ctx context.Context, principal auth.User, mode, projectID string,
) (auth.PermissionResolution, error) {
	return f(ctx, principal, mode, projectID)
}

// signedSessionCookie builds a cookie in the exact shape verifySession expects:
// base64url(payload) + "." + hex(HMAC-SHA256(payload)).
func signedSessionCookie(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(encoded))
	return encoded + "." + hex.EncodeToString(mac.Sum(nil))
}

var injectedConfig = regexp.MustCompile(`window\.admin_ui_config = (\{.*\});`)

// serveAdminIndex drives the real ServeSPA and returns the injected object.
func serveAdminIndex(t *testing.T, cfg Config, cookie string) adminUIConfig {
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
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "elitea_session", Value: cookie})
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

func validCookie(t *testing.T) string {
	return signedSessionCookie(t, map[string]any{
		"uid":   "42",
		"email": "member@example.com",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
	})
}

func TestServeSPA_UserWithNoAdminRoleGetsNoPermissions(t *testing.T) {
	t.Parallel()

	var seenPrincipal auth.User
	var seenMode, seenProject string
	cfg := Config{Resolver: resolverFunc(func(
		_ context.Context, principal auth.User, mode, projectID string,
	) (auth.PermissionResolution, error) {
		seenPrincipal, seenMode, seenProject = principal, mode, projectID
		// A rank-and-file user holds no administration-mode role.
		return auth.PermissionResolution{UserID: 42, Permissions: []string{}}, nil
	})}

	injected := serveAdminIndex(t, cfg, validCookie(t))

	if len(injected.Permissions) != 0 {
		t.Fatalf("permissions = %v, want none", injected.Permissions)
	}
	if len(injected.Roles) != 0 {
		t.Fatalf("roles = %v, want none", injected.Roles)
	}
	if seenPrincipal.UserID != "42" {
		t.Errorf("resolver saw user %q, want \"42\"", seenPrincipal.UserID)
	}
	if seenMode != auth.PermissionModeAdministration {
		t.Errorf("resolver saw mode %q, want administration", seenMode)
	}
	if seenProject != "" {
		t.Errorf("resolver saw project %q, want the empty string", seenProject)
	}
	// The `uid` claim must reach the page. The old code read `user_id`, a
	// claim the minting code never writes, so user_id was null in production.
	if injected.UserID != float64(42) {
		t.Errorf("user_id = %v (%T), want 42", injected.UserID, injected.UserID)
	}
}

func TestServeSPA_SuspendedOrRefusedUserGetsNoPermissions(t *testing.T) {
	t.Parallel()

	// The resolver refuses a suspended user with auth.ErrPermissionDenied.
	cfg := Config{Resolver: resolverFunc(func(
		context.Context, auth.User, string, string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, auth.ErrPermissionDenied
	})}

	if injected := serveAdminIndex(t, cfg, validCookie(t)); len(injected.Permissions) != 0 {
		t.Fatalf("permissions = %v, want none for a refused principal", injected.Permissions)
	}
}

func TestServeSPA_ResolverFailureGrantsNothing(t *testing.T) {
	t.Parallel()

	cfg := Config{Resolver: resolverFunc(func(
		context.Context, auth.User, string, string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, errors.New("database unavailable")
	})}

	if injected := serveAdminIndex(t, cfg, validCookie(t)); len(injected.Permissions) != 0 {
		t.Fatalf("permissions = %v, want none when the resolver fails", injected.Permissions)
	}
}

// A mis-wired composition root must degrade closed, not open.
func TestServeSPA_NilResolverGrantsNothing(t *testing.T) {
	t.Parallel()

	if injected := serveAdminIndex(t, Config{}, validCookie(t)); len(injected.Permissions) != 0 {
		t.Fatalf("permissions = %v, want none with no resolver configured", injected.Permissions)
	}
}

func TestServeSPA_AdminGetsTheResolvedPermissions(t *testing.T) {
	t.Parallel()

	granted := []string{"admin.auth.users", "configuration.roles.roles.delete"}
	cfg := Config{Resolver: resolverFunc(func(
		context.Context, auth.User, string, string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 42, Permissions: granted}, nil
	})}

	injected := serveAdminIndex(t, cfg, validCookie(t))
	if len(injected.Permissions) != len(granted) {
		t.Fatalf("permissions = %v, want %v", injected.Permissions, granted)
	}
	for i, want := range granted {
		if injected.Permissions[i] != want {
			t.Fatalf("permissions = %v, want %v", injected.Permissions, granted)
		}
	}
}

// No cookie means no identity, so the resolver must not run at all.
func TestServeSPA_AnonymousRequestResolvesNothing(t *testing.T) {
	t.Parallel()

	cfg := Config{Resolver: resolverFunc(func(
		context.Context, auth.User, string, string,
	) (auth.PermissionResolution, error) {
		t.Fatal("the resolver ran for a request with no session cookie")
		return auth.PermissionResolution{}, nil
	})}

	injected := serveAdminIndex(t, cfg, "")
	if len(injected.Permissions) != 0 {
		t.Fatalf("permissions = %v, want none", injected.Permissions)
	}
	if injected.UserID != nil {
		t.Fatalf("user_id = %v, want null", injected.UserID)
	}
}
