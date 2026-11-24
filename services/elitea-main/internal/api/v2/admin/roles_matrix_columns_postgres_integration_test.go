package admin_test

// The central matrix describes its columns (ADR-0059 R0.8c, issue 2586): the
// Users page lists the assignable roles from the read, so whether a role is
// built-in, and whether it is never assigned, comes from the seed record and
// not from its name.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

type matrixColumnBody struct {
	Name        string  `json:"name"`
	BuiltIn     bool    `json:"built_in"`
	Marker      *string `json:"marker"`
	DisplayName string  `json:"display_name"`
}

func centralMatrixColumns(t *testing.T, env escalationEnvironment) map[string]matrixColumnBody {
	t.Helper()
	recorder := adminDo(t, env.as(escalationSuper), http.MethodGet, centralMatrix, nil)
	expectStatus(t, recorder, http.StatusOK)
	var body struct {
		Roles []matrixColumnBody `json:"roles"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	columns := map[string]matrixColumnBody{}
	for _, column := range body.Roles {
		columns[column.Name] = column
	}
	return columns
}

func TestTheCentralMatrixDescribesEachRoleBySeedRecord(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltIn(t, env.pool, "llm_manager")
	// The fixture's system role, as a legacy-backed database records it:
	// built-in and never assigned.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := env.pool.Exec(ctx, `INSERT INTO elitea_auth.admin_role_seeds (role_id, name, mode, marker, seeded_by)
		SELECT id, name, mode, 'unassignable', 'test' FROM public.auth_core__role
		WHERE mode = 'administration' AND name = 'system'`); err != nil {
		t.Fatalf("seed system: %v", err)
	}

	columns := centralMatrixColumns(t, env)
	llm, ok := columns["llm_manager"]
	if !ok || !llm.BuiltIn || llm.Marker == nil || *llm.Marker != "declared" || llm.DisplayName != "LLM manager" {
		t.Errorf("llm_manager = %+v, want built-in, declared, \"LLM manager\"", llm)
	}
	system := columns["system"]
	if !system.BuiltIn || system.Marker == nil || *system.Marker != "unassignable" {
		t.Errorf("system = %+v, want built-in and unassignable", system)
	}
	// A custom role of the fixture: no seed row, no display name.
	custom, ok := columns["matrix_editor"]
	if !ok || custom.BuiltIn || custom.Marker != nil || custom.DisplayName != "" {
		t.Errorf("matrix_editor = %+v, want a custom role", custom)
	}
}

// A project matrix has no administration roles to describe.
func TestAProjectMatrixDescribesNoRoles(t *testing.T) {
	env := newEscalationEnvironment(t)
	recorder := adminDo(t, env.as(escalationSuper), http.MethodGet, "/m/admin/permissions/public/default", nil)
	expectStatus(t, recorder, http.StatusOK)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["roles"]; ok {
		t.Errorf("the project matrix carries roles: %s", recorder.Body.String())
	}
}

// Without the seed table nothing is built-in, and the read does not fail.
func TestWithoutTheSeedTableNoColumnIsBuiltIn(t *testing.T) {
	env := newEscalationEnvironment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := env.pool.Exec(ctx, `DROP TABLE elitea_auth.admin_role_seeds`); err != nil {
		t.Fatalf("drop the seed table: %v", err)
	}
	columns := centralMatrixColumns(t, env)
	if len(columns) == 0 {
		t.Fatal("no roles described")
	}
	for name, column := range columns {
		if column.BuiltIn || column.Marker != nil {
			t.Errorf("%s = %+v without a seed table", name, column)
		}
	}
}
