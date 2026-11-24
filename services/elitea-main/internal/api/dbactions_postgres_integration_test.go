package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
)

// The authorization matrix of /dbactions (gate §3a) on a CLEAN database:
// real router, real legacyrbac resolver, 0271 grants, and real repository.
// Each principal is exercised with both token and session auth shapes across
// all five routes (list, create, get, update, rule-names).

type dbActionPrincipal struct {
	name    string
	userID  int
	project int
	role    string
}

var dbActionPrincipals = []dbActionPrincipal{
	{"admin", 17301, 1, "admin"},
	{"editor", 17302, 1, "editor"},
	{"viewer", 17303, 1, "viewer"},
	{"foreign admin", 17304, 2, "admin"},
	{"no project", 17305, 0, ""},
}

func TestDBActionRulesAuthorizationMatrixOnACleanDatabase(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), credentialJourneyDeadline)
	defer cancel()

	// Seed a shared SQL configuration in project 1 so valid creates succeed.
	const sqlConfigID = 741
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.configuration (id,uuid,project_id,elitea_title,type,section,data,meta,shared,status_ok,source,author_id,created_at)
VALUES ($1,'60000000-0000-4000-8000-000000000741',1,'matrix_sql','sql','credentials',
 '{"host":"warehouse.example.com","port":5432,"username":"etl","password":"{{secret.PASS}}"}'::jsonb,
 '{}'::jsonb,true,true,'user',17301,now() AT TIME ZONE 'UTC')`, sqlConfigID); err != nil {
		t.Fatal(err)
	}

	tokens := tokenTable{}
	for _, principal := range dbActionPrincipals {
		if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name) VALUES ($1, $2, $2) ON CONFLICT (id) DO NOTHING`,
			principal.userID, fmt.Sprintf("db-action-%d@test.local", principal.userID)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__token (id, uuid, user_id, name) VALUES ($1, $2, $1, 'matrix')`,
			principal.userID, fmt.Sprintf("db-action-pat-%d", principal.userID)); err != nil {
			t.Fatal(err)
		}
		if principal.project != 0 {
			if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role (project_id, name) VALUES ($1, $2) ON CONFLICT (project_id, name) DO NOTHING`,
				principal.project, principal.role); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT $1, $2, id FROM public.auth_core__project_role WHERE project_id = $1 AND name = $3`,
				principal.project, principal.userID, principal.role); err != nil {
				t.Fatal(err)
			}
		}
		id := fmt.Sprint(principal.userID)
		tokens["token-"+id] = auth.User{ID: id, UserID: id, TokenID: id, AuthType: "token"}
		tokens["session-"+id] = auth.User{ID: id, UserID: id, AuthType: "session"}
	}

	rules, err := repos.NewDBActionRulesRepo(pool)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := egress.NewDBActionGuard("", egress.WithResolver(httpActionResolver{
		"warehouse.example.com": {{IP: net.ParseIP("93.184.216.34")}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(RouterConfig{
		Pool:               pool,
		AuthValidator:      apimw.TokenValidator(tokens),
		PrincipalValidator: testPrincipalValidator{},
		DBActionRules:      rules,
		DBActionGuard:      guard,
	})

	created := 0
	body := func(revision int64) string {
		created++
		payload := map[string]any{
			"name":             fmt.Sprintf("db_matrix_%03d", created),
			"configuration_id": sqlConfigID,
			"dialect":          "postgres",
			"database_name":    "analytics",
			"mode":             "read",
			"enabled":          true,
		}
		if revision > 0 {
			payload["revision"] = revision
		}
		encoded, _ := json.Marshal(payload)
		return string(encoded)
	}
	serve := func(token, method, path, payload string) (int, string) {
		request := httptest.NewRequest(method, path, strings.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder.Code, recorder.Body.String()
	}

	status, response := serve("token-17301", http.MethodPost, "/api/v2/dbactions/prompt_lib/1/rules", body(0))
	if status != http.StatusCreated {
		t.Fatalf("admin create answered %d: %s", status, response)
	}
	var rule struct {
		ID       int64 `json:"id"`
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal([]byte(response), &rule); err != nil || rule.ID <= 0 {
		t.Fatalf("create response %s: %v", response, err)
	}
	revision := func() int64 {
		var current int64
		if err := pool.QueryRow(ctx, `SELECT revision FROM elitea_runtime.db_action_rules WHERE id = $1`, rule.ID).Scan(&current); err != nil {
			t.Fatal(err)
		}
		return current
	}
	ruleCount := func() int {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.db_action_rules`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	routes := []struct {
		name   string
		method string
		path   string
		names  bool
	}{
		{"list", http.MethodGet, "/api/v2/dbactions/prompt_lib/1/rules", false},
		{"create", http.MethodPost, "/api/v2/dbactions/prompt_lib/1/rules", false},
		{"get", http.MethodGet, fmt.Sprintf("/api/v2/dbactions/prompt_lib/1/rules/%d", rule.ID), false},
		{"update", http.MethodPut, fmt.Sprintf("/api/v2/dbactions/prompt_lib/1/rules/%d", rule.ID), false},
		{"rule names", http.MethodGet, "/api/v2/dbactions/prompt_lib/1/rule-names", true},
	}
	checked := 0
	for _, r := range routes {
		payload := func() string {
			switch r.method {
			case http.MethodPost:
				return body(0)
			case http.MethodPut:
				return body(revision())
			}
			return ""
		}
		success := http.StatusOK
		if r.method == http.MethodPost {
			success = http.StatusCreated
		}
		if status, response := serve("", r.method, r.path, payload()); status != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated answered %d: %s", r.name, status, response)
		}
		for _, principal := range dbActionPrincipals {
			want := http.StatusForbidden
			switch {
			case principal.project != 1:
			case principal.role == "admin":
				want = success
			case r.names:
				want = http.StatusOK
			}
			for _, shape := range []string{"token", "session"} {
				before := ruleCount()
				status, response := serve(fmt.Sprintf("%s-%d", shape, principal.userID), r.method, r.path, payload())
				checked++
				if status != want {
					t.Errorf("%s by %s (%s) answered %d, want %d: %s", r.name, principal.name, shape, status, want, response)
				}
				if want == http.StatusForbidden {
					if after := ruleCount(); after != before {
						t.Errorf("refused %s by %s (%s) changed the rule table", r.name, principal.name, shape)
					}
					if strings.Contains(response, "analytics") || strings.Contains(response, "db_matrix_") {
						t.Errorf("refused %s by %s (%s) disclosed rule content: %s", r.name, principal.name, shape, response)
					}
				}
			}
		}
	}

	// Foreign admin's own project: cross-project rule id answers identical 404 to missing id.
	if status, response := serve("token-17304", http.MethodGet, "/api/v2/dbactions/prompt_lib/2/rule-names", ""); status != http.StatusOK ||
		strings.Contains(response, "db_matrix_") {
		t.Fatalf("foreign admin rule names answered %d: %s", status, response)
	}
	crossProject := fmt.Sprintf("/api/v2/dbactions/prompt_lib/2/rules/%d", rule.ID)
	statusCross, responseCross := serve("token-17304", http.MethodGet, crossProject, "")
	statusMissing, responseMissing := serve("token-17304", http.MethodGet, "/api/v2/dbactions/prompt_lib/2/rules/999999", "")
	if statusCross != http.StatusNotFound || statusCross != statusMissing || responseCross != responseMissing {
		t.Fatalf("cross-project rule id answered %d %s; missing %d %s", statusCross, responseCross, statusMissing, responseMissing)
	}
	putCrossStatus, putCrossBody := serve("token-17304", http.MethodPut, crossProject, body(revision()))
	putMissingStatus, putMissingBody := serve("token-17304", http.MethodPut, "/api/v2/dbactions/prompt_lib/2/rules/999999", body(revision()))
	if putCrossStatus != http.StatusNotFound || putCrossStatus != putMissingStatus || putCrossBody != putMissingBody {
		t.Fatalf("cross-project update answered %d %s; missing %d %s", putCrossStatus, putCrossBody, putMissingStatus, putMissingBody)
	}
	t.Logf("%d authenticated route x principal x shape cases", checked)
}
