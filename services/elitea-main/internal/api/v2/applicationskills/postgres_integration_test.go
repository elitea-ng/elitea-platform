package applicationskills_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	platformapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applicationskills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentApplicationSkillsRoutePostgresContractAndTenantIsolation(t *testing.T) {
	pool := newCurrentApplicationSkillsPostgresPool(t)
	prepareCurrentApplicationSkillsProjects(t, pool)
	assertCurrentApplicationSkillsSchema(t, pool)

	repository, err := handler.NewCurrentApplicationSkillsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	route := newCurrentApplicationSkillsRoute(
		t,
		repository,
		currentApplicationSkillsPermissionResolverFunc(
			func(
				context.Context,
				auth.User,
				string,
				string,
			) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{
					UserID:      11,
					Permissions: []string{handler.CurrentApplicationSkillsPermission},
				}, nil
			},
		),
	)

	projectOne := httptest.NewRecorder()
	route.ServeHTTP(projectOne, currentApplicationSkillsRequest(
		http.MethodGet,
		"/api/v2/elitea_core/application_skills/prompt_lib/1/301",
		true,
		"10.0.0.8:43120",
	))
	if projectOne.Code != http.StatusOK {
		t.Fatalf("project one status=%d body=%s", projectOne.Code, projectOne.Body.String())
	}
	var projectOneBody struct {
		Items []struct {
			ID        string `json:"id"`
			ProjectID string `json:"project_id"`
			Name      string `json:"name"`
			Type      string `json:"type"`
			CreatedAt string `json:"created_at"`
		} `json:"items"`
		Total      int                               `json:"total"`
		Page       int                               `json:"page"`
		PageSize   int                               `json:"page_size"`
		TotalPages int                               `json:"total_pages"`
		Skills     []handler.CurrentApplicationSkill `json:"skills"`
		MaxSkills  int                               `json:"max_skills"`
	}
	if err := json.NewDecoder(projectOne.Body).Decode(&projectOneBody); err != nil {
		t.Fatal(err)
	}
	if projectOneBody.MaxSkills != 5 || len(projectOneBody.Skills) != 2 {
		t.Fatalf("project one response=%+v", projectOneBody)
	}
	// The published SkillsList half (#395), read from the same rows. Without
	// it apps/elitea-web sees a body with no `items` key and renders an empty
	// skill list for an agent version that has two skills attached.
	if len(projectOneBody.Items) != 2 || projectOneBody.Total != 2 ||
		projectOneBody.Page != 1 || projectOneBody.PageSize != 2 ||
		projectOneBody.TotalPages != 1 {
		t.Fatalf("project one published half=%+v", projectOneBody)
	}
	for _, item := range projectOneBody.Items {
		if item.ProjectID != "1" || item.Type != "skill" || item.ID == "" ||
			item.Name == "" || item.CreatedAt == "" ||
			strings.HasPrefix(item.CreatedAt, "0001-01-01") {
			t.Fatalf("published item=%+v", item)
		}
	}
	byID := make(map[int32]handler.CurrentApplicationSkill, len(projectOneBody.Skills))
	for _, skill := range projectOneBody.Skills {
		byID[skill.SkillID] = skill
	}
	present := byID[101]
	if present.Name != "deploy" || present.Description != "Deploy safely" ||
		present.VersionID == nil || *present.VersionID != 201 ||
		present.VersionName != "release" || present.VersionMissing {
		t.Fatalf("present version=%+v icon=%s", present, present.IconMeta)
	}
	var icon map[string]string
	if err := json.Unmarshal(present.IconMeta, &icon); err != nil ||
		icon["url"] != "/icons/deploy.svg" ||
		icon["type"] != "image/svg+xml" {
		t.Fatalf("present icon=%s err=%v", present.IconMeta, err)
	}
	review := byID[102]
	if review.Name != "review" || review.VersionID == nil ||
		*review.VersionID != 202 ||
		review.VersionName != "base" || review.VersionMissing ||
		string(review.IconMeta) != "null" {
		t.Fatalf("review version=%+v icon=%s", review, review.IconMeta)
	}
	if _, found := byID[103]; found {
		t.Fatalf("non-agent mapping escaped filter: %+v", byID[103])
	}

	projectTwo := httptest.NewRecorder()
	route.ServeHTTP(projectTwo, currentApplicationSkillsRequest(
		http.MethodGet,
		"/api/v2/elitea_core/application_skills/prompt_lib/2/301",
		true,
		"10.0.0.8:43120",
	))
	if projectTwo.Code != http.StatusOK {
		t.Fatalf("project two status=%d body=%s", projectTwo.Code, projectTwo.Body.String())
	}
	var projectTwoBody struct {
		Skills []handler.CurrentApplicationSkill `json:"skills"`
	}
	if err := json.NewDecoder(projectTwo.Body).Decode(&projectTwoBody); err != nil {
		t.Fatal(err)
	}
	if len(projectTwoBody.Skills) != 1 ||
		projectTwoBody.Skills[0].Name != "private-project-skill" ||
		projectTwoBody.Skills[0].SkillID != 101 {
		t.Fatalf("project two response=%+v", projectTwoBody)
	}
}

func assertCurrentApplicationSkillsSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, schema := range []string{"p_1", "p_2"} {
		var nonNullable bool
		if err := pool.QueryRow(ctx, `
SELECT is_nullable = 'NO'
FROM information_schema.columns
WHERE table_schema = $1
  AND table_name = 'entity_skill_mapping'
  AND column_name = 'skill_version_id'`,
			schema,
		).Scan(&nonNullable); err != nil || !nonNullable {
			t.Fatalf(
				"%s skill_version_id NOT NULL=%t error=%v",
				schema,
				nonNullable,
				err,
			)
		}

		var mappingForeignKeys, versionForeignKeys int
		if err := pool.QueryRow(
			ctx,
			`SELECT count(*) FROM pg_constraint
WHERE conrelid = to_regclass($1)
  AND contype = 'f'`,
			schema+".entity_skill_mapping",
		).Scan(&mappingForeignKeys); err != nil {
			t.Fatalf("%s mapping foreign keys: %v", schema, err)
		}
		if err := pool.QueryRow(
			ctx,
			`SELECT count(*) FROM pg_constraint
WHERE conrelid = to_regclass($1)
  AND contype = 'f'`,
			schema+".skill_versions",
		).Scan(&versionForeignKeys); err != nil {
			t.Fatalf("%s version foreign keys: %v", schema, err)
		}
		if mappingForeignKeys != 2 || versionForeignKeys != 1 {
			t.Fatalf(
				"%s foreign keys mapping=%d version=%d",
				schema,
				mappingForeignKeys,
				versionForeignKeys,
			)
		}
	}
}

func TestCurrentApplicationSkillsHTTPPostgresRBACAndTenantMatrix(t *testing.T) {
	pool := newCurrentApplicationSkillsPostgresPool(t)
	prepareCurrentApplicationSkillsProjects(t, pool)

	repository, err := handler.NewCurrentApplicationSkillsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	route, err := handler.NewCurrentApplicationSkillsRoute(
		repository,
		apimw.AuthConfig{
			PrincipalValidator: authsvc.NewPrincipalValidator(pool),
			ForwardedIdentityVerifier: currentApplicationSkillsPeerVerifierFunc(
				func(request *http.Request) error {
					host, _, splitErr := net.SplitHostPort(request.RemoteAddr)
					if splitErr == nil &&
						(host == "127.0.0.1" || host == "::1") {
						return nil
					}
					return fmt.Errorf("untrusted peer")
				},
			),
		},
		legacyrbac.NewPostgresResolver(pool),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(platformapi.NewRouter(platformapi.RouterConfig{
		CurrentApplicationSkills: route,
	}))
	defer server.Close()

	tests := []struct {
		name       string
		projectID  string
		appVersion string
		userID     string
		wantStatus int
		wantBody   string
		forbidBody string
		exactBody  string
	}{
		{
			name:       "project viewer",
			projectID:  "1",
			appVersion: "301",
			userID:     "11",
			wantStatus: http.StatusOK,
			wantBody:   `"name":"deploy"`,
			forbidBody: `"name":"private-project-skill"`,
		},
		{
			name:       "project editor",
			projectID:  "1",
			appVersion: "301",
			userID:     "16",
			wantStatus: http.StatusOK,
			wantBody:   `"name":"review"`,
			forbidBody: `"name":"private-project-skill"`,
		},
		{
			name:       "other project viewer",
			projectID:  "2",
			appVersion: "301",
			userID:     "12",
			wantStatus: http.StatusOK,
			wantBody:   `"name":"private-project-skill"`,
			forbidBody: `"name":"deploy"`,
		},
		{
			name:       "missing positive version remains empty",
			projectID:  "1",
			appVersion: "999999",
			userID:     "11",
			wantStatus: http.StatusOK,
			exactBody:  "{\"items\":[],\"total\":0,\"page\":1,\"page_size\":0,\"total_pages\":0,\"skills\":[],\"max_skills\":5}\n",
		},
		{
			name:       "zero version remains empty",
			projectID:  "1",
			appVersion: "0",
			userID:     "11",
			wantStatus: http.StatusOK,
			exactBody:  "{\"items\":[],\"total\":0,\"page\":1,\"page_size\":0,\"total_pages\":0,\"skills\":[],\"max_skills\":5}\n",
		},
		{
			name:       "version beyond PostgreSQL integer remains empty",
			projectID:  "1",
			appVersion: "9999999999999999999999999999999999999999",
			userID:     "11",
			wantStatus: http.StatusOK,
			exactBody:  "{\"items\":[],\"total\":0,\"page\":1,\"page_size\":0,\"total_pages\":0,\"skills\":[],\"max_skills\":5}\n",
		},
		{
			name:       "nonnumeric version follows current routing 404",
			projectID:  "1",
			appVersion: "not-an-id",
			userID:     "11",
			wantStatus: http.StatusNotFound,
			exactBody:  "{\"message\":\"The requested URL was not found on the server. If you entered the URL manually please check your spelling and try again.\"}\n",
		},
		{
			name:       "cross-project membership",
			projectID:  "2",
			appVersion: "301",
			userID:     "11",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "zero project fails current RBAC closed",
			projectID:  "0",
			appVersion: "301",
			userID:     "11",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "project beyond PostgreSQL integer fails current RBAC closed",
			projectID:  "9999999999999999999999999999999999999999",
			appVersion: "301",
			userID:     "11",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "project role without permission",
			projectID:  "1",
			appVersion: "301",
			userID:     "13",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "suspended principal",
			projectID:  "1",
			appVersion: "301",
			userID:     "14",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "platform admin does not inherit project access",
			projectID:  "1",
			appVersion: "301",
			userID:     "15",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "suspended project",
			projectID:  "3",
			appVersion: "301",
			userID:     "11",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(
				http.MethodGet,
				server.URL+
					"/api/v2/elitea_core/application_skills/prompt_lib/"+
					test.projectID+"/"+test.appVersion,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Auth-Type", "user")
			request.Header.Set("X-Auth-ID", test.userID)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			if response.StatusCode != test.wantStatus {
				t.Fatalf(
					"status=%d want=%d body=%s",
					response.StatusCode,
					test.wantStatus,
					body,
				)
			}
			if test.exactBody != "" && string(body) != test.exactBody {
				t.Fatalf("body=%q want=%q", body, test.exactBody)
			}
			if test.wantBody != "" && !strings.Contains(string(body), test.wantBody) {
				t.Fatalf("body=%s missing=%s", body, test.wantBody)
			}
			if test.forbidBody != "" && strings.Contains(string(body), test.forbidBody) {
				t.Fatalf("tenant data leaked: %s", body)
			}
		})
	}
}

func newCurrentApplicationSkillsPostgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_application_skills_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("ping isolated PostgreSQL integration database: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(
			dropCtx,
			"DROP DATABASE "+quotedDatabase+" WITH (FORCE)",
		); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})
	return pool
}

func prepareCurrentApplicationSkillsProjects(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id INTEGER PRIMARY KEY,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO centry.project (id, suspended) VALUES
    (1, FALSE),
    (2, FALSE),
    (3, TRUE);

CREATE TABLE public.auth_core__user (
    id INTEGER PRIMARY KEY,
    email TEXT,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO public.auth_core__user (id, email, suspended) VALUES
    (11, 'viewer-one@example.test', FALSE),
    (12, 'viewer-two@example.test', FALSE),
    (13, 'wrong-permission@example.test', FALSE),
    (14, 'suspended@example.test', TRUE),
    (15, 'platform-admin@example.test', FALSE),
    (16, 'editor-one@example.test', FALSE);

CREATE TABLE public.auth_core__role (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    mode TEXT NOT NULL
);
CREATE TABLE public.auth_core__role_permission (
    role_id INTEGER NOT NULL,
    permission TEXT NOT NULL
);
CREATE TABLE public.auth_core__user_role (
    user_id INTEGER NOT NULL,
    role_id INTEGER NOT NULL
);
INSERT INTO public.auth_core__role (id, name, mode) VALUES
    (1, 'super_admin', 'administration');
INSERT INTO public.auth_core__role_permission (role_id, permission) VALUES
    (1, 'models.applications.applications.details');
INSERT INTO public.auth_core__user_role (user_id, role_id) VALUES (15, 1);

CREATE TABLE public.auth_core__project_role (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL,
    name TEXT NOT NULL
);
CREATE TABLE public.auth_core__project_role_permission (
    project_id INTEGER NOT NULL,
    role_id INTEGER NOT NULL,
    permission TEXT NOT NULL
);
CREATE TABLE public.auth_core__project_user_role (
    project_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    role_id INTEGER NOT NULL
);
INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES
    (101, 1, 'viewer'),
    (102, 1, 'editor'),
    (103, 1, 'restricted'),
    (201, 2, 'viewer'),
    (301, 3, 'viewer');
INSERT INTO public.auth_core__project_role_permission (
    project_id, role_id, permission
) VALUES
    (1, 101, 'models.applications.applications.details'),
    (1, 102, 'models.applications.applications.details'),
    (1, 103, 'models.applications.skills.list'),
    (2, 201, 'models.applications.applications.details'),
    (3, 301, 'models.applications.applications.details');
INSERT INTO public.auth_core__project_user_role (
    project_id, user_id, role_id
) VALUES
    (1, 11, 101),
    (1, 13, 103),
    (1, 14, 101),
    (1, 16, 102),
    (2, 12, 201),
    (3, 11, 301);

CREATE SCHEMA p_1;
CREATE SCHEMA p_2;

CREATE TABLE p_1.skills (
    id INTEGER PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    description VARCHAR(2304) NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT now()
);
CREATE TABLE p_1.skill_versions (
    id INTEGER PRIMARY KEY,
    skill_id INTEGER NOT NULL REFERENCES p_1.skills(id) ON DELETE CASCADE,
    name VARCHAR(128) NOT NULL,
    instructions TEXT NOT NULL DEFAULT '',
    meta JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE p_1.entity_skill_mapping (
    id INTEGER PRIMARY KEY,
    entity_version_id INTEGER NOT NULL,
    entity_type VARCHAR(50) NOT NULL,
    skill_id INTEGER NOT NULL REFERENCES p_1.skills(id) ON DELETE CASCADE,
    skill_version_id INTEGER NOT NULL REFERENCES p_1.skill_versions(id),
    UNIQUE (entity_version_id, skill_id, entity_type)
);
INSERT INTO p_1.skills (id, name, description) VALUES
    (101, 'deploy', 'Deploy safely'),
    (102, 'review', 'Review changes'),
    (103, 'pipeline-only', 'Not an agent skill');
INSERT INTO p_1.skill_versions (id, skill_id, name, meta) VALUES
    (201, 101, 'release', '{"icon_meta":{"url":"/icons/deploy.svg","type":"image/svg+xml"}}'),
    (202, 102, 'base', '{}'),
    (203, 103, 'base', '{}');
INSERT INTO p_1.entity_skill_mapping (
    id, entity_version_id, entity_type, skill_id, skill_version_id
) VALUES
    (1, 301, 'agent', 101, 201),
    (2, 301, 'agent', 102, 202),
    (3, 301, 'pipeline', 103, 203),
    (4, 302, 'agent', 103, 203);

CREATE TABLE p_2.skills (
    id INTEGER PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    description VARCHAR(2304) NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT now()
);
CREATE TABLE p_2.skill_versions (
    id INTEGER PRIMARY KEY,
    skill_id INTEGER NOT NULL REFERENCES p_2.skills(id) ON DELETE CASCADE,
    name VARCHAR(128) NOT NULL,
    instructions TEXT NOT NULL DEFAULT '',
    meta JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE p_2.entity_skill_mapping (
    id INTEGER PRIMARY KEY,
    entity_version_id INTEGER NOT NULL,
    entity_type VARCHAR(50) NOT NULL,
    skill_id INTEGER NOT NULL REFERENCES p_2.skills(id) ON DELETE CASCADE,
    skill_version_id INTEGER NOT NULL REFERENCES p_2.skill_versions(id),
    UNIQUE (entity_version_id, skill_id, entity_type)
);
INSERT INTO p_2.skills (id, name, description)
VALUES (101, 'private-project-skill', 'Tenant two only');
INSERT INTO p_2.skill_versions (id, skill_id, name, meta)
VALUES (201, 101, 'base', '{}');
INSERT INTO p_2.entity_skill_mapping (
    id, entity_version_id, entity_type, skill_id, skill_version_id
) VALUES (1, 301, 'agent', 101, 201);`); err != nil {
		t.Fatalf("prepare current application-skills projects: %v", err)
	}
}
