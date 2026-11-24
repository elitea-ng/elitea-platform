package legacyrbac

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// activeUserRow answers requireUser with the user id, and nothing else.
type activeUserRow struct{ id int64 }

func (r activeUserRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("unexpected scan")
	}
	target, ok := dest[0].(*int64)
	if !ok {
		return errors.New("unexpected scan target")
	}
	*target = r.id
	return nil
}

// userOnlyStore knows one active user and fails every permission query, so a
// resolution that reaches the role tables fails the test.
type userOnlyStore struct{ queries int }

func (s *userOnlyStore) Query(context.Context, string, ...any) (pgx.Rows, error) {
	s.queries++
	return nil, errors.New("the administration set was read for a credential that holds none")
}

func (s *userOnlyStore) QueryRow(context.Context, string, ...any) pgx.Row {
	return activeUserRow{id: 1}
}

// The zero credential kind (a principal no authenticating site built) and the
// kinds that hold no standing set resolve the empty administration set without
// reading a role (ADR-0059 R1.2a; R1.8 pins the same rule from the handlers).
func TestKindsWithoutAStandingSetResolveNothing(t *testing.T) {
	for _, principal := range []auth.User{
		{ID: "1", UserID: "1"},
		{ID: "1", UserID: "1", AuthType: "session"},
		{ID: "1", UserID: "1", CredentialKind: auth.CredentialKindSealed, BrowserSessionRef: 4},
		// A browser kind that names a token row was not built by a browser
		// plane.
		{ID: "1", UserID: "1", CredentialKind: auth.CredentialKindSession},
	} {
		if principal.CredentialKind == auth.CredentialKindSession {
			principal.TokenID = "9"
			principal.AuthType = "token"
			// resolveUserID reads the token owner first; the fake answers it.
		}
		store := &userOnlyStore{}
		resolution, err := (&PostgresResolver{store: store}).ResolvePermissions(
			context.Background(), principal, auth.PermissionModeAdministration, "")
		if err != nil {
			t.Fatalf("%+v: %v", principal, err)
		}
		if resolution.UserID != 1 || len(resolution.Permissions) != 0 || resolution.Permissions == nil {
			t.Fatalf("%+v resolves %+v, want user 1 and an empty, non-nil set", principal, resolution)
		}
		if store.queries != 0 {
			t.Fatalf("%+v read the role tables %d times", principal, store.queries)
		}
		if principal.CredentialKind != auth.CredentialKindSealed && resolution.ElevationSessionRef != 0 {
			t.Fatalf("%+v exposes elevation ref %d", principal, resolution.ElevationSessionRef)
		}
	}
}

func TestAdministrationSetFollowsTheCredentialsTable(t *testing.T) {
	// No registry row is a token scope before R1.3a; this table stands one in.
	AllowTokenScopesForTest(t, adminaccess.AdminAuthUsers, "not.held")
	owner := []string{
		adminaccess.RuntimePlugins,
		adminaccess.AdminAuthUsers,
		adminaccess.RuntimeAdminPublishedAgents,
		adminaccess.AdminAuthUsers,
	}
	full := []string{adminaccess.AdminAuthUsers, adminaccess.RuntimeAdminPublishedAgents, adminaccess.RuntimePlugins}
	scoped := TokenAdminAccess{Access: TokenAdminAccessScoped, Scopes: []string{adminaccess.AdminAuthUsers, "not.held"}}
	for _, tc := range []struct {
		kind   auth.CredentialKind
		access TokenAdminAccess
		want   []string
	}{
		{auth.CredentialKindSession, TokenAdminAccess{}, full},
		{auth.CredentialKindForm, TokenAdminAccess{}, full},
		{auth.CredentialKindToken, TokenAdminAccess{Access: TokenAdminAccessNone}, []string{}},
		{auth.CredentialKindToken, TokenAdminAccess{Access: TokenAdminAccessInherit}, full},
		{auth.CredentialKindToken, scoped, []string{adminaccess.AdminAuthUsers}},
		{auth.CredentialKindToken, TokenAdminAccess{Access: "unknown"}, []string{}},
		{auth.CredentialKindNative, TokenAdminAccess{}, []string{adminaccess.RuntimeAdminPublishedAgents, adminaccess.RuntimePlugins}},
		// The token row's access never widens a kind that is not a token.
		{auth.CredentialKindNative, TokenAdminAccess{Access: TokenAdminAccessInherit},
			[]string{adminaccess.RuntimeAdminPublishedAgents, adminaccess.RuntimePlugins}},
		{auth.CredentialKindRun, TokenAdminAccess{Access: TokenAdminAccessInherit}, []string{}},
		{auth.CredentialKindSystem, TokenAdminAccess{Access: TokenAdminAccessInherit}, []string{}},
		{auth.CredentialKindSealed, TokenAdminAccess{}, []string{}},
		{auth.CredentialKindNone, TokenAdminAccess{Access: TokenAdminAccessInherit}, []string{}},
	} {
		got := AdministrationSet(tc.kind, tc.access, owner)
		if got == nil || !slices.Equal(got, tc.want) {
			t.Errorf("AdministrationSet(%q, %+v) = %v, want %v", tc.kind, tc.access, got, tc.want)
		}
	}
}

// A scope resolves only when its registry row admits it as a token scope.
// R1.3a opted the eligible rows in: super_admin, identity trust and the legacy
// names never resolve, whatever admin_scopes says.
func TestAScopeOutsideTheRegistrysTokenScopesNeverResolves(t *testing.T) {
	owner := []string{adminaccess.AdminAuthUsers, adminaccess.AdminAuthUsersSuperAdmin,
		adminaccess.IdentitySSOTrustManage, adminaccess.RuntimePlugins}
	scoped := TokenAdminAccess{Access: TokenAdminAccessScoped, Scopes: owner}
	if got := AdministrationSet(auth.CredentialKindToken, scoped, owner); !slices.Equal(got, []string{adminaccess.AdminAuthUsers}) {
		t.Fatalf("with today's registry a scoped token resolved %v, want only %s", got, adminaccess.AdminAuthUsers)
	}
	AllowTokenScopesForTest(t)
	if got := AdministrationSet(auth.CredentialKindToken, scoped, owner); len(got) != 0 {
		t.Fatalf("with no admitted scope a scoped token resolved %v, want nothing", got)
	}
	AllowTokenScopesForTest(t, adminaccess.AdminAuthUsers)
	if got := AdministrationSet(auth.CredentialKindToken, scoped, owner); !slices.Equal(got, []string{adminaccess.AdminAuthUsers}) {
		t.Fatalf("a scoped token resolved %v, want only the admitted %s", got, adminaccess.AdminAuthUsers)
	}
}
