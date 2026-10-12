package social_test

// SEC-14: suspension is part of the shared project-membership predicate. This
// is the authorization matrix for it, through the real Social router and real
// PostgreSQL: one gate-only read, the pin write and the feedback create and
// list routes, for every kind of actor, with the project active and
// suspended. A refused write must change no rows and a refusal must carry no
// e-mail address.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
)

const (
	suspensionOwner         = "61"
	suspensionMember        = "62"
	suspensionSuspendedUser = "63"
	suspensionForeign       = "64"
	suspensionNoProject     = "65"
	suspensionAdmin         = "66"
	suspensionNullFlag      = "67"
)

// Users sit on top of prepareCurrentFeedbackDatabase (projects 7 and 8 active,
// 9 suspended; centry.social_feedbacks). 61 and 62 are members of project 7, 63
// a member of project 7 whose account is suspended, 64 a member of project 8
// only, 65 belongs to nothing, 66 is the administration-mode super_admin with
// no project role, 67 a member of project 7 whose suspended flag is NULL.
func prepareSuspensionMatrixDatabase(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	prepareCurrentFeedbackDatabase(t, pool)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO public.auth_core__user (id, email, name, suspended) VALUES
    (61, 'owner@seven.example', 'Owner', FALSE),
    (62, 'member@seven.example', 'Member', FALSE),
    (63, 'suspended-member@seven.example', 'Suspended Member', TRUE),
    (64, 'foreign@eight.example', 'Foreign', FALSE),
    (65, 'loner@nowhere.example', 'Loner', FALSE),
    (66, 'platform-admin@elitea.example', 'Platform Admin', FALSE),
    (67, 'null-flag@seven.example', 'Null Flag', FALSE);
INSERT INTO public.auth_core__role (id, name, mode) VALUES
    (900, 'super_admin', 'administration');
INSERT INTO public.auth_core__user_role (user_id, role_id) VALUES (66, 900);
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES
    (7, 61, 101), (7, 62, 103), (7, 63, 103), (7, 67, 103), (8, 64, 201);
CREATE TABLE centry.social_users (
    id serial PRIMARY KEY, user_id integer NOT NULL UNIQUE, avatar varchar, description varchar,
    personalization jsonb, default_context_management jsonb, default_summarization jsonb);
CREATE TABLE centry.social_pins (
    id serial PRIMARY KEY, entity varchar NOT NULL, project_id integer NOT NULL, entity_id integer NOT NULL,
    user_id integer NOT NULL, created_at timestamp NOT NULL DEFAULT now(), updated_at timestamp NOT NULL DEFAULT now(),
    UNIQUE (entity, project_id, entity_id));
CREATE SCHEMA p_7;
CREATE TABLE p_7.applications (id serial PRIMARY KEY, name varchar NOT NULL);
INSERT INTO p_7.applications (name) VALUES ('pin-target');`); err != nil {
		t.Fatalf("prepare suspension matrix database: %v", err)
	}
}

type suspensionRoute struct {
	name   string
	method string
	target string
	body   string
	// rows names the table whose row count a refused request must not move.
	rows string
	ok   int
}

var suspensionRoutes = []suspensionRoute{
	{"authors (gate only)", http.MethodGet, "/api/v2/social/authors/7", "", "", http.StatusOK},
	{"pin", http.MethodPost, "/api/v2/social/pin/prompt_lib/7/application/1", "", "centry.social_pins", http.StatusOK},
	{"feedback create", http.MethodPost, "/api/v2/social/feedbacks/default/7",
		`{"description":"matrix","rating":4}`, "centry.social_feedbacks", http.StatusCreated},
	{"feedback list", http.MethodGet, "/api/v2/social/feedbacks/default/7", "", "", http.StatusOK},
}

func suspensionRowCount(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	if table == "" {
		return 0
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSocialRoutesRefuseASuspendedMemberAndASuspendedProject(t *testing.T) {
	pool := newCurrentFeedbackPostgresPool(t)
	prepareSuspensionMatrixDatabase(t, pool)
	router := feedbackRouter(pool, handler.WithPermissionResolver(feedbackGrantAll{}))
	exec := func(statement string) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	run := func(t *testing.T, state string, want map[string]int) {
		t.Helper()
		for _, actor := range []string{
			suspensionOwner, suspensionMember, suspensionSuspendedUser, suspensionForeign,
			suspensionNoProject, suspensionAdmin, suspensionNullFlag, "",
		} {
			wantStatus, ok := want[actor]
			if !ok {
				t.Fatalf("no expectation for actor %q", actor)
			}
			for _, route := range suspensionRoutes {
				before := suspensionRowCount(t, pool, route.rows)
				recorder := feedbackDo(router, route.method, actor, route.target, route.body)
				expected := wantStatus
				if wantStatus == http.StatusOK {
					expected = route.ok
				}
				if recorder.Code != expected {
					t.Errorf("%s: actor %q %s = %d (%s), want %d", state, actor, route.name, recorder.Code, recorder.Body, expected)
					continue
				}
				if expected >= 400 {
					if strings.Contains(recorder.Body.String(), "@") {
						t.Errorf("%s: actor %q %s refusal leaked an address: %s", state, actor, route.name, recorder.Body)
					}
					if after := suspensionRowCount(t, pool, route.rows); after != before {
						t.Errorf("%s: actor %q %s refused but %s went from %d to %d rows", state, actor, route.name, route.rows, before, after)
					}
				}
			}
		}
	}

	allowed := http.StatusOK // stands for each route's own success status
	t.Run("active project", func(t *testing.T) {
		exec(`ALTER TABLE public.auth_core__user ALTER COLUMN suspended DROP NOT NULL;
UPDATE public.auth_core__user SET suspended = NULL WHERE id = 67`)
		run(t, "active", map[string]int{
			suspensionOwner:         allowed,
			suspensionMember:        allowed,
			suspensionSuspendedUser: http.StatusForbidden,
			suspensionForeign:       http.StatusForbidden,
			suspensionNoProject:     http.StatusForbidden,
			suspensionAdmin:         allowed,
			suspensionNullFlag:      http.StatusForbidden, // a NULL flag fails closed
			"":                      http.StatusUnauthorized,
		})
	})

	t.Run("suspended project refuses everyone including the administrator", func(t *testing.T) {
		exec(`UPDATE centry.project SET suspended = TRUE WHERE id = 7`)
		refused := http.StatusForbidden
		run(t, "suspended project", map[string]int{
			suspensionOwner:         refused,
			suspensionMember:        refused,
			suspensionSuspendedUser: refused,
			suspensionForeign:       refused,
			suspensionNoProject:     refused,
			suspensionAdmin:         refused,
			suspensionNullFlag:      refused,
			"":                      http.StatusUnauthorized,
		})
	})

	t.Run("a NULL project flag fails closed", func(t *testing.T) {
		exec(`ALTER TABLE centry.project ALTER COLUMN suspended DROP NOT NULL;
UPDATE centry.project SET suspended = NULL WHERE id = 7`)
		for _, actor := range []string{suspensionOwner, suspensionAdmin} {
			if got := feedbackDo(router, http.MethodGet, actor, "/api/v2/social/authors/7", "").Code; got != http.StatusForbidden {
				t.Errorf("actor %s on a project with a NULL flag = %d, want 403", actor, got)
			}
		}
	})

	t.Run("lifting the suspension restores access", func(t *testing.T) {
		exec(`UPDATE centry.project SET suspended = FALSE WHERE id = 7`)
		run(t, "restored", map[string]int{
			suspensionOwner:         allowed,
			suspensionMember:        allowed,
			suspensionSuspendedUser: http.StatusForbidden,
			suspensionForeign:       http.StatusForbidden,
			suspensionNoProject:     http.StatusForbidden,
			suspensionAdmin:         allowed,
			suspensionNullFlag:      http.StatusForbidden,
			"":                      http.StatusUnauthorized,
		})
		// The suspended account is admitted again once the flag is cleared.
		exec(`UPDATE public.auth_core__user SET suspended = FALSE WHERE id IN (63, 67)`)
		for _, actor := range []string{suspensionSuspendedUser, suspensionNullFlag} {
			if got := feedbackDo(router, http.MethodGet, actor, "/api/v2/social/authors/7", "").Code; got != http.StatusOK {
				t.Errorf("actor %s after the flag was cleared = %d, want 200", actor, got)
			}
		}
	})

	t.Run("an unknown project is 404 for the administrator and 403 for everyone else", func(t *testing.T) {
		if got := feedbackDo(router, http.MethodGet, suspensionAdmin, "/api/v2/social/authors/99", "").Code; got != http.StatusNotFound {
			t.Errorf("administrator on an unknown project = %d, want 404", got)
		}
		if got := feedbackDo(router, http.MethodGet, suspensionOwner, "/api/v2/social/authors/99", "").Code; got != http.StatusForbidden {
			t.Errorf("member of another project on an unknown project = %d, want 403", got)
		}
	})
}
