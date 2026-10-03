package api

// The personal-project exception of the project SETTINGS writes (#6789),
// composed end to end: the production router, the real legacyrbac resolver
// and the real OwnershipCheck over one pool.
//
// The router unit tests run with no pool, so NewOwnershipCheck(nil) answers
// false and the exception never runs there. A wiring fault (the wrong user id
// handed to the check, or another pool) would pass them. This test seeds three
// projects. The caller is an EDITOR in all three, so `models.project_context.edit`
// is all it holds there, and `models.project_settings.edit` is held nowhere:
//
//	own personal project      name project_user_<caller>, owner caller → admitted
//	another user's personal   name project_user_<other>,  owner other  → refused
//	squatted team project     name project_user_<caller>, owner other  → refused
//
// The third one is the case the owner_id half of the check exists for.

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const (
	settingsCaller        = 9201
	settingsOther         = 9202
	settingsOwnPersonal   = 9301
	settingsOtherPersonal = 9302
	settingsSquatted      = 9303
)

func seedPersonalSettingsFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), memberWriteDeadline)
	defer cancel()

	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO public.auth_core__user (id, email, name)
VALUES ($1, 'settings-caller@autotest.local', 'Caller'),
       ($2, 'settings-other@autotest.local', 'Other')
ON CONFLICT (id) DO NOTHING`, []any{settingsCaller, settingsOther}},
		{`INSERT INTO centry.project (id, name, owner_id, create_success)
VALUES ($1, 'project_user_' || $4::int::text, $4::int, true),
       ($2, 'project_user_' || $5::int::text, $5::int, true),
       ($3, 'project_user_' || $4::int::text, $5::int, true)
ON CONFLICT (id) DO NOTHING`, []any{settingsOwnPersonal, settingsOtherPersonal, settingsSquatted, settingsCaller, settingsOther}},
		{`INSERT INTO public.auth_core__project_role (project_id, name)
SELECT project_id, 'editor' FROM unnest($1::int[]) AS project_id
ON CONFLICT (project_id, name) DO NOTHING`, []any{[]int{settingsOwnPersonal, settingsOtherPersonal, settingsSquatted}}},
		{`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT role.project_id, $1, role.id
FROM public.auth_core__project_role AS role
WHERE role.project_id = ANY($2::int[]) AND role.name = 'editor'
ON CONFLICT (project_id, user_id, role_id) DO NOTHING`, []any{settingsCaller, []int{settingsOwnPersonal, settingsOtherPersonal, settingsSquatted}}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed the settings fixture: %v", err)
		}
	}
}

func newSettingsCallerRouter(pool *pgxpool.Pool) http.Handler {
	id := fmt.Sprintf("%d", settingsCaller)
	return NewRouter(RouterConfig{
		Pool:       pool,
		SkillsRepo: struct{ v2skills.Repository }{},
		AuthValidator: testTokenValidator{user: auth.User{
			ID:     id,
			UserID: id,
			Email:  "settings-caller@autotest.local",
		}},
		PrincipalValidator: testPrincipalValidator{},
	})
}

func TestProjectSettingsPersonalExceptionThroughTheProductionRouter(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	seedPersonalSettingsFixture(t, pool)
	router := newSettingsCallerRouter(pool)

	writes := func(projectID int) []struct{ method, path string } {
		return []struct{ method, path string }{
			{http.MethodPut, fmt.Sprintf("/api/v2/elitea_core/project_info/prompt_lib/%d/project-info", projectID)},
			{http.MethodPost, fmt.Sprintf("/api/v2/elitea_core/project_icon/prompt_lib/%d", projectID)},
			{http.MethodDelete, fmt.Sprintf("/api/v2/elitea_core/project_icon/prompt_lib/%d/icon.png", projectID)},
		}
	}

	// The caller really is an editor of all three, so a refusal below is the
	// settings gate and not a missing membership: the context READ, which
	// an editor holds, is admitted in each.
	for _, projectID := range []int{settingsOwnPersonal, settingsOtherPersonal, settingsSquatted} {
		path := fmt.Sprintf("/api/v2/elitea_core/project_context/prompt_lib/%d/project-context", projectID)
		if recorder := serveMemberWrite(t, router, http.MethodGet, path, nil); recorder.Code == http.StatusForbidden ||
			recorder.Code == http.StatusUnauthorized {
			t.Fatalf("GET %s = %d; the fixture does not make the caller a member", path, recorder.Code)
		}
	}

	for _, route := range writes(settingsOwnPersonal) {
		recorder := serveMemberWrite(t, router, route.method, route.path, map[string]any{})
		if recorder.Code == http.StatusForbidden || recorder.Code == http.StatusUnauthorized {
			t.Errorf("own personal project: %s %s = %d (%s), want the editor-owner admitted",
				route.method, route.path, recorder.Code, recorder.Body.String())
		}
	}
	for name, projectID := range map[string]int{
		"another user's personal project":             settingsOtherPersonal,
		"a team project renamed to the caller's name": settingsSquatted,
	} {
		for _, route := range writes(projectID) {
			recorder := serveMemberWrite(t, router, route.method, route.path, map[string]any{})
			if recorder.Code != http.StatusForbidden {
				t.Errorf("%s: %s %s = %d (%s), want 403",
					name, route.method, route.path, recorder.Code, recorder.Body.String())
			}
		}
	}
}
