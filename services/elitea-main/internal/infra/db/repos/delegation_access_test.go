package repos

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

type fakePermissions map[string]bool

func (f fakePermissions) HasProjectPermission(_ context.Context, _, _ int64, permission string) (bool, error) {
	return f[permission], nil
}

type fakeVisibility struct {
	hidden     []int64
	references []string
	err        error
	reads      int
}

func (f *fakeVisibility) HiddenToolkitsForUser(context.Context, int64, int64) ([]int64, error) {
	f.reads++
	return f.hidden, f.err
}

func (f *fakeVisibility) UsableCredentialReferencesInProject(context.Context, int64) ([]string, error) {
	f.reads++
	return f.references, f.err
}

func accessOf(t *testing.T, permissions fakePermissions, visibility *fakeVisibility, governed bool) interface {
	HoldsToolkit(string) bool
	HoldsCredential(string) bool
} {
	t.Helper()
	options := []DelegationAccessOption{}
	if visibility != nil {
		options = append(options, WithDelegationVisibility(visibility, func(context.Context) bool { return governed }))
	}
	resolver, err := NewDelegationAccess(permissions, options...)
	if err != nil {
		t.Fatal(err)
	}
	access, err := resolver.AccessFor(context.Background(), 7, 31)
	if err != nil {
		t.Fatal(err)
	}
	return access
}

func TestDelegationAccessIsPerToolkit(t *testing.T) {
	both := fakePermissions{DelegationToolkitPermission: true, credentialgrant.PermissionUse: true}
	access := accessOf(t, both, &fakeVisibility{hidden: []int64{8}}, false)
	if !access.HoldsToolkit("toolkit:7") {
		t.Error("an open toolkit must be held")
	}
	if access.HoldsToolkit("toolkit:8") {
		t.Error("a toolkit the project's folder access hides from the approver must not be held")
	}
	if !access.HoldsToolkit("builtin:calculator") {
		t.Error("a built-in toolkit holds no secret and is open to every approver")
	}

	// Without the toolkit permission nothing but a built-in is held, and the
	// hidden list is not even read.
	visibility := &fakeVisibility{}
	none := accessOf(t, fakePermissions{credentialgrant.PermissionUse: true}, visibility, false)
	if none.HoldsToolkit("toolkit:7") || !none.HoldsToolkit("builtin:calculator") {
		t.Error("no toolkit permission: only a built-in toolkit is held")
	}
	if visibility.reads != 0 {
		t.Errorf("a person without the permission cost %d reads", visibility.reads)
	}
}

func TestDelegationAccessIsPerCredential(t *testing.T) {
	both := fakePermissions{DelegationToolkitPermission: true, credentialgrant.PermissionUse: true}

	// Outside the governed profile the credential permission decides.
	open := accessOf(t, both, &fakeVisibility{references: []string{"ref-a"}}, false)
	if !open.HoldsCredential("ref-a") || !open.HoldsCredential("ref-b") {
		t.Error("ungoverned: the credential permission alone decides")
	}

	// Under it a reference no active use grant covers is not held, even with the
	// permission: nobody can stand behind a reference that can never be used.
	governed := accessOf(t, both, &fakeVisibility{references: []string{"ref-a"}}, true)
	if !governed.HoldsCredential("ref-a") {
		t.Error("governed: a covered reference must be held")
	}
	if governed.HoldsCredential("ref-b") {
		t.Error("governed: a reference with no use grant must not be held")
	}

	// And the permission is still required.
	denied := accessOf(t, fakePermissions{DelegationToolkitPermission: true}, &fakeVisibility{references: []string{"ref-a"}}, true)
	if denied.HoldsCredential("ref-a") {
		t.Error("no credential permission: nothing is held")
	}
}

func TestDelegationAccessWithoutVisibilityIsTheTwoPermissions(t *testing.T) {
	access := accessOf(t, fakePermissions{DelegationToolkitPermission: true, credentialgrant.PermissionUse: true}, nil, true)
	if !access.HoldsToolkit("toolkit:8") || !access.HoldsCredential("ref-z") {
		t.Error("with no visibility reader the permissions decide")
	}
}

func TestDelegationAccessRefusesWhatItCannotRead(t *testing.T) {
	boom := errors.New("down")
	resolver, err := NewDelegationAccess(fakePermissions{DelegationToolkitPermission: true},
		WithDelegationVisibility(&fakeVisibility{err: boom}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.AccessFor(context.Background(), 7, 31); !errors.Is(err, boom) {
		t.Fatalf("an unreadable folder access must not read as open: %v", err)
	}
}
