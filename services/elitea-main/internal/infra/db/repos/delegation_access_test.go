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
	hidden []int64
	uses   []ToolkitCredential
	err    error
	reads  int
}

func (f *fakeVisibility) HiddenToolkitsForUser(context.Context, int64, int64) ([]int64, error) {
	f.reads++
	return f.hidden, f.err
}

func (f *fakeVisibility) UsableCredentialsInProject(context.Context, int64) ([]ToolkitCredential, error) {
	f.reads++
	return f.uses, f.err
}

func accessOf(t *testing.T, permissions fakePermissions, visibility *fakeVisibility, governed bool) interface {
	HoldsToolkit(string) bool
	HoldsCredential(string, string) bool
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
	for _, key := range []string{"toolkit:", "toolkit:x", "mcp:12", "12", ""} {
		if access.HoldsToolkit(key) {
			t.Errorf("a key that is not a toolkit id (%q) must fail closed", key)
		}
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
	open := accessOf(t, both, &fakeVisibility{uses: []ToolkitCredential{{ToolkitID: 7, Reference: "ref-a"}}}, false)
	if !open.HoldsCredential("toolkit:7", "ref-a") || !open.HoldsCredential("toolkit:7", "ref-b") {
		t.Error("ungoverned: the credential permission alone decides")
	}

	// Under it a reference no active use grant covers is not held, even with the
	// permission: nobody can stand behind a reference that can never be used.
	governed := accessOf(t, both, &fakeVisibility{uses: []ToolkitCredential{{ToolkitID: 7, Reference: "ref-a"}, {ToolkitID: 8, Reference: "0192F5E2-7C3A"}}}, true)
	if !governed.HoldsCredential("toolkit:7", "ref-a") {
		t.Error("governed: a covered reference must be held")
	}
	if !governed.HoldsCredential("toolkit:8", "0192f5e2-7c3a") || !governed.HoldsCredential("toolkit:8", "0192F5E2-7C3A") {
		t.Error("governed: references compare in lower case, whatever case either side spells them")
	}
	if governed.HoldsCredential("toolkit:7", "ref-b") {
		t.Error("governed: a reference with no use grant must not be held")
	}
	// #2215: the grant that covers a reference for toolkit 7 does not cover it for toolkit 8, or for a malformed key.
	if governed.HoldsCredential("toolkit:8", "ref-a") || governed.HoldsCredential("toolkit:x", "ref-a") || governed.HoldsCredential("toolkit:", "ref-a") {
		t.Error("governed: a reference is held per toolkit, not project-wide")
	}
	if !governed.HoldsCredential("builtin:calculator", "ref-a") {
		t.Error("governed: a toolkit key with no id falls back to the project's covered references")
	}

	// And the permission is still required.
	denied := accessOf(t, fakePermissions{DelegationToolkitPermission: true}, &fakeVisibility{uses: []ToolkitCredential{{ToolkitID: 7, Reference: "ref-a"}}}, true)
	if denied.HoldsCredential("toolkit:7", "ref-a") {
		t.Error("no credential permission: nothing is held")
	}
}

func TestDelegationAccessWithoutVisibilityIsTheTwoPermissions(t *testing.T) {
	access := accessOf(t, fakePermissions{DelegationToolkitPermission: true, credentialgrant.PermissionUse: true}, nil, true)
	if !access.HoldsToolkit("toolkit:8") || !access.HoldsCredential("toolkit:8", "ref-z") {
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
