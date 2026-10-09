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

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentFeedbackCreateHTTPPostgresParityAndNegativeSecurity(t *testing.T) {
	pool := newCurrentFeedbackPostgresPool(t)
	prepareCurrentFeedbackDatabase(t, pool)

	repository, err := dbrepos.NewCurrentSocialFeedbacksRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	route, err := handler.NewCurrentFeedbackCreateRoute(
		repository,
		apimw.AuthConfig{
			PrincipalValidator: authsvc.NewPrincipalValidator(pool),
			ForwardedIdentityVerifier: currentFeedbackIntegrationPeerVerifier{
				trustedRemote: "10.0.0.8:43120",
			},
		},
		legacyrbac.NewPostgresResolver(pool),
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		target     string
		authType   string
		authID     string
		userID     string
		body       string
		referrer   string
		userAgent  string
		wantStatus int
		wantBody   string
	}{
		{
			name:     "canonical default alias and PAT owner authority",
			target:   "/api/v2/social/feedbacks/default/7",
			authType: "token",
			authID:   "91",
			userID:   "41",
			body: `{"description":"admin feedback","rating":5,"location":"/app/chat",` +
				`"user_id":999,"referrer":"https://attacker.invalid/body",` +
				`"user_agent":"forged"}`,
			referrer:   "https://elitea.example/app/chat",
			userAgent:  "EliteaUI/current",
			wantStatus: http.StatusCreated,
			wantBody:   "{\"id\":1}\n",
		},
		{
			name:       "implicit default alias and editor role",
			target:     "/api/v2/social/feedbacks/7",
			authType:   "user",
			authID:     "42",
			userID:     "42",
			body:       `{"description":"editor feedback","rating":4,"referrer":"https://attacker.invalid/body"}`,
			wantStatus: http.StatusCreated,
			wantBody:   "{\"id\":2}\n",
		},
		{
			name:       "viewer role and baseline empty description",
			target:     "/api/v2/social/feedbacks/default/7",
			authType:   "user",
			authID:     "43",
			userID:     "43",
			body:       `{"description":"","rating":0}`,
			wantStatus: http.StatusCreated,
			wantBody:   "{\"id\":3}\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentFeedbackIntegrationRequest(
				test.target,
				test.authType,
				test.authID,
				test.userID,
				test.body,
				"10.0.0.8:43120",
				test.referrer,
				test.userAgent,
			))
			if response.Code != test.wantStatus || response.Body.String() != test.wantBody ||
				response.Header().Get("Content-Type") != "application/json" {
				t.Fatalf(
					"status=%d content_type=%q body=%q",
					response.Code,
					response.Header().Get("Content-Type"),
					response.Body.String(),
				)
			}
		})
	}

	assertCurrentFeedbackRows(t, pool)
	assertCurrentFeedbackSharedTableShape(t, pool)

	for _, test := range []struct {
		name       string
		target     string
		authType   string
		authID     string
		userID     string
		remote     string
		wantStatus int
	}{
		{
			name:       "authentication required",
			target:     "/api/v2/social/feedbacks/default/7",
			remote:     "10.0.0.8:43120",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "untrusted forwarded identity",
			target:     "/api/v2/social/feedbacks/default/7",
			authType:   "user",
			authID:     "41",
			userID:     "41",
			remote:     "192.0.2.9:443",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong permission",
			target:     "/api/v2/social/feedbacks/default/7",
			authType:   "user",
			authID:     "44",
			userID:     "44",
			remote:     "10.0.0.8:43120",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "suspended principal",
			target:     "/api/v2/social/feedbacks/default/7",
			authType:   "user",
			authID:     "45",
			userID:     "45",
			remote:     "10.0.0.8:43120",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "cross-project membership",
			target:     "/api/v2/social/feedbacks/default/8",
			authType:   "user",
			authID:     "41",
			userID:     "41",
			remote:     "10.0.0.8:43120",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "suspended project",
			target:     "/api/v2/social/feedbacks/default/9",
			authType:   "user",
			authID:     "49",
			userID:     "49",
			remote:     "10.0.0.8:43120",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "non-canonical project identifier",
			target:     "/api/v2/social/feedbacks/default/007",
			authType:   "user",
			authID:     "41",
			userID:     "41",
			remote:     "10.0.0.8:43120",
			wantStatus: http.StatusForbidden,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &currentFeedbackTrackingBody{
				Reader: strings.NewReader(`{"description":"private body canary","rating":5}`),
			}
			request := currentFeedbackIntegrationRequest(
				test.target,
				test.authType,
				test.authID,
				test.userID,
				"",
				test.remote,
				"",
				"",
			)
			request.Body = body
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf(
					"status=%d want=%d body=%q",
					response.Code,
					test.wantStatus,
					response.Body.String(),
				)
			}
			if body.read {
				t.Fatal("denied request body was read")
			}
			for _, private := range []string{
				handler.CurrentFeedbackCreatePermission,
				"private body canary",
				"database",
			} {
				if strings.Contains(response.Body.String(), private) {
					t.Fatalf("denial leaked %q in %q", private, response.Body.String())
				}
			}
		})
	}

	t.Run("statement transaction failure is safe and has no partial row", func(t *testing.T) {
		var before int
		if err := pool.QueryRow(
			context.Background(),
			`SELECT COUNT(*)::integer FROM centry.social_feedbacks`,
		).Scan(&before); err != nil {
			t.Fatal(err)
		}

		response := httptest.NewRecorder()
		route.ServeHTTP(response, currentFeedbackIntegrationRequest(
			"/api/v2/social/feedbacks/default/7",
			"user",
			"41",
			"41",
			`{"description":"reject-current-feedback","rating":5}`,
			"10.0.0.8:43120",
			"",
			"",
		))
		if response.Code != http.StatusInternalServerError ||
			response.Body.String() != "{\"error\":\"internal server error\"}\n" ||
			strings.Contains(response.Body.String(), "feedback_rejected_for_test") {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}

		var after int
		if err := pool.QueryRow(
			context.Background(),
			`SELECT COUNT(*)::integer FROM centry.social_feedbacks`,
		).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatalf("feedback count changed across failed insert: before=%d after=%d", before, after)
		}
	})
}

type currentFeedbackIntegrationPeerVerifier struct {
	trustedRemote string
}

func (verifier currentFeedbackIntegrationPeerVerifier) VerifyForwardedIdentityPeer(
	request *http.Request,
) error {
	if request.RemoteAddr != verifier.trustedRemote {
		return errors.New("untrusted peer")
	}
	return nil
}

func currentFeedbackIntegrationRequest(
	target string,
	authType string,
	authID string,
	userID string,
	body string,
	remote string,
	referrer string,
	userAgent string,
) *http.Request {
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.RemoteAddr = remote
	request.Header.Set("Content-Type", "application/json")
	if authID != "" {
		request.Header.Set("X-Auth-Type", authType)
		request.Header.Set("X-Auth-ID", authID)
		if authType == "token" {
			request.Header.Set("X-Auth-User-ID", userID)
		}
	}
	if referrer != "" {
		request.Header.Set("Referer", referrer)
	}
	if userAgent != "" {
		request.Header.Set("User-Agent", userAgent)
	}
	return request
}

func assertCurrentFeedbackRows(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT user_id, referrer, description, rating, user_agent
FROM centry.social_feedbacks
ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	type feedbackRow struct {
		userID      int64
		referrer    *string
		description string
		rating      int
		userAgent   *string
	}
	got := make([]feedbackRow, 0, 3)
	for rows.Next() {
		var row feedbackRow
		if err := rows.Scan(
			&row.userID,
			&row.referrer,
			&row.description,
			&row.rating,
			&row.userAgent,
		); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("feedback rows=%#v", got)
	}
	if got[0].userID != 41 ||
		got[0].referrer == nil ||
		*got[0].referrer != "https://elitea.example/app/chat" ||
		got[0].description != "admin feedback" ||
		got[0].rating != 5 ||
		got[0].userAgent == nil ||
		*got[0].userAgent != "EliteaUI/current" {
		t.Fatalf("admin feedback=%#v", got[0])
	}
	if got[1].userID != 42 ||
		got[1].referrer != nil ||
		got[1].description != "editor feedback" ||
		got[1].rating != 4 ||
		got[1].userAgent == nil ||
		*got[1].userAgent != "" {
		t.Fatalf("editor feedback=%#v", got[1])
	}
	if got[2].userID != 43 ||
		got[2].referrer != nil ||
		got[2].description != "" ||
		got[2].rating != 0 ||
		got[2].userAgent == nil ||
		*got[2].userAgent != "" {
		t.Fatalf("viewer feedback=%#v", got[2])
	}
}

func assertCurrentFeedbackSharedTableShape(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var projectColumns int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*)::integer
FROM information_schema.columns
WHERE table_schema = 'centry'
  AND table_name = 'social_feedbacks'
  AND column_name = 'project_id'`).Scan(&projectColumns); err != nil {
		t.Fatal(err)
	}
	if projectColumns != 0 {
		t.Fatalf("shared feedback table unexpectedly has %d project_id columns", projectColumns)
	}
}

func newCurrentFeedbackPostgresPool(t *testing.T) *pgxpool.Pool {
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

	databaseName := fmt.Sprintf("elitea_social_feedback_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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

func prepareCurrentFeedbackDatabase(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id INTEGER PRIMARY KEY,
    suspended BOOLEAN NOT NULL DEFAULT FALSE,
    create_success BOOLEAN NOT NULL DEFAULT TRUE
);
-- The token principal reload joins the project binding (shared migration 0071).
CREATE SCHEMA IF NOT EXISTS elitea_identity;
CREATE TABLE elitea_identity.token_project_binding (
    token_id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL
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
    (41, 'admin@elitea.example', 'Admin', NULL, FALSE),
    (42, 'editor@elitea.example', 'Editor', NULL, FALSE),
    (43, 'viewer@elitea.example', 'Viewer', NULL, FALSE),
    (44, 'wrong-permission@elitea.example', 'Wrong Permission', NULL, FALSE),
    (45, 'suspended@elitea.example', 'Suspended', NULL, TRUE),
    (48, 'tenant-eight@elitea.example', 'Tenant Eight', NULL, FALSE),
    (49, 'suspended-project@elitea.example', 'Suspended Project', NULL, FALSE);

CREATE TABLE public.auth_core__token (
    id INTEGER PRIMARY KEY,
    uuid VARCHAR(36),
    expires TIMESTAMP WITHOUT TIME ZONE,
    user_id INTEGER NOT NULL REFERENCES public.auth_core__user(id),
    name TEXT
);
INSERT INTO public.auth_core__token (id, uuid, expires, user_id, name)
VALUES (91, 'feedback-token-91', NULL, 41, 'feedback-integration');

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
    (104, 7, 'without-feedback-permission'),
    (105, 7, 'suspended-member'),
    (201, 8, 'admin'),
    (301, 9, 'admin');
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission) VALUES
    (7, 101, 'models.social.feedbacks.create'),
    (7, 102, 'models.social.feedbacks.create'),
    (7, 103, 'models.social.feedbacks.create'),
    (7, 104, 'models.social.authors.get'),
    (7, 105, 'models.social.feedbacks.create'),
    (8, 201, 'models.social.feedbacks.create'),
    (9, 301, 'models.social.feedbacks.create');
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES
    (7, 41, 101),
    (7, 42, 102),
    (7, 43, 103),
    (7, 44, 104),
    (7, 45, 105),
    (8, 48, 201),
    (9, 49, 301);

CREATE TABLE centry.social_feedbacks (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL,
    referrer VARCHAR,
    description TEXT NOT NULL
        CHECK (description <> 'reject-current-feedback'),
    rating INTEGER NOT NULL,
    user_agent VARCHAR,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT now()
);`); err != nil {
		t.Fatalf("prepare current Social feedback database: %v", err)
	}
}
