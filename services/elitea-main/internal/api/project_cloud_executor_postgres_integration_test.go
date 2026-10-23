package api

// A project's own Send to cloud switch over the real router, the real gates
// and the real tables (ADR-0034 decision 11, owner decision 2026-10-19):
//
//	GET|PUT /api/v2/admin/project_cloud_executor/administration/{projectID}
//	GET|PUT /api/v2/elitea_core/project_cloud_executor/prompt_lib/{projectID}
//
// The admin pair is gated on the projects surface's central permissions,
// resolved from auth_core__user_role (no injection seam, hence Postgres); the
// project pair on the project's own permissions. The assertions read the
// tenant configuration row the policy reads, not only the status line.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const (
	cloudSwitchProjectID = 2
	cloudSwitchAdminID   = 9201 // administration `admin`: view and edit
	cloudSwitchViewerID  = 9202 // a custom administration role with view only
	cloudSwitchPlainID   = 9203 // no central role
)

func seedCloudSwitchCallers(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO public.auth_core__user (id, email, name)
VALUES (9201, 'ce-admin@autotest.local', 'Admin'), (9202, 'ce-viewer@autotest.local', 'Viewer'),
       (9203, 'ce-plain@autotest.local', 'Plain') ON CONFLICT (id) DO NOTHING`,
		`INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT 9201, role.id FROM public.auth_core__role AS role WHERE role.mode = 'administration' AND role.name = 'admin'
ON CONFLICT (user_id, role_id) DO NOTHING`,
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('ce_projects_viewer', 'administration')`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT id, 'projects.projects.projects.view' FROM public.auth_core__role
WHERE name = 'ce_projects_viewer' AND mode = 'administration'`,
		`INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT 9202, id FROM public.auth_core__role WHERE name = 'ce_projects_viewer' AND mode = 'administration'`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the callers: %v\n%s", err, statement)
		}
	}
}

func newCloudSwitchRouter(pool *pgxpool.Pool, userID int, projectPermissions ...string) http.Handler {
	id := fmt.Sprintf("%d", userID)
	return NewRouter(RouterConfig{
		Pool:       pool,
		SkillsRepo: struct{ v2skills.Repository }{},
		AuthValidator: testTokenValidator{user: auth.User{
			ID: id, UserID: id, Email: "ce-" + id + "@autotest.local",
		}},
		PrincipalValidator: testPrincipalValidator{},
		// The project gates resolve through this; it grants only in project 2.
		ProjectPermissionResolver: fakePermissionResolver{granted: projectPermissions, forProject: "2"},
	})
}

type cloudSwitchState struct {
	ProjectID             int64  `json:"project_id"`
	PlatformEnabled       bool   `json:"platform_enabled"`
	ProjectDefaultEnabled bool   `json:"project_default_enabled"`
	Explicit              *bool  `json:"explicit"`
	Source                string `json:"source"`
	Enabled               bool   `json:"enabled"`
}

func cloudSwitchCall(t *testing.T, router http.Handler, method, path string, body any) (int, cloudSwitchState) {
	t.Helper()
	recorder := serveMemberWrite(t, router, method, path, body)
	var state cloudSwitchState
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, recorder.Body.String(), err)
		}
	}
	return recorder.Code, state
}

func cloudSwitchRow(t *testing.T, pool *pgxpool.Pool) map[string]any {
	t.Helper()
	var raw []byte
	err := pool.QueryRow(context.Background(),
		`SELECT data FROM p_2.configuration WHERE type = 'cloud_executor' ORDER BY id LIMIT 1`).Scan(&raw)
	if err != nil {
		t.Fatalf("read the project row: %v", err)
	}
	data := map[string]any{}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

// cloudSwitchLastActor is the actor of the project row's latest lifecycle
// outbox event.
func cloudSwitchLastActor(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var actor int64
	if err := pool.QueryRow(context.Background(), `
SELECT outbox.actor_id FROM elitea_runtime.configuration_lifecycle_outbox AS outbox
JOIN p_2.configuration AS row ON row.uuid = outbox.configuration_uuid
WHERE row.type = 'cloud_executor' ORDER BY outbox.revision DESC LIMIT 1`).Scan(&actor); err != nil {
		t.Fatalf("read the lifecycle outbox: %v", err)
	}
	return actor
}

func setCloudExecutorPlatform(t *testing.T, pool *pgxpool.Pool, key, value string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO centry.platform_config (section, key, value, updated_at)
VALUES ('cloud_executor', $1, $2::jsonb, now() AT TIME ZONE 'UTC')
ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value`, key, value); err != nil {
		t.Fatalf("set cloud_executor.%s: %v", key, err)
	}
}

func TestAdminProjectCloudExecutorSwitch(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	seedCloudSwitchCallers(t, pool)
	path := fmt.Sprintf("/api/v2/admin/project_cloud_executor/administration/%d", cloudSwitchProjectID)

	t.Run("refusals", func(t *testing.T) {
		plain := newCloudSwitchRouter(pool, cloudSwitchPlainID)
		if code, _ := cloudSwitchCall(t, plain, http.MethodGet, path, nil); code != http.StatusForbidden {
			t.Fatalf("no central role: GET = %d, want 403", code)
		}
		if code, _ := cloudSwitchCall(t, plain, http.MethodPut, path, map[string]any{"enabled": false}); code != http.StatusForbidden {
			t.Fatalf("no central role: PUT = %d, want 403", code)
		}
		viewer := newCloudSwitchRouter(pool, cloudSwitchViewerID)
		if code, _ := cloudSwitchCall(t, viewer, http.MethodGet, path, nil); code != http.StatusOK {
			t.Fatalf("projects view: GET = %d, want 200", code)
		}
		if code, _ := cloudSwitchCall(t, viewer, http.MethodPut, path, map[string]any{"enabled": false}); code != http.StatusForbidden {
			t.Fatalf("projects view only: PUT = %d, want 403", code)
		}
		// Project permissions do not open the admin route.
		member := newCloudSwitchRouter(pool, cloudSwitchPlainID, "models.project_settings.edit", "models.project_context.view")
		if code, _ := cloudSwitchCall(t, member, http.MethodPut, path, map[string]any{"enabled": false}); code != http.StatusForbidden {
			t.Fatalf("a project admin on the admin route: PUT = %d, want 403", code)
		}
	})

	admin := newCloudSwitchRouter(pool, cloudSwitchAdminID)
	t.Run("nothing saved follows the default under a platform that is off", func(t *testing.T) {
		code, state := cloudSwitchCall(t, admin, http.MethodGet, path, nil)
		if code != http.StatusOK || state.ProjectID != cloudSwitchProjectID || state.PlatformEnabled ||
			state.ProjectDefaultEnabled || state.Explicit != nil || state.Source != "default" || state.Enabled {
			t.Fatalf("GET = %d %+v", code, state)
		}
	})
	t.Run("on is refused while the platform is off; off is not", func(t *testing.T) {
		if code, _ := cloudSwitchCall(t, admin, http.MethodPut, path, map[string]any{"enabled": true}); code != http.StatusConflict {
			t.Fatalf("PUT on under the platform off = %d, want 409", code)
		}
		code, state := cloudSwitchCall(t, admin, http.MethodPut, path, map[string]any{"enabled": false})
		if code != http.StatusOK || state.Explicit == nil || *state.Explicit || state.Source != "explicit" {
			t.Fatalf("PUT off = %d %+v", code, state)
		}
	})
	t.Run("the default decides an unset project, an explicit value wins", func(t *testing.T) {
		setCloudExecutorPlatform(t, pool, "enabled", "true")
		setCloudExecutorPlatform(t, pool, "project_default_enabled", "true")
		// Explicit off over a default that is on.
		if code, state := cloudSwitchCall(t, admin, http.MethodGet, path, nil); code != http.StatusOK ||
			!state.PlatformEnabled || !state.ProjectDefaultEnabled || state.Enabled || state.Source != "explicit" {
			t.Fatalf("explicit off, default on = %d %+v", code, state)
		}
		// Back to the default: the key leaves the row, the other fields stay.
		if _, err := pool.Exec(context.Background(),
			`UPDATE p_2.configuration SET data = data || '{"shell": false}'::jsonb WHERE type = 'cloud_executor'`); err != nil {
			t.Fatal(err)
		}
		code, state := cloudSwitchCall(t, admin, http.MethodPut, path, map[string]any{"enabled": nil})
		if code != http.StatusOK || state.Explicit != nil || state.Source != "default" || !state.Enabled {
			t.Fatalf("PUT null = %d %+v", code, state)
		}
		row := cloudSwitchRow(t, pool)
		if _, has := row["enabled"]; has || row["shell"] != false {
			t.Fatalf("row after null = %v, want shell kept and no enabled", row)
		}
		// Explicit on over a default that is off.
		setCloudExecutorPlatform(t, pool, "project_default_enabled", "false")
		code, state = cloudSwitchCall(t, admin, http.MethodPut, path, map[string]any{"enabled": true})
		if code != http.StatusOK || state.Explicit == nil || !*state.Explicit || !state.Enabled {
			t.Fatalf("PUT on = %d %+v", code, state)
		}
		if row := cloudSwitchRow(t, pool); row["enabled"] != true || row["shell"] != false {
			t.Fatalf("row after on = %v", row)
		}
		// The central resolver resolves the admin himself: he is the actor.
		if actor := cloudSwitchLastActor(t, pool); actor != cloudSwitchAdminID {
			t.Fatalf("lifecycle actor = %d, want %d", actor, cloudSwitchAdminID)
		}
		// The platform's off wins over the stored on, which stays stored.
		setCloudExecutorPlatform(t, pool, "enabled", "false")
		if code, state := cloudSwitchCall(t, admin, http.MethodGet, path, nil); code != http.StatusOK ||
			state.Enabled || state.Explicit == nil || !*state.Explicit {
			t.Fatalf("platform off over explicit on = %d %+v", code, state)
		}
		var rows int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM p_2.configuration WHERE type = 'cloud_executor'`).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("cloud_executor rows = %d (%v), want one", rows, err)
		}
	})
	t.Run("bad requests", func(t *testing.T) {
		if code, _ := cloudSwitchCall(t, admin, http.MethodGet,
			"/api/v2/admin/project_cloud_executor/administration/999", nil); code != http.StatusNotFound {
			t.Fatalf("unknown project = %d, want 404", code)
		}
		for _, body := range []any{map[string]any{}, map[string]any{"enabled": "yes"},
			map[string]any{"enabled": true, "shell": false}} {
			if code, _ := cloudSwitchCall(t, admin, http.MethodPut, path, body); code != http.StatusBadRequest {
				t.Fatalf("PUT %v = %d, want 400", body, code)
			}
		}
	})
}

func TestProjectCloudExecutorSwitch(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	seedCloudSwitchCallers(t, pool)
	setCloudExecutorPlatform(t, pool, "enabled", "true")
	path := fmt.Sprintf("/api/v2/elitea_core/project_cloud_executor/prompt_lib/%d", cloudSwitchProjectID)

	// The upgrade rule: with the platform on and Default for Projects never
	// saved, a project that set nothing keeps Send to cloud on.
	viewer := newCloudSwitchRouter(pool, cloudSwitchPlainID, viewerPermissions...)
	if code, state := cloudSwitchCall(t, viewer, http.MethodGet, path, nil); code != http.StatusOK || !state.Enabled ||
		!state.ProjectDefaultEnabled || state.Source != "default" {
		t.Fatalf("default never saved: GET = %d %+v; want on", code, state)
	}
	// Once saved, the saved default wins.
	setCloudExecutorPlatform(t, pool, "project_default_enabled", "false")

	// A viewer or an editor reads it and cannot write it.
	for _, granted := range [][]string{viewerPermissions, editorPermissions} {
		router := newCloudSwitchRouter(pool, cloudSwitchPlainID, granted...)
		if code, state := cloudSwitchCall(t, router, http.MethodGet, path, nil); code != http.StatusOK || state.Enabled ||
			state.Source != "default" || !state.PlatformEnabled {
			t.Fatalf("%v: GET = %d %+v", granted, code, state)
		}
		if code, _ := cloudSwitchCall(t, router, http.MethodPut, path, map[string]any{"enabled": true}); code != http.StatusForbidden {
			t.Fatalf("%v: PUT = %d, want 403", granted, code)
		}
	}
	// A central admin without a project role cannot use the project route.
	if code, _ := cloudSwitchCall(t, newCloudSwitchRouter(pool, cloudSwitchAdminID), http.MethodPut, path,
		map[string]any{"enabled": true}); code != http.StatusForbidden {
		t.Fatalf("central admin on the project route: PUT = %d, want 403", code)
	}
	projectAdmin := newCloudSwitchRouter(pool, cloudSwitchPlainID, "models.project_context.view", "models.project_settings.edit")
	code, state := cloudSwitchCall(t, projectAdmin, http.MethodPut, path, map[string]any{"enabled": true})
	if code != http.StatusOK || !state.Enabled || state.Source != "explicit" {
		t.Fatalf("project admin PUT on = %d %+v", code, state)
	}
	if row := cloudSwitchRow(t, pool); row["enabled"] != true {
		t.Fatalf("row = %v", row)
	}
	// The write is a configuration mutation: its lifecycle event names the
	// caller as the actor, the user the permission gate resolved (the fake
	// project resolver resolves every caller to user 1).
	if actor := cloudSwitchLastActor(t, pool); actor != 1 {
		t.Fatalf("lifecycle actor = %d, want the resolved user 1", actor)
	}
	// Another project: the same caller holds nothing there.
	if code, _ := cloudSwitchCall(t, projectAdmin, http.MethodPut,
		"/api/v2/elitea_core/project_cloud_executor/prompt_lib/1", map[string]any{"enabled": true}); code != http.StatusForbidden {
		t.Fatalf("another project: PUT = %d, want 403", code)
	}
}
