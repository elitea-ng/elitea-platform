package indextypes_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	platformapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indextypes"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentIndexTypesHTTPPostgresRBACAndTenantMatrix(t *testing.T) {
	pool := newCurrentIndexTypesPostgresPool(t)
	prepareCurrentIndexTypesRBAC(t, pool)

	snapshot, err := runtimecomposition.LoadPinnedCurrentIndexTypesSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	route, err := handler.NewCurrentIndexTypesRoute(
		snapshot,
		apimw.AuthConfig{
			PrincipalValidator: authsvc.NewPrincipalValidator(pool),
			FormSessions:       authtest.FormSessions{},
		},
		legacyrbac.NewPostgresResolver(pool),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(platformapi.NewRouter(platformapi.RouterConfig{
		CurrentIndexTypes: route,
	}))
	defer server.Close()

	fixture := currentIndexTypesUIFixture(t)

	tests := []struct {
		name       string
		projectID  string
		userID     string
		wantStatus int
	}{
		{
			name:       "project admin",
			projectID:  "1",
			userID:     "17",
			wantStatus: http.StatusOK,
		},
		{
			name:       "project viewer",
			projectID:  "1",
			userID:     "11",
			wantStatus: http.StatusOK,
		},
		{
			name:       "project editor",
			projectID:  "1",
			userID:     "16",
			wantStatus: http.StatusOK,
		},
		{
			name:       "other project viewer",
			projectID:  "2",
			userID:     "12",
			wantStatus: http.StatusOK,
		},
		{
			name:       "cross-project membership",
			projectID:  "2",
			userID:     "11",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "project role without permission",
			projectID:  "1",
			userID:     "13",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "custom role with exact permission",
			projectID:  "1",
			userID:     "18",
			wantStatus: http.StatusOK,
		},
		{
			name:       "nonmember",
			projectID:  "1",
			userID:     "19",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "dual-project member in project one",
			projectID:  "1",
			userID:     "20",
			wantStatus: http.StatusOK,
		},
		{
			name:       "dual-project member in project two",
			projectID:  "2",
			userID:     "20",
			wantStatus: http.StatusOK,
		},
		{
			name:       "suspended principal",
			projectID:  "1",
			userID:     "14",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "platform admin does not inherit project access",
			projectID:  "1",
			userID:     "15",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "suspended project",
			projectID:  "3",
			userID:     "11",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(
				http.MethodGet,
				server.URL+
					"/api/v2/elitea_core/index_types/prompt_lib/"+
					test.projectID,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			authtest.SignIn(request, test.userID)
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
			if test.wantStatus == http.StatusOK {
				// The body carries both halves since #394: the published
				// `items`/`total` envelope beside the three legacy maps. Read
				// the maps back out and hold them to the pinned SDK fixture,
				// and require the published half to be present.
				var legacyMaps handler.CurrentIndexTypes
				if err := json.Unmarshal(body, &legacyMaps); err != nil {
					t.Fatalf("UI response does not decode: %v", err)
				}
				if !reflect.DeepEqual(legacyMaps, fixture) {
					t.Fatalf("UI response drifted: %s", body)
				}
				var published struct {
					Items []map[string]any `json:"items"`
					Total int              `json:"total"`
				}
				if err := json.Unmarshal(body, &published); err != nil {
					t.Fatalf("published half does not decode: %v", err)
				}
				if len(published.Items) != 3 || published.Total != 3 {
					t.Fatalf("published half missing: %s", body)
				}
			}
			if strings.Contains(string(body), "tenant-one-canary") ||
				strings.Contains(string(body), "tenant-two-canary") ||
				strings.Contains(string(body), "suspended-project-canary") {
				t.Fatalf("tenant data leaked: %s", body)
			}
		})
	}
}

func newCurrentIndexTypesPostgresPool(t *testing.T) *pgxpool.Pool {
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

	databaseName := fmt.Sprintf(
		"elitea_index_types_it_%d_%d",
		os.Getpid(),
		time.Now().UnixNano(),
	)
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(
		ctx,
		"CREATE DATABASE "+quotedDatabase,
	); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(
			context.Background(),
			"DROP DATABASE "+quotedDatabase+" WITH (FORCE)",
		)
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = adminPool.Exec(
			context.Background(),
			"DROP DATABASE "+quotedDatabase+" WITH (FORCE)",
		)
		adminPool.Close()
		t.Fatalf("ping isolated PostgreSQL integration database: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(
			context.Background(),
			120*time.Second,
		)
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

func prepareCurrentIndexTypesRBAC(t *testing.T, pool *pgxpool.Pool) {
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
    (16, 'editor-one@example.test', FALSE),
    (17, 'admin-one@example.test', FALSE),
    (18, 'custom-index-types@example.test', FALSE),
    (19, 'nonmember@example.test', FALSE),
    (20, 'dual-project@example.test', FALSE);

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
    (1, 'models.applications.index_types.details');
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
    (104, 1, 'admin'),
    (105, 1, 'custom_index_types_reader'),
    (201, 2, 'viewer'),
    (202, 2, 'editor'),
    (301, 3, 'viewer');
INSERT INTO public.auth_core__project_role_permission (
    project_id, role_id, permission
) VALUES
    (1, 101, 'models.applications.index_types.details'),
    (1, 102, 'models.applications.index_types.details'),
    (1, 103, 'models.applications.details'),
    (1, 104, 'models.applications.index_types.details'),
    (1, 105, 'models.applications.index_types.details'),
    (2, 201, 'models.applications.index_types.details'),
    (2, 202, 'models.applications.index_types.details'),
    (3, 301, 'models.applications.index_types.details');
INSERT INTO public.auth_core__project_user_role (
    project_id, user_id, role_id
) VALUES
    (1, 11, 101),
    (1, 13, 103),
    (1, 14, 101),
    (1, 16, 102),
    (1, 17, 104),
    (1, 18, 105),
    (1, 20, 102),
    (2, 12, 201),
    (2, 20, 202),
    (3, 11, 301);

CREATE SCHEMA p_1;
CREATE SCHEMA p_2;
CREATE SCHEMA p_3;
CREATE TABLE p_1.index_types_canary (value TEXT NOT NULL);
CREATE TABLE p_2.index_types_canary (value TEXT NOT NULL);
CREATE TABLE p_3.index_types_canary (value TEXT NOT NULL);
INSERT INTO p_1.index_types_canary VALUES ('tenant-one-canary');
INSERT INTO p_2.index_types_canary VALUES ('tenant-two-canary');
INSERT INTO p_3.index_types_canary VALUES ('suspended-project-canary');
`); err != nil {
		t.Fatalf("prepare current index-types PostgreSQL fixture: %v", err)
	}
}
