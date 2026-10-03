package adminui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

// `permissions: []` is injected for three different situations. The SPA must
// not call a lapsed session or a database hiccup "access denied", so the
// handler states WHY in `access`.

func failingResolver(err error) auth.PermissionResolver {
	return resolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, err
	})
}

func TestServeSPA_AccessReasonCookiePath(t *testing.T) {
	t.Parallel()

	noRole := resolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 42, Permissions: []string{}}, nil
	})
	cases := map[string]struct {
		cfg    Config
		cookie string
		want   string
	}{
		"admin is granted":                             {cfg: Config{Resolver: adminResolver("admin.auth.users")}, cookie: validCookie(t), want: "granted"},
		"verified user without a role is denied":       {cfg: Config{Resolver: noRole}, cookie: validCookie(t), want: "denied"},
		"suspended user (permission denied) is denied": {cfg: Config{Resolver: failingResolver(auth.ErrPermissionDenied)}, cookie: validCookie(t), want: "denied"},
		"inactive principal is denied":                 {cfg: Config{Resolver: failingResolver(auth.ErrPrincipalInactive)}, cookie: validCookie(t), want: "denied"},
		"lookup error is unavailable":                  {cfg: Config{Resolver: failingResolver(errors.New("database unavailable"))}, cookie: validCookie(t), want: "unavailable"},
		"no resolver is unavailable":                   {cfg: Config{}, cookie: validCookie(t), want: "unavailable"},
		"no cookie is unauthenticated":                 {cfg: Config{Resolver: adminResolver("admin.auth.users")}, cookie: "", want: "unauthenticated"},
		"forged cookie is unauthenticated":             {cfg: Config{Resolver: adminResolver("admin.auth.users")}, cookie: "garbage.value", want: "unauthenticated"},
		"expired cookie is unauthenticated": {
			cfg: Config{Resolver: adminResolver("admin.auth.users")},
			cookie: signedSessionCookie(t, map[string]any{
				"uid": "42", "exp": float64(time.Now().Add(-time.Hour).Unix()),
			}),
			want: "unauthenticated",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			injected := serveAdminIndex(t, tc.cfg, tc.cookie)
			if injected.Access != tc.want {
				t.Fatalf("access = %q, want %q", injected.Access, tc.want)
			}
			if tc.want != "granted" && len(injected.Permissions) != 0 {
				t.Fatalf("permissions = %v, want none for access=%s", injected.Permissions, tc.want)
			}
		})
	}
}

func TestServeSPA_AccessReasonServerSideSession(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		sessions stubSessions
		want     string
	}{
		"live session, admin": {sessions: stubSessions{session: adminSession()}, want: "granted"},
		"session not found":   {sessions: stubSessions{err: browsersession.ErrNotFound}, want: "unauthenticated"},
		"session expired":     {sessions: stubSessions{err: browsersession.ErrExpired}, want: "unauthenticated"},
		"session idle":        {sessions: stubSessions{err: browsersession.ErrIdle}, want: "unauthenticated"},
		"session revoked":     {sessions: stubSessions{err: browsersession.ErrRevoked}, want: "unauthenticated"},
		"session store fault": {sessions: stubSessions{err: errors.New("pool timeout")}, want: "unavailable"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{Sessions: tc.sessions, Resolver: adminResolver("admin.auth.users")}
			injected := serveAdminIndex(t, cfg, serverSessionCookie(t))
			if injected.Access != tc.want {
				t.Fatalf("access = %q, want %q", injected.Access, tc.want)
			}
		})
	}
}

func TestServeSPA_AccessReasonForwardedIdentity(t *testing.T) {
	t.Parallel()

	// The forwarded path needs the resolver to report the owning user.
	adminResolver := func(permissions ...string) auth.PermissionResolver {
		return resolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 42, Permissions: permissions}, nil
		})
	}

	cases := map[string]struct {
		cfg     Config
		headers map[string]string
		want    string
	}{
		"admin": {
			cfg:     Config{ForwardedIdentityVerifier: peerVerifier{}, Resolver: adminResolver("admin.auth.users")},
			headers: userHeaders("42"), want: "granted",
		},
		"suspended": {
			cfg:     Config{ForwardedIdentityVerifier: peerVerifier{}, Resolver: failingResolver(auth.ErrPrincipalInactive)},
			headers: userHeaders("42"), want: "denied",
		},
		"resolver fault": {
			cfg:     Config{ForwardedIdentityVerifier: peerVerifier{}, Resolver: failingResolver(errors.New("boom"))},
			headers: userHeaders("42"), want: "unavailable",
		},
		"headers without peer proof": {
			cfg:     Config{ForwardedIdentityVerifier: peerVerifier{err: errors.New("not the ingress")}, Resolver: adminResolver("admin.auth.users")},
			headers: userHeaders("42"), want: "unauthenticated",
		},
		"no headers": {
			cfg:  Config{ForwardedIdentityVerifier: peerVerifier{}, Resolver: adminResolver("admin.auth.users")},
			want: "unauthenticated",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if injected := serveForwarded(t, tc.cfg, tc.headers); injected.Access != tc.want {
				t.Fatalf("access = %q, want %q", injected.Access, tc.want)
			}
		})
	}
}
