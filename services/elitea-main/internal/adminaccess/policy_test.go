package adminaccess

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// adminSet is what the administration `admin` role holds: everything except
// the super_admin permission.
var adminSet = SetOf("admin.auth.users", "configuration.roles.permissions.edit", "runtime.plugins")

func TestAnAdminCannotAddTheSuperAdminPermissionToTheAdminRole(t *testing.T) {
	refusal := CheckCentralEdit(CentralEdit{
		Caller:    adminSet,
		HeldRoles: map[string]bool{"admin": true},
		Mode:      ModeAdministration,
		Granted:   []Cell{{Role: "admin", Permission: SuperAdminPermission}},
	})
	if refusal == nil || refusal.Code != CodeReservedPermission {
		t.Fatalf("refusal = %+v, want %s", refusal, CodeReservedPermission)
	}
}

func TestTheSuperAdminPermissionIsReservedEvenForASuperAdmin(t *testing.T) {
	for _, mode := range []string{ModeAdministration, ModeDeveloper} {
		refusal := CheckCentralEdit(CentralEdit{
			Caller:     SetOf(SuperAdminPermission),
			SuperAdmin: true,
			Mode:       mode,
			Granted:    []Cell{{Role: "editor", Permission: SuperAdminPermission}},
		})
		if refusal == nil || refusal.Code != CodeReservedPermission {
			t.Fatalf("mode %s: refusal = %+v, want %s", mode, refusal, CodeReservedPermission)
		}
	}
	// The administration super_admin role itself may hold it.
	if refusal := CheckCentralEdit(CentralEdit{
		SuperAdmin: true, Mode: ModeAdministration,
		Granted: []Cell{{Role: SuperAdminRole, Permission: SuperAdminPermission}},
	}); refusal != nil {
		t.Fatalf("granting it to super_admin was refused: %+v", refusal)
	}
	// A project role may not.
	if refusal := CheckReserved("", []Cell{{Role: SuperAdminRole, Permission: SuperAdminPermission}}); refusal == nil {
		t.Fatal("a project role named super_admin was given the reserved permission")
	}
}

func TestACallerCannotGrantAPermissionTheCallerDoesNotHold(t *testing.T) {
	refusal := CheckCentralEdit(CentralEdit{
		Caller:  SetOf("configuration.roles.permissions.edit"),
		Mode:    ModeAdministration,
		Granted: []Cell{{Role: "editor", Permission: "admin.auth.users"}, {Role: "editor", Permission: "configuration.roles.permissions.edit"}},
	})
	if refusal == nil || refusal.Code != CodePermissionNotHeld {
		t.Fatalf("refusal = %+v, want %s", refusal, CodePermissionNotHeld)
	}
	if !reflect.DeepEqual(refusal.Permissions, []string{"admin.auth.users"}) || refusal.Role != "editor" {
		t.Fatalf("refusal names %v on %q, want [admin.auth.users] on editor", refusal.Permissions, refusal.Role)
	}
}

// A revoke is bound by the subset rule too: a caller who could strip the
// reserved permission from super_admin would leave nobody able to grant it back.
func TestACallerCannotRevokeAPermissionTheCallerDoesNotHold(t *testing.T) {
	refusal := CheckCentralEdit(CentralEdit{
		Caller: adminSet, Mode: ModeAdministration, HeldRoles: map[string]bool{"admin": true},
		Revoked: []Cell{{Role: SuperAdminRole, Permission: SuperAdminPermission}},
	})
	if refusal == nil || refusal.Code != CodePermissionNotHeld || refusal.Role != SuperAdminRole {
		t.Fatalf("refusal = %+v, want %s on %s", refusal, CodePermissionNotHeld, SuperAdminRole)
	}
	if !reflect.DeepEqual(refusal.Permissions, []string{SuperAdminPermission}) {
		t.Fatalf("refusal names %v, want [%s]", refusal.Permissions, SuperAdminPermission)
	}
}

func TestACallerCannotEditARoleTheCallerHolds(t *testing.T) {
	for name, edit := range map[string]CentralEdit{
		"grant a held permission": {Granted: []Cell{{Role: "admin", Permission: "runtime.plugins"}}},
		"revoke":                  {Revoked: []Cell{{Role: "admin", Permission: "runtime.plugins"}}},
	} {
		edit.Caller, edit.Mode, edit.HeldRoles = adminSet, ModeAdministration, map[string]bool{"admin": true}
		refusal := CheckCentralEdit(edit)
		if refusal == nil || refusal.Code != CodeOwnRoleLocked || refusal.Role != "admin" {
			t.Fatalf("%s: refusal = %+v, want %s on admin", name, refusal, CodeOwnRoleLocked)
		}
	}
}

func TestLegitimateMatrixEditsPass(t *testing.T) {
	// An admin grants a permission it holds to a role it does not hold, and
	// revokes one from that role.
	if refusal := CheckCentralEdit(CentralEdit{
		Caller: adminSet, Mode: ModeAdministration, HeldRoles: map[string]bool{"admin": true},
		Granted: []Cell{{Role: "editor", Permission: "runtime.plugins"}},
		Revoked: []Cell{{Role: "viewer", Permission: "admin.auth.users"}},
	}); refusal != nil {
		t.Fatalf("legitimate edit refused: %+v", refusal)
	}
	// A super_admin edits its own role and grants what it does not hold.
	if refusal := CheckCentralEdit(CentralEdit{
		Caller: SetOf(SuperAdminPermission), SuperAdmin: true, Mode: ModeAdministration,
		HeldRoles: map[string]bool{SuperAdminRole: true},
		Granted:   []Cell{{Role: SuperAdminRole, Permission: "anything.new"}},
	}); refusal != nil {
		t.Fatalf("super_admin edit refused: %+v", refusal)
	}
}

func TestAssignmentNeedsEveryPermissionOfTheRole(t *testing.T) {
	refusal := CheckAssignment(SetOf("modes.users"), false, "admin", []string{"modes.users", "admin.auth.users"})
	if refusal == nil || refusal.Code != CodePermissionNotHeld || refusal.Role != "admin" {
		t.Fatalf("refusal = %+v", refusal)
	}
	if CheckAssignment(SetOf("a", "b"), false, "custom", []string{"a"}) != nil {
		t.Fatal("a subset assignment was refused")
	}
	if CheckAssignment(SetOf(), true, "admin", []string{"a"}) != nil {
		t.Fatal("a super_admin assignment was refused")
	}
}

func TestWidenedFindsAGainInOneProjectThatAnotherProjectHides(t *testing.T) {
	before := map[int64]Set{1: SetOf("x", "y"), 2: SetOf("y")}
	after := map[int64]Set{1: SetOf("x", "y"), 2: SetOf("x", "y")}
	gain, widened := Widened(before, after)
	if !widened || gain.ProjectID != 2 || !reflect.DeepEqual(gain.Permissions, []string{"x"}) {
		t.Fatalf("gain = %+v widened=%v, want project 2 gaining x", gain, widened)
	}
	// A new membership is a gain too.
	if _, widened := Widened(before, map[int64]Set{1: before[1], 2: before[2], 3: SetOf("z")}); !widened {
		t.Fatal("a new project membership was not measured")
	}
	// Losing permissions is not.
	if _, widened := Widened(before, map[int64]Set{1: SetOf("x")}); widened {
		t.Fatal("a revocation was measured as a gain")
	}
}

func TestWriteAnswers403WithTheCodeAndAnnotatesTheAuditRow(t *testing.T) {
	ctx, slot := audit.ContextWithAnnotationSlot(context.Background())
	request := httptest.NewRequest(http.MethodPut, "/api/v2/admin/permissions/administration/administration", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	Write(recorder, request, NotHeld("you can grant only permissions that you hold", "editor", []string{"admin.auth.users"}))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	// The headers apierr.WriteStatus sets on every other JSON error.
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != CodePermissionNotHeld || body["error"] == "" || body["role"] != "editor" {
		t.Fatalf("body = %v", body)
	}
	annotation, present := slot.Read()
	if !present || annotation.Action != RefusalAuditAction || annotation.EntityType != "role" || annotation.EntityName != "editor" {
		t.Fatalf("annotation = %+v present=%v", annotation, present)
	}
}

type stubResolver struct {
	resolution auth.PermissionResolution
	err        error
}

func (s stubResolver) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return s.resolution, s.err
}

func TestResolveCallerFailsClosed(t *testing.T) {
	request := httptest.NewRequest(http.MethodPut, "/", nil)
	if _, refusal := ResolveCaller(request, nil); refusal == nil || refusal.Code != CodeCallerUnresolved {
		t.Fatalf("no resolver: refusal = %+v", refusal)
	}
	resolver := stubResolver{resolution: auth.PermissionResolution{UserID: 7, Permissions: []string{SuperAdminPermission}}}
	if _, refusal := ResolveCaller(request, resolver); refusal == nil {
		t.Fatal("no principal was not refused")
	}
	withUser := request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "7", UserID: "7"}))
	if _, refusal := ResolveCaller(withUser, stubResolver{err: auth.ErrPermissionDenied}); refusal == nil {
		t.Fatal("a failed read was not refused")
	}
	caller, refusal := ResolveCaller(withUser, resolver)
	if refusal != nil || caller.UserID != 7 || !caller.SuperAdmin {
		t.Fatalf("caller = %+v refusal = %+v", caller, refusal)
	}
}
