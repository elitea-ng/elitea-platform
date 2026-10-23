package projectinfo_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	platformapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projectinfo"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProductionProjectInfoHTTPPostgresContractRBACAndTenantIsolation(t *testing.T) {
	pool := newCurrentProjectInfoPostgresPool(t)
	prepareCurrentProjectInfoProjects(t, pool)

	repository, err := handler.NewCurrentProjectInfoRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	route, err := handler.NewCurrentProjectInfoRoute(
		repository,
		apimw.AuthConfig{
			PrincipalValidator: authsvc.NewPrincipalValidator(pool),
			FormSessions:       authtest.FormSessions{},
		},
		legacyrbac.NewPostgresResolver(pool),
	)
	if err != nil {
		t.Fatal(err)
	}
	publicRouter := platformapi.NewRouter(platformapi.RouterConfig{
		CurrentProjectInfo: route,
	})
	publicServer := httptest.NewServer(publicRouter)
	defer publicServer.Close()

	t.Run("object icon and distinct teammates excluding project system user", func(t *testing.T) {
		request, err := http.NewRequest(
			http.MethodGet,
			publicServer.URL+"/api/v2/elitea_core/project_info/prompt_lib/1/project-info",
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		authtest.SignIn(request, "11")
		response, err := publicServer.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := response.Body.Close(); err != nil {
				t.Errorf("close project-info response body: %v", err)
			}
		}()
		payload, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.StatusCode, payload)
		}

		var body handler.CurrentProjectInfo
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatal(err)
		}
		// User 12 has two role rows but is counted once. The current user,
		// suspended member, wrong-permission member, global system user, and
		// null-email member remain in the current baseline count. Only the
		// project-specific system_user_1 identity is filtered.
		if body.TeammatesCount != 6 {
			t.Fatalf("teammates=%d want=6 body=%s", body.TeammatesCount, payload)
		}
		var icon map[string]string
		if err := json.Unmarshal(body.IconMeta, &icon); err != nil ||
			icon["url"] != "/project-icons/one.svg" ||
			icon["type"] != "image/svg+xml" {
			t.Fatalf("icon=%s decoded=%+v err=%v", body.IconMeta, icon, err)
		}
	})

	t.Run("missing project system user and missing icon preserve count and null", func(t *testing.T) {
		response := httptest.NewRecorder()
		publicRouter.ServeHTTP(response, currentProjectInfoRequest(
			http.MethodGet,
			"/api/v2/elitea_core/project_info/prompt_lib/3/project-info",
			true,
			"10.0.0.8:43120",
			"11",
		))
		if response.Code != http.StatusOK ||
			response.Body.String() != "{\"teammates_count\":2,\"icon_meta\":null}\n" {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})

	for _, test := range []struct {
		name      string
		projectID string
		userID    string
	}{
		{
			name:      "cross-project membership is denied",
			projectID: "2",
			userID:    "11",
		},
		{
			name:      "wrong permission is denied",
			projectID: "1",
			userID:    "15",
		},
		{
			name:      "suspended principal is denied",
			projectID: "1",
			userID:    "14",
		},
		{
			name:      "suspended project is denied",
			projectID: "4",
			userID:    "11",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			publicRouter.ServeHTTP(response, currentProjectInfoRequest(
				http.MethodGet,
				"/api/v2/elitea_core/project_info/prompt_lib/"+test.projectID+"/project-info",
				true,
				"10.0.0.8:43120",
				test.userID,
			))
			wantStatus := http.StatusForbidden
			if test.userID == "14" {
				wantStatus = http.StatusUnauthorized
			}
			if response.Code != wantStatus ||
				strings.Contains(response.Body.String(), "tenant-two-canary") ||
				strings.Contains(response.Body.String(), "suspended-project-canary") {
				t.Fatalf(
					"status=%d want=%d body=%q",
					response.Code,
					wantStatus,
					response.Body.String(),
				)
			}
		})
	}

	t.Run("membership dependency failure falls back to zero while icon remains available", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(
			ctx,
			`ALTER TABLE public.auth_core__project_user_role
			 RENAME TO auth_core__project_user_role_unavailable`,
		); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := pool.Exec(
				context.Background(),
				`ALTER TABLE public.auth_core__project_user_role_unavailable
				 RENAME TO auth_core__project_user_role`,
			); err != nil {
				t.Errorf("restore membership table: %v", err)
			}
		}()

		result, err := repository.GetCurrentProjectInfo(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if result.TeammatesCount != 0 ||
			!strings.Contains(string(result.IconMeta), "/project-icons/one.svg") {
			t.Fatalf("fallback result=%+v icon=%s", result, result.IconMeta)
		}
	})
}

func newCurrentProjectInfoPostgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
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

	databaseName := fmt.Sprintf("elitea_project_info_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool ping failure: %v", dropErr)
		}
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

func prepareCurrentProjectInfoProjects(t *testing.T, pool *pgxpool.Pool) {
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
    (3, FALSE),
    (4, TRUE);

CREATE TABLE public.auth_core__user (
    id INTEGER PRIMARY KEY,
    email TEXT UNIQUE,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO public.auth_core__user (id, email, suspended) VALUES
    (11, 'allowed@elitea.example', FALSE),
    (12, 'multiple-roles@elitea.example', FALSE),
    (13, 'system_user_1@centry.user', FALSE),
    (14, 'suspended@elitea.example', TRUE),
    (15, 'wrong-permission@elitea.example', FALSE),
    (16, 'system@centry.user', FALSE),
    (17, NULL, FALSE),
    (18, 'project-three-member@elitea.example', FALSE),
    (19, 'project-two-member@elitea.example', FALSE);

CREATE TABLE public.auth_core__role (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    mode TEXT NOT NULL
);
CREATE TABLE public.auth_core__role_permission (
    role_id INTEGER NOT NULL,
    permission TEXT NOT NULL
);
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
    role_id INTEGER NOT NULL,
    UNIQUE (project_id, user_id, role_id)
);

INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES
    (101, 1, 'viewer'),
    (102, 1, 'editor'),
    (103, 1, 'without-project-context'),
    (201, 2, 'viewer'),
    (301, 3, 'viewer'),
    (401, 4, 'viewer');
INSERT INTO public.auth_core__project_role_permission (
    project_id, role_id, permission
) VALUES
    (1, 101, 'models.project_context.view'),
    (1, 102, 'models.project_context.view'),
    (2, 201, 'models.project_context.view'),
    (3, 301, 'models.project_context.view'),
    (4, 401, 'models.project_context.view');
INSERT INTO public.auth_core__project_user_role (
    project_id, user_id, role_id
) VALUES
    (1, 11, 101),
    (1, 12, 101),
    (1, 12, 102),
    (1, 13, 101),
    (1, 14, 101),
    (1, 15, 103),
    (1, 16, 101),
    (1, 17, 101),
    (2, 19, 201),
    (3, 11, 301),
    (3, 18, 301),
    (4, 11, 401);

CREATE SCHEMA p_1;
CREATE SCHEMA p_2;
CREATE SCHEMA p_3;
CREATE SCHEMA p_4;
CREATE TABLE p_1.configuration (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL,
    type TEXT NOT NULL,
    elitea_title TEXT NOT NULL,
    data JSONB NOT NULL
);
CREATE TABLE p_2.configuration (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL,
    type TEXT NOT NULL,
    elitea_title TEXT NOT NULL,
    data JSONB NOT NULL
);
CREATE TABLE p_3.configuration (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL,
    type TEXT NOT NULL,
    elitea_title TEXT NOT NULL,
    data JSONB NOT NULL
);
CREATE TABLE p_4.configuration (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL,
    type TEXT NOT NULL,
    elitea_title TEXT NOT NULL,
    data JSONB NOT NULL
);
INSERT INTO p_1.configuration (id, project_id, type, elitea_title, data)
VALUES (
    1,
    1,
    'project_icon',
    'project_icon_1',
    '{"icon_meta":{"url":"/project-icons/one.svg","type":"image/svg+xml"}}'
);
INSERT INTO p_2.configuration (id, project_id, type, elitea_title, data)
VALUES (
    1,
    2,
    'project_icon',
    'project_icon_2',
    '{"icon_meta":{"url":"tenant-two-canary"}}'
);
INSERT INTO p_4.configuration (id, project_id, type, elitea_title, data)
VALUES (
    1,
    4,
    'project_icon',
    'project_icon_4',
    '{"icon_meta":{"url":"suspended-project-canary"}}'
);`); err != nil {
		t.Fatalf("prepare current project-info projects: %v", err)
	}
}
