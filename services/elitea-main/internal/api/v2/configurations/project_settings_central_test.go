package configurations_test

// The central branch of the guarded project settings (ADR-0059 R0.6b, issue
// 2585). The caller holds the built-in project administrator set in the
// administration mode and nothing in the project: it is not a member.
//
// The refusals are decided at the route gate, so they need no database. The
// saves run on a migrated database (ELITEA_TEST_DATABASE_URL), so "passes the
// gate" is not mistaken for "works".

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// settingsCaller answers `central` in the administration mode and `member`
// in every project; an empty member set is a refusal, as for a non-member.
type settingsCaller struct {
	central, member []string
}

func (s settingsCaller) ResolvePermissions(
	_ context.Context, _ auth.User, mode string, _ string,
) (auth.PermissionResolution, error) {
	if mode == auth.PermissionModeAdministration {
		return auth.PermissionResolution{UserID: 1, Permissions: slices.Clone(s.central)}, nil
	}
	if len(s.member) == 0 {
		return auth.PermissionResolution{}, auth.ErrPermissionDenied
	}
	return auth.PermissionResolution{UserID: 1, Permissions: slices.Clone(s.member)}, nil
}

func projectAdministrator(t *testing.T) settingsCaller {
	t.Helper()
	role, ok := adminaccess.LookupBuiltInRole(adminaccess.RoleProjectAdministrator)
	if !ok {
		t.Fatal("project_administrator is not a built-in role")
	}
	return settingsCaller{central: role.Set()}
}

// settingsRouter mounts Routes() where router.go mounts it, for one caller,
// with the audit middleware's annotation slot on every request.
func settingsRouter(pool *pgxpool.Pool, caller settingsCaller) (chi.Router, func() audit.Annotation) {
	var slot *audit.AnnotationSlot
	h := handler.NewHandler(pool, handler.WithPermissionResolver(caller),
		handler.WithPublicProjectID(globalScopeProject))
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, recorded := audit.ContextWithAnnotationSlot(r.Context())
			slot = recorded
			// No numeric id on the session user: the gate sets the resolved
			// one, as the member branch does, so the row's author is the caller.
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(ctx, auth.User{ID: "session-1"})))
		})
	})
	router.Mount("/api/v2/configurations", h.Routes())
	return router, func() audit.Annotation {
		annotation, _ := slot.Read()
		return annotation
	}
}

func settingsDo(t *testing.T, router http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

var (
	settingsList   = fmt.Sprintf("/api/v2/configurations/configurations/%d", globalScopeProject)
	settingsRowFmt = "/api/v2/configurations/configuration/%d/%d"
)

func cloudExecutorBody(enabled bool) map[string]any {
	return map[string]any{
		"elitea_title": "cloud_executor", "label": "Cloud executor", "type": "cloud_executor",
		"section": "project_settings", "data": map[string]any{"enabled": enabled},
	}
}

/* ── refusals at the route gate (no database) ─────────────────────────── */

// The same caller is refused a credential, a model configuration and a
// toolkit configuration of the project, on the create and on a read of the
// list filtered to that type.
func TestTheProjectAdministratorCannotWriteACredential(t *testing.T) {
	expectSettingsRefused(t, "github", "credentials")
}

func TestTheProjectAdministratorCannotWriteAModelConfiguration(t *testing.T) {
	expectSettingsRefused(t, "llm_model", "llm")
}

func TestTheProjectAdministratorCannotWriteAToolkitConfiguration(t *testing.T) {
	expectSettingsRefused(t, "jira", "toolkits")
}

func expectSettingsRefused(t *testing.T, typeName, section string) {
	t.Helper()
	router, recorded := settingsRouter(closedPool(t), projectAdministrator(t))
	created := settingsDo(t, router, http.MethodPost, settingsList, map[string]any{
		"elitea_title": "x", "label": "x", "type": typeName, "section": section, "data": map[string]any{},
	})
	if created.Code != http.StatusForbidden {
		t.Fatalf("create %s = %d %s, want 403", typeName, created.Code, created.Body.String())
	}
	if recorded().CentralOverride {
		t.Fatalf("a refused %s create is marked central_override", typeName)
	}
	listed := settingsDo(t, router, http.MethodGet, settingsList+"?type="+typeName, nil)
	if listed.Code != http.StatusForbidden {
		t.Fatalf("list of %s = %d, want 403", typeName, listed.Code)
	}
	// A list that mixes a guarded type with this one is refused as a whole.
	mixed := settingsDo(t, router, http.MethodGet, settingsList+"?type=cloud_executor&type="+typeName, nil)
	if mixed.Code != http.StatusForbidden {
		t.Fatalf("list of cloud_executor and %s = %d, want 403", typeName, mixed.Code)
	}
}

// The gate reads the create body's `type` the way the handler does, as the
// exact key: a second key that differs only in case cannot pass a credential
// off as a guarded setting.
func TestACaseVariantTypeKeyDoesNotPassACredential(t *testing.T) {
	router, recorded := settingsRouter(closedPool(t), projectAdministrator(t))
	for _, body := range []string{
		`{"type":"github","TYPE":"browser","section":"credentials","data":{}}`,
		`{"TYPE":"browser","type":"github","section":"credentials","data":{}}`,
		`{"Type":"browser","section":"credentials","data":{}}`,
	} {
		request := httptest.NewRequest(http.MethodPost, settingsList, bytes.NewBufferString(body))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("create %s = %d, want 403", body, recorder.Code)
		}
		if recorded().CentralOverride {
			t.Errorf("create %s is marked central_override", body)
		}
	}
}

// A list with no type filter would carry every row of the project, so the
// override does not reach it; nor any route outside the allowlist.
func TestTheOverrideNeedsATypeAndAnAllowlistedRoute(t *testing.T) {
	router, _ := settingsRouter(closedPool(t), projectAdministrator(t))
	for _, path := range []string{
		settingsList,
		fmt.Sprintf("/api/v2/configurations/models/%d", globalScopeProject),
		fmt.Sprintf("/api/v2/configurations/types/%d", globalScopeProject),
	} {
		if got := settingsDo(t, router, http.MethodGet, path, nil).Code; got != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", path, got)
		}
	}
}

// A holder of projects.settings.view alone reads a guarded setting and is
// refused its write.
func TestSettingsViewReadsAndCannotWrite(t *testing.T) {
	router, _ := settingsRouter(closedPool(t), settingsCaller{central: []string{adminaccess.ProjectsSettingsView}})
	// Past the gate, the closed pool answers; the gate's own answer is 403.
	if got := settingsDo(t, router, http.MethodGet, settingsList+"?type=cloud_executor", nil).Code; got == http.StatusForbidden {
		t.Fatalf("a projects.settings.view holder is refused the guarded list")
	}
	if got := settingsDo(t, router, http.MethodPost, settingsList, cloudExecutorBody(true)).Code; got != http.StatusForbidden {
		t.Fatalf("a projects.settings.view holder created a guarded setting: %d", got)
	}
}

// A caller without either name is refused the guarded setting too.
func TestNoCentralNameNoOverride(t *testing.T) {
	router, _ := settingsRouter(closedPool(t), settingsCaller{central: []string{adminaccess.ProjectsMembersManage}})
	if got := settingsDo(t, router, http.MethodGet, settingsList+"?type=cloud_executor", nil).Code; got != http.StatusForbidden {
		t.Fatalf("GET = %d, want 403", got)
	}
	if got := settingsDo(t, router, http.MethodPost, settingsList, cloudExecutorBody(true)).Code; got != http.StatusForbidden {
		t.Fatalf("POST = %d, want 403", got)
	}
}

// The allowlist and the guard name the same five settings.
func TestTheAllowlistNamesTheGuardedSettings(t *testing.T) {
	got := adminaccess.GuardedSettingTypes()
	want := handler.GuardedProjectSettingTypes()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the central override names %v, the project settings guard %v", got, want)
	}
}

/* ── saves (a migrated database) ───────────────────────────────────────── */

func createCloudExecutor(t *testing.T, router http.Handler) int {
	t.Helper()
	created := settingsDo(t, router, http.MethodPost, settingsList, cloudExecutorBody(false))
	if created.Code != http.StatusCreated && created.Code != http.StatusOK {
		t.Fatalf("create = %d %s, want success", created.Code, created.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	id, ok := body["id"].(float64)
	if !ok {
		t.Fatalf("the create answered no id: %s", created.Body.String())
	}
	return int(id)
}

// The project administrator changes a guarded setting of a project it is not
// a member of, and the audit record says it was a central override.
func TestTheProjectAdministratorChangesAGuardedSettingOfAnyProject(t *testing.T) {
	pool := newCreateEchoPool(t)
	router, recorded := settingsRouter(pool, projectAdministrator(t))

	id := createCloudExecutor(t, router)
	if !recorded().CentralOverride {
		t.Fatal("the central create has no central_override")
	}
	row := fmt.Sprintf(settingsRowFmt, globalScopeProject, id)
	updated := settingsDo(t, router, http.MethodPut, row, cloudExecutorBody(true))
	if updated.Code != http.StatusOK {
		t.Fatalf("update = %d %s, want 200", updated.Code, updated.Body.String())
	}
	if !recorded().CentralOverride {
		t.Fatal("the central update has no central_override")
	}
	var enabled bool
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT (data->>'enabled')::boolean FROM p_%d.configuration WHERE id = $1`, globalScopeProject), id).
		Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("the update did not land")
	}
	var author *int64
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT author_id FROM p_%d.configuration WHERE id = $1`, globalScopeProject), id).Scan(&author); err != nil {
		t.Fatal(err)
	}
	if author == nil || *author != 1 {
		t.Fatalf("the central create's author is %v, want the resolved user 1", author)
	}

	// An update that changes the guarded type into a credential type is
	// refused, and the row keeps its type.
	changed := settingsDo(t, router, http.MethodPut, row, map[string]any{
		"type": "github", "section": "credentials", "data": map[string]any{},
	})
	if changed.Code != http.StatusForbidden {
		t.Fatalf("a change into a credential type = %d %s, want 403", changed.Code, changed.Body.String())
	}
	var stored string
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT type FROM p_%d.configuration WHERE id = $1`, globalScopeProject), id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "cloud_executor" {
		t.Fatalf("the row's type is %q after the refusal", stored)
	}
}

// A holder of projects.settings.view alone reads the stored guarded setting
// and is refused its update and its delete.
func TestSettingsViewReadsTheStoredSettingAndCannotChangeIt(t *testing.T) {
	pool := newCreateEchoPool(t)
	manager, _ := settingsRouter(pool, settingsCaller{central: []string{adminaccess.ProjectsSettingsManage}})
	id := createCloudExecutor(t, manager)
	row := fmt.Sprintf(settingsRowFmt, globalScopeProject, id)

	viewer, _ := settingsRouter(pool, settingsCaller{central: []string{adminaccess.ProjectsSettingsView}})
	if read := settingsDo(t, viewer, http.MethodGet, row, nil); read.Code != http.StatusOK {
		t.Fatalf("read = %d %s, want 200", read.Code, read.Body.String())
	}
	if listed := settingsDo(t, viewer, http.MethodGet, settingsList+"?type=cloud_executor", nil); listed.Code != http.StatusOK ||
		!bytes.Contains(listed.Body.Bytes(), []byte(`"cloud_executor"`)) {
		t.Fatalf("list = %d %s, want 200 with the setting", listed.Code, listed.Body.String())
	}
	if got := settingsDo(t, viewer, http.MethodPut, row, cloudExecutorBody(true)).Code; got != http.StatusForbidden {
		t.Fatalf("update = %d, want 403", got)
	}
	if got := settingsDo(t, viewer, http.MethodDelete, row, nil).Code; got != http.StatusForbidden {
		t.Fatalf("delete = %d, want 403", got)
	}
}

// A member with models.project_settings.edit keeps the existing path and sets
// no central_override.
func TestAMemberWithTheProjectPermissionIsNoOverride(t *testing.T) {
	pool := newCreateEchoPool(t)
	member := settingsCaller{member: []string{
		handler.CurrentConfigurationCreatePermission, handler.CurrentConfigurationUpdatePermission,
		handler.ProjectSettingsEditPermission,
	}}
	router, recorded := settingsRouter(pool, member)
	createCloudExecutor(t, router)
	if recorded().CentralOverride {
		t.Fatal("a member's write is marked central_override")
	}
}
