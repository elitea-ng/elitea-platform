package adminaccess

// The rules ADR-0059 R0.8a adds to the #2555 policy (issue 2497). The #2555
// tests in policy_test.go stay as they were.

import (
	"fmt"
	"testing"
)

// Every seeded_only name of the registry is refused on every role, in every
// plane and for a global administrator, except the one grant #2555 kept.
func TestEverySeededOnlyNameIsReserved(t *testing.T) {
	var seeded []string
	for _, row := range Registry().Rows() {
		if row.Grantable == GrantableSeededOnly {
			seeded = append(seeded, row.Name)
		}
	}
	if len(seeded) != 6 {
		t.Fatalf("premise: %d seeded_only names, want the 6 of decision 3: %v", len(seeded), seeded)
	}
	for _, permission := range seeded {
		for _, mode := range []string{ModeAdministration, ModeDeveloper, ModeDefault, ""} {
			for _, role := range []string{"custom", "admin", SuperAdminRole} {
				if permission == SuperAdminPermission && mode == ModeAdministration && role == SuperAdminRole {
					continue
				}
				refusal := CheckCentralEdit(CentralEdit{
					Caller: SetOf(seeded...), SuperAdmin: true, Mode: mode,
					Granted: []Cell{{Role: role, Permission: permission}},
				})
				if refusal == nil || refusal.Code != CodeReservedPermission || refusal.Role != role ||
					len(refusal.Permissions) != 1 || refusal.Permissions[0] != permission {
					t.Errorf("%s on %s/%s: refusal = %+v, want %s", permission, mode, role, refusal, CodeReservedPermission)
				}
			}
		}
	}
	if CheckReserved(ModeAdministration, []Cell{{Role: "custom", Permission: ConfigurationRolesPermissionsEdit}}) != nil {
		t.Fatal("a permission that is not seeded_only was refused")
	}
}

func TestSelfAssignmentIsRefusedExceptForAGlobalAdministrator(t *testing.T) {
	refusal := CheckSelfAssignment(7, false, 7, "editor")
	if refusal == nil || refusal.Code != CodeSelfAssignment || refusal.Role != "editor" {
		t.Fatalf("refusal = %+v, want %s on editor", refusal, CodeSelfAssignment)
	}
	if CheckSelfAssignment(7, true, 7, "editor") != nil {
		t.Fatal("a global administrator was refused its own role")
	}
	if CheckSelfAssignment(7, false, 8, "editor") != nil {
		t.Fatal("an assignment to another user was refused")
	}
}

func TestLockedAndUnassignableRefusalsCarryTheLockCode(t *testing.T) {
	refusal := SetLocked([]string{"admin", "editor"})
	if refusal.Code != CodeSetLocked || refusal.Role != "admin" {
		t.Fatalf("refusal = %+v", refusal)
	}
	// Every touched built-in role is named, so the client can show them all.
	if body := refusal.Body(); body["error"] == "" || body["code"] != CodeSetLocked ||
		fmt.Sprint(body["roles"]) != "[admin editor]" {
		t.Fatalf("body = %v", body)
	}
	if _, listed := SetLocked([]string{"editor"}).Body()["roles"]; listed {
		t.Fatal("a refusal about one role lists roles")
	}
	if refusal := Unassignable(RoleSystem); refusal.Code != CodeSetLocked || refusal.Role != RoleSystem {
		t.Fatalf("refusal = %+v", refusal)
	}
}
