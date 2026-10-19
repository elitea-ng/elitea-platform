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

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/egress"
)

// The authorization matrix of /httpactions (gate §3a) on a CLEAN database:
// the real router, the real legacyrbac resolver, the 0170 grants and the real
// repository. Project 1 is Go-provisioned (no per-project rows), so the
// central default-mode grants decide: manage is admin-only, list (rule names)
// is admin, editor and viewer. Each principal is tried with a token and with a
// session shape.

type httpActionPrincipal struct {
	name    string
	userID  int
	project int // the project the user is a member of; 0 = none
	role    string
}

var httpActionPrincipals = []httpActionPrincipal{
	{"admin", 17101, 1, "admin"},
	{"editor", 17102, 1, "editor"},
	{"viewer", 17103, 1, "viewer"},
	{"foreign admin", 17104, 2, "admin"},
	{"no project", 17105, 0, ""},
}

type httpActionResolver map[string][]net.IPAddr

func (r httpActionResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if addrs, ok := r[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func TestHTTPActionRulesAuthorizationMatrixOnACleanDatabase(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), credentialJourneyDeadline)
	defer cancel()

	tokens := tokenTable{}
	for _, principal := range httpActionPrincipals {
		if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name) VALUES ($1, $2, $2) ON CONFLICT (id) DO NOTHING`,
			principal.userID, fmt.Sprintf("http-action-%d@test.local", principal.userID)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__token (id, uuid, user_id, name) VALUES ($1, $2, $1, 'matrix')`,
			principal.userID, fmt.Sprintf("http-action-pat-%d", principal.userID)); err != nil {
			t.Fatal(err)
		}
		if principal.project != 0 {
			if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role (project_id, name) VALUES ($1, $2) ON CONFLICT (project_id, name) DO NOTHING;`,
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
	var overrides int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.auth_core__project_role_permission WHERE project_id IN (1, 2)`).Scan(&overrides); err != nil {
		t.Fatal(err)
	}
	if overrides != 0 {
		t.Fatalf("projects 1 and 2 carry %d per-project rows; this matrix measures the central grants", overrides)
	}

	rules, err := repos.NewHTTPActionRulesRepo(pool)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := egress.NewHTTPActionGuard("", egress.WithResolver(httpActionResolver{
		"api.example.com": {{IP: net.ParseIP("93.184.216.34")}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(RouterConfig{
		Pool:               pool,
		AuthValidator:      apimw.TokenValidator(tokens),
		PrincipalValidator: testPrincipalValidator{},
		HTTPActionRules:    rules,
		HTTPActionGuard:    guard,
	})

	created := 0
	body := func(revision int64) string {
		created++
		payload := map[string]any{
			"name": fmt.Sprintf("matrix_%03d", created), "origin": "https://api.example.com", "path_template": "/v1/items/{id}",
			"method": "GET", "query_names": []string{}, "configuration_id": 0, "toolkit": "openapi", "tool": "get_item", "enabled": true,
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

	// The rule every read and update below names: created by the admin.
	status, response := serve("token-17101", http.MethodPost, "/api/v2/httpactions/prompt_lib/1/rules", body(0))
	if status != http.StatusCreated {
		t.Fatalf("the admin's create answered %d: %s", status, response)
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
		if err := pool.QueryRow(ctx, `SELECT revision FROM elitea_runtime.http_action_rules WHERE id = $1`, rule.ID).Scan(&current); err != nil {
			t.Fatal(err)
		}
		return current
	}
	ruleCount := func() int {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.http_action_rules`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	type route struct {
		name   string
		method string
		path   string
		names  bool // the rule-names route (list permission)
	}
	routes := []route{
		{"list", http.MethodGet, "/api/v2/httpactions/prompt_lib/1/rules", false},
		{"create", http.MethodPost, "/api/v2/httpactions/prompt_lib/1/rules", false},
		{"get", http.MethodGet, fmt.Sprintf("/api/v2/httpactions/prompt_lib/1/rules/%d", rule.ID), false},
		{"update", http.MethodPut, fmt.Sprintf("/api/v2/httpactions/prompt_lib/1/rules/%d", rule.ID), false},
		{"rule names", http.MethodGet, "/api/v2/httpactions/prompt_lib/1/rule-names", true},
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
		// Unauthenticated.
		if status, response := serve("", r.method, r.path, payload()); status != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated answered %d: %s", r.name, status, response)
		}
		for _, principal := range httpActionPrincipals {
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
						t.Errorf("a refused %s by %s (%s) changed the rule table", r.name, principal.name, shape)
					}
					if strings.Contains(response, "api.example.com") || strings.Contains(response, "matrix_") {
						t.Errorf("a refused %s by %s (%s) disclosed rule content: %s", r.name, principal.name, shape, response)
					}
				}
			}
		}
	}

	// The foreign admin's own project: its rule names are reachable, and
	// project 1's rule id answers exactly what a missing id answers.
	if status, response := serve("token-17104", http.MethodGet, "/api/v2/httpactions/prompt_lib/2/rule-names", ""); status != http.StatusOK ||
		strings.Contains(response, "matrix_") {
		t.Fatalf("the foreign admin's own rule names answered %d: %s", status, response)
	}
	crossProject := fmt.Sprintf("/api/v2/httpactions/prompt_lib/2/rules/%d", rule.ID)
	statusCross, responseCross := serve("token-17104", http.MethodGet, crossProject, "")
	statusMissing, responseMissing := serve("token-17104", http.MethodGet, "/api/v2/httpactions/prompt_lib/2/rules/999999", "")
	if statusCross != http.StatusNotFound || statusCross != statusMissing || responseCross != responseMissing {
		t.Fatalf("another project's rule id answered %d %s; a missing one %d %s", statusCross, responseCross, statusMissing, responseMissing)
	}
	if status, _ := serve("token-17104", http.MethodPut, crossProject, body(revision())); status != http.StatusNotFound {
		t.Fatalf("an update of another project's rule id answered %d", status)
	}
	t.Logf("%d authenticated route x principal x shape cases", checked)
}
