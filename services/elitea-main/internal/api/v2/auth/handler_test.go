package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	v2auth "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/auth"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type permissionResolverFunc func(
	context.Context,
	identity.User,
	string,
	string,
) (identity.PermissionResolution, error)

func (f permissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	principal identity.User,
	mode string,
	projectID string,
) (identity.PermissionResolution, error) {
	return f(ctx, principal, mode, projectID)
}

func TestPermissionListReturnsOnlyResolvedPermissions(t *testing.T) {
	resolver := permissionResolverFunc(func(
		_ context.Context,
		principal identity.User,
		mode string,
		projectID string,
	) (identity.PermissionResolution, error) {
		if principal.ID != "42" || mode != identity.PermissionModeDefault || projectID != "7" {
			t.Fatalf("unexpected resolver input: principal=%+v mode=%q project=%q", principal, mode, projectID)
		}
		return identity.PermissionResolution{
			UserID:      42,
			Permissions: []string{"configurations.configurations.list"},
		}, nil
	})
	h := v2auth.NewHandler(nil, v2auth.WithPermissionResolver(resolver))
	req := httptest.NewRequest(http.MethodGet, "/permissions/prompt_lib/7", nil)
	req = req.WithContext(identity.ContextWithUser(req.Context(), identity.User{ID: "42"}))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	// The wire shape is an array of Permission objects, not of bare strings —
	// v2.yaml's permissionList response is `array of #/components/schemas/
	// Permission` {name, enabled}. Decoding into []string silently passed while
	// the handler returned strings and started failing when it began returning
	// objects; assert the object shape so the two cannot drift apart again.
	var permissions []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&permissions); err != nil {
		t.Fatal(err)
	}
	if len(permissions) != 1 {
		t.Fatalf("permissions = %v, want exactly 1", permissions)
	}
	if permissions[0].Name != "configurations.configurations.list" {
		t.Fatalf("permissions[0].Name = %q", permissions[0].Name)
	}
	if !permissions[0].Enabled {
		t.Fatalf("permissions[0].Enabled = false, want true (a resolved permission is granted)")
	}
}

func TestPermissionListDoesNotEscalateMissingResolverOrFailure(t *testing.T) {
	for _, test := range []struct {
		name     string
		resolver identity.PermissionResolver
	}{
		{name: "missing resolver"},
		{
			name: "resolver failure",
			resolver: permissionResolverFunc(func(context.Context, identity.User, string, string) (identity.PermissionResolution, error) {
				return identity.PermissionResolution{}, errors.New("database unavailable")
			}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := v2auth.NewHandler(nil, v2auth.WithPermissionResolver(test.resolver))
			req := httptest.NewRequest(http.MethodGet, "/permissions/prompt_lib/7", nil)
			req = req.WithContext(identity.ContextWithUser(req.Context(), identity.User{ID: "42"}))
			rec := httptest.NewRecorder()
			h.Routes().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			var permissions []string
			if err := json.NewDecoder(rec.Body).Decode(&permissions); err != nil {
				t.Fatal(err)
			}
			if len(permissions) != 0 {
				t.Fatalf("permissions = %v, want empty", permissions)
			}
		})
	}
}
