package social_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	platformapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	socialapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentAuthorsProductionHTTPPostgresParityAndNegativeSecurity(t *testing.T) {
	pool := newCurrentAuthorsPostgresPool(t)
	prepareCurrentAuthorsDatabase(t, pool)

	repository, err := dbrepos.NewCurrentSocialAuthorsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, err := socialapp.NewCurrentAuthorsService(repository)
	if err != nil {
		t.Fatal(err)
	}
	route, err := handler.NewCurrentAuthorsRoute(
		service,
		apimw.AuthConfig{
			PrincipalValidator: authsvc.NewPrincipalValidator(pool),
			ForwardedIdentityVerifier: currentAuthorsIntegrationPeerVerifier{
				trustedRemote: "10.0.0.8:43120",
			},
		},
		legacyrbac.NewPostgresResolver(pool),
	)
	if err != nil {
		t.Fatal(err)
	}
	router := platformapi.NewRouter(platformapi.RouterConfig{
		CurrentSocialAuthors: route,
	})

	const wantProjectSeven = `[{"id":11,"email":"admin@elitea.example","name":"Admin","last_login":"Mon, 27 Jul 2026 11:12:13 GMT","suspended":false,"avatar":"avatar-admin"},{"id":12,"email":"editor@elitea.example","name":"Editor","last_login":null,"suspended":false,"avatar":null},{"id":13,"email":"viewer@elitea.example","name":"Viewer","last_login":null,"suspended":false,"avatar":"avatar-viewer"},{"id":14,"email":"wrong-permission@elitea.example","name":"Wrong Permission","last_login":null,"suspended":false,"avatar":null},{"id":15,"email":"suspended@elitea.example","name":"Suspended Member","last_login":null,"suspended":true,"avatar":null},{"id":17,"email":null,"name":null,"last_login":null,"suspended":false,"avatar":null}]` + "\n"
	for _, caller := range []struct {
		role   string
		userID string
	}{
		{role: "admin", userID: "11"},
		{role: "editor", userID: "12"},
		{role: "viewer", userID: "13"},
	} {
		t.Run(caller.role+" role receives exact current array", func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, currentAuthorsIntegrationRequest(
				"/api/v2/social/authors/default/7?limit=5&sort_by=name",
				caller.userID,
				"10.0.0.8:43120",
			))
			if response.Code != http.StatusOK || response.Body.String() != wantProjectSeven {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}

	t.Run("direct alias and tenant isolation", func(t *testing.T) {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, currentAuthorsIntegrationRequest(
			"/api/v2/social/authors/8",
			"18",
			"10.0.0.8:43120",
		))
		const want = `[{"id":18,"email":"tenant-eight-canary@elitea.example","name":"Tenant Eight","last_login":null,"suspended":false,"avatar":"tenant-eight-avatar"}]` + "\n"
		if response.Code != http.StatusOK || response.Body.String() != want ||
			strings.Contains(response.Body.String(), "admin@elitea.example") {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})

	for _, test := range []struct {
		name          string
		target        string
		userID        string
		remote        string
		withoutHeader bool
		wantStatus    int
	}{
		{
			name:          "authentication required",
			target:        "/api/v2/social/authors/7",
			remote:        "10.0.0.8:43120",
			withoutHeader: true,
			wantStatus:    http.StatusUnauthorized,
		},
		{
			name:       "untrusted forwarded identity",
			target:     "/api/v2/social/authors/7",
			userID:     "11",
			remote:     "192.0.2.9:443",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong permission",
			target:     "/api/v2/social/authors/7",
			userID:     "14",
			remote:     "10.0.0.8:43120",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "suspended principal",
			target:     "/api/v2/social/authors/7",
			userID:     "15",
			remote:     "10.0.0.8:43120",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "cross-project membership",
			target:     "/api/v2/social/authors/8",
			userID:     "11",
			remote:     "10.0.0.8:43120",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "suspended project",
			target:     "/api/v2/social/authors/9",
			userID:     "19",
			remote:     "10.0.0.8:43120",
			wantStatus: http.StatusForbidden,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := currentAuthorsIntegrationRequest(test.target, test.userID, test.remote)
			if test.withoutHeader {
				request.Header.Del("X-Auth-Type")
				request.Header.Del("X-Auth-ID")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf(
					"status=%d want=%d body=%q",
					response.Code,
					test.wantStatus,
					response.Body.String(),
				)
			}
			for _, private := range []string{
				handler.CurrentAuthorsPermission,
				"tenant-eight-canary",
				"suspended-project-canary",
			} {
				if strings.Contains(response.Body.String(), private) {
					t.Fatalf("denial leaked %q in %q", private, response.Body.String())
				}
			}
		})
	}

	t.Run("authorization completes before social query", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(
			ctx,
			`ALTER TABLE centry.social_users RENAME TO social_users_unavailable`,
		); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := pool.Exec(
				context.Background(),
				`ALTER TABLE centry.social_users_unavailable RENAME TO social_users`,
			); err != nil {
				t.Errorf("restore social users table: %v", err)
			}
		}()

		response := httptest.NewRecorder()
		router.ServeHTTP(response, currentAuthorsIntegrationRequest(
			"/api/v2/social/authors/7",
			"14",
			"10.0.0.8:43120",
		))
		if response.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})
}

type currentAuthorsIntegrationPeerVerifier struct {
	trustedRemote string
}

func (verifier currentAuthorsIntegrationPeerVerifier) VerifyForwardedIdentityPeer(
	request *http.Request,
) error {
	if request.RemoteAddr != verifier.trustedRemote {
		return errors.New("untrusted peer")
	}
	return nil
}

func currentAuthorsIntegrationRequest(
	target string,
	userID string,
	remote string,
) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.RemoteAddr = remote
	if userID != "" {
		request.Header.Set("X-Auth-Type", "user")
		request.Header.Set("X-Auth-ID", userID)
	}
	return request
}

func newCurrentAuthorsPostgresPool(t *testing.T) *pgxpool.Pool {
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

	databaseName := fmt.Sprintf("elitea_social_authors_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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
		dropContext, dropCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(
			dropContext,
			"DROP DATABASE "+quotedDatabase+" WITH (FORCE)",
		); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})
	return pool
}

func prepareCurrentAuthorsDatabase(t *testing.T, pool *pgxpool.Pool) {
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
    (7, FALSE),
    (8, FALSE),
    (9, TRUE);

CREATE TABLE public.auth_core__user (
    id INTEGER PRIMARY KEY,
    email TEXT UNIQUE,
    name TEXT,
    last_login TIMESTAMP WITHOUT TIME ZONE,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO public.auth_core__user (id, email, name, last_login, suspended) VALUES
    (11, 'admin@elitea.example', 'Admin', TIMESTAMP '2026-07-27 11:12:13', FALSE),
    (12, 'editor@elitea.example', 'Editor', NULL, FALSE),
    (13, 'viewer@elitea.example', 'Viewer', NULL, FALSE),
    (14, 'wrong-permission@elitea.example', 'Wrong Permission', NULL, FALSE),
    (15, 'suspended@elitea.example', 'Suspended Member', NULL, TRUE),
    (16, 'system_user_7@centry.user', 'Project System', NULL, FALSE),
    (17, NULL, NULL, NULL, FALSE),
    (18, 'tenant-eight-canary@elitea.example', 'Tenant Eight', NULL, FALSE),
    (19, 'suspended-project-canary@elitea.example', 'Suspended Project', NULL, FALSE);

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
    id SERIAL PRIMARY KEY,
    project_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    role_id INTEGER NOT NULL,
    UNIQUE (project_id, user_id, role_id)
);
CREATE INDEX ix_auth_core__project_user_role_project_id
    ON public.auth_core__project_user_role (project_id);

INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES
    (101, 7, 'admin'),
    (102, 7, 'editor'),
    (103, 7, 'viewer'),
    (104, 7, 'without-author-permission'),
    (201, 8, 'admin'),
    (301, 9, 'admin');
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission) VALUES
    (7, 101, 'models.social.authors.get'),
    (7, 102, 'models.social.authors.get'),
    (7, 103, 'models.social.authors.get'),
    (7, 104, 'models.social.feedbacks.create'),
    (8, 201, 'models.social.authors.get'),
    (9, 301, 'models.social.authors.get');
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES
    (7, 11, 101),
    (7, 11, 102),
    (7, 12, 102),
    (7, 13, 103),
    (7, 14, 104),
    (7, 15, 103),
    (7, 16, 103),
    (7, 17, 103),
    (8, 18, 201),
    (9, 19, 301);

CREATE TABLE centry.social_users (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL UNIQUE,
    avatar VARCHAR
);
INSERT INTO centry.social_users (user_id, avatar) VALUES
    (11, 'avatar-admin'),
    (13, 'avatar-viewer'),
    (16, 'system-avatar'),
    (18, 'tenant-eight-avatar'),
    (99, 'social-only-canary');`); err != nil {
		t.Fatalf("prepare current Social authors database: %v", err)
	}
}
