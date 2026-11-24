package legacyrbac

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// stringRows is a pgx.Rows over string or *string columns. The embedded
// interface is nil: a method the resolver does not call panics.
type stringRows struct {
	pgx.Rows
	rows [][]*string
	at   int
}

func (r *stringRows) Close()     {}
func (r *stringRows) Err() error { return nil }
func (r *stringRows) Next() bool {
	r.at++
	return r.at <= len(r.rows)
}

func (r *stringRows) Scan(dest ...any) error {
	row := r.rows[r.at-1]
	if len(dest) != len(row) {
		return errors.New("unexpected scan")
	}
	for i, target := range dest {
		switch target := target.(type) {
		case *string:
			*target = *row[i]
		case **string:
			*target = row[i]
		default:
			return errors.New("unexpected scan target")
		}
	}
	return nil
}

func text(value string) *string { return &value }

// elevatedStore knows one active user who holds the role admin with one
// grant, and an active super_admin-level elevation of that user, bound to
// session 41, that grants two more.
type elevatedStore struct{ statements []string }

func (s *elevatedStore) Query(_ context.Context, statement string, args ...any) (pgx.Rows, error) {
	s.statements = append(s.statements, statement)
	switch {
	case strings.Contains(statement, "admin_elevations"):
		if args[0] != int64(1) || args[1] != int64(41) {
			return &stringRows{}, nil
		}
		return &stringRows{rows: [][]*string{
			{text("0b7c2d4e-0000-4000-8000-000000000001"), text("admin.auth.users")},
			{text("0b7c2d4e-0000-4000-8000-000000000001"), text("admin.auth.users.super_admin")},
		}}, nil
	case strings.Contains(statement, "auth_core__user_role"):
		return &stringRows{rows: [][]*string{{text("admin"), text("admin.auth.users")}}}, nil
	}
	return nil, errors.New("unexpected query")
}

func (s *elevatedStore) QueryRow(context.Context, string, ...any) pgx.Row {
	return activeUserRow{id: 1}
}

// The standing-set call returns the same set with and without an active
// elevation: it never reads the ledger, while ResolvePermissions on the same
// session unions the elevation in and says which part came from it.
func TestTheStandingSetCallNeverReadsTheElevation(t *testing.T) {
	session := auth.User{ID: "1", UserID: "1", CredentialKind: auth.CredentialKindSession, BrowserSessionRef: 41}

	store := &elevatedStore{}
	resolver := &PostgresResolver{store: store}
	standing, err := resolver.ResolveStandingAdministration(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range store.statements {
		if strings.Contains(statement, "admin_elevations") {
			t.Fatalf("the standing-set call read the elevation ledger: %s", statement)
		}
	}
	if !slices.Equal(standing.Permissions, []string{"admin.auth.users"}) || standing.Elevated() {
		t.Fatalf("the standing set under an elevation is %+v, want the admin grant alone", standing)
	}

	full, err := resolver.ResolvePermissions(context.Background(), session, auth.PermissionModeAdministration, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(full.Permissions, []string{"admin.auth.users", "admin.auth.users.super_admin"}) {
		t.Fatalf("the bound session resolves %v", full.Permissions)
	}
	if !slices.Equal(full.ElevatedPermissions, []string{"admin.auth.users.super_admin"}) {
		t.Fatalf("the elevated part is %v, want the super_admin permission alone", full.ElevatedPermissions)
	}
	if !slices.Equal(full.StandingPermissions(), standing.Permissions) || !slices.Equal(full.Roles, standing.Roles) {
		t.Fatalf("the standing part of the full resolution %v (roles %v) is not the standing call's %v (roles %v)",
			full.StandingPermissions(), full.Roles, standing.Permissions, standing.Roles)
	}

	// Without the elevation (another session) the two calls agree.
	other := session
	other.BrowserSessionRef = 42
	withoutElevation, err := resolver.ResolvePermissions(context.Background(), other, auth.PermissionModeAdministration, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(withoutElevation.Permissions, standing.Permissions) || withoutElevation.Elevated() {
		t.Fatalf("a session without an elevation resolves %+v, want the standing set %v", withoutElevation, standing.Permissions)
	}
}

// The kinds that never hold an elevation never read the ledger, whatever
// reference they carry.
func TestOnlyABrowserSessionReadsTheLedger(t *testing.T) {
	for _, kind := range []auth.CredentialKind{auth.CredentialKindToken, auth.CredentialKindNative, auth.CredentialKindRun, auth.CredentialKindSystem} {
		store := &elevatedStore{}
		resolver := &PostgresResolver{store: &tokenAccessStore{elevatedStore: store}}
		principal := auth.User{ID: "1", UserID: "1", TokenID: "9", AuthType: "token", CredentialKind: kind, BrowserSessionRef: 41}
		resolution, err := resolver.ResolvePermissions(context.Background(), principal, auth.PermissionModeAdministration, "")
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if resolution.Elevated() || slices.Contains(resolution.Permissions, "admin.auth.users.super_admin") {
			t.Fatalf("%s resolves an elevation: %+v", kind, resolution)
		}
		for _, statement := range store.statements {
			if strings.Contains(statement, "admin_elevations") {
				t.Fatalf("%s read the elevation ledger", kind)
			}
		}
	}
}

// tokenAccessStore answers the token owner and an inherit token row.
type tokenAccessStore struct{ *elevatedStore }

func (s *tokenAccessStore) QueryRow(_ context.Context, statement string, _ ...any) pgx.Row {
	if strings.Contains(statement, "admin_access") {
		return tokenRow{}
	}
	return activeUserRow{id: 1}
}

type tokenRow struct{}

func (tokenRow) Scan(dest ...any) error {
	if len(dest) != 2 {
		return errors.New("unexpected scan")
	}
	*(dest[0].(*string)) = TokenAdminAccessInherit
	*(dest[1].(*[]string)) = nil
	return nil
}

func TestUnionElevation(t *testing.T) {
	union, elevated := UnionElevation([]string{"a", "c"}, []string{"b", "c", "d"})
	if !slices.Equal(union, []string{"a", "b", "c", "d"}) || !slices.Equal(elevated, []string{"b", "d"}) {
		t.Fatalf("union %v elevated %v", union, elevated)
	}
	union, elevated = UnionElevation([]string{"a"}, nil)
	if !slices.Equal(union, []string{"a"}) || elevated != nil {
		t.Fatalf("an empty grant: union %v elevated %v", union, elevated)
	}
	union, _ = UnionElevation(nil, nil)
	if union == nil {
		t.Fatal("the union is nil")
	}
}
