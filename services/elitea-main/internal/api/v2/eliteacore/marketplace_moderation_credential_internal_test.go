package eliteacore

import (
	"context"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

// moderationCredentialResolver is the production rule
// (legacyrbac.AdministrationSet) over an owner who holds `admin`.
type moderationCredentialResolver struct{}

func (moderationCredentialResolver) ResolvePermissions(
	_ context.Context, principal auth.User, _ string, _ string,
) (auth.PermissionResolution, error) {
	owner := []string{adminaccess.AdminAuthUsers, adminaccess.RuntimeAdminPublishedAgents, adminaccess.RuntimePlugins}
	return auth.PermissionResolution{
		UserID:      1,
		Permissions: legacyrbac.AdministrationSet(principal.CredentialKind, legacyrbac.TokenAdminAccess{}, owner),
	}, nil
}

// A native device session of an administrator keeps Marketplace moderation
// (ADR-0059 decision 7, "Native sessions"; R1.2a); a run token, a `none`
// token and a principal no authenticating site built do not.
func TestANativeSessionOfAnAdministratorKeepsMarketplaceModeration(t *testing.T) {
	handler := NewHandler(nil, WithPermissionResolver(moderationCredentialResolver{}))
	for _, tc := range []struct {
		user auth.User
		want bool
	}{
		{auth.User{ID: "1", UserID: "1", TokenID: "5", NativeClientID: "mobile", CredentialKind: auth.CredentialKindNative}, true},
		{auth.User{ID: "1", UserID: "1", CredentialKind: auth.CredentialKindForm}, true},
		{auth.User{ID: "1", UserID: "1", TokenID: "6", CredentialKind: auth.CredentialKindRun}, false},
		{auth.User{ID: "1", UserID: "1", TokenID: "7", CredentialKind: auth.CredentialKindToken}, false},
		{auth.User{ID: "1", UserID: "1"}, false},
	} {
		ctx := auth.ContextWithUser(context.Background(), tc.user)
		if got := handler.holdsModerationPermission(ctx); got != tc.want {
			t.Errorf("kind %q: moderation %v, want %v", tc.user.CredentialKind, got, tc.want)
		}
	}
}
