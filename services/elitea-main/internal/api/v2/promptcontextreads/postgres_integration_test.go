package promptcontextreads_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/promptcontextreads"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	promptContextPythonMasterKey  = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	promptContextPythonProjectKey = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
	promptContextPythonWrappedKey = "gAAAAABlU_EAoKGio6SlpqeoqaqrrK2uryHMXcX6u3HizkFeKnBIPXqOzdaW4oCAofma6bzJk8y2Ei0rbhRDFYgh-0veP4OgLAC1Vi8Jba2ulGhC4bQrzwA4rrjMrzA3m8X5wInwskE6"
	promptContextPythonVaultToken = "gAAAAABlU_EBsLGys7S1tre4ubq7vL2-vx1XMkCj-NAPVrz2qJjob7g8g2X5uZRKHkqRYf3PrTLUC8Q1IHnCMja09Xr6VixBNDJNqcJhTDidsE3D9XlcDpLfJ6e5zNz6DsTP67crLz-PvCJO0qwoNSpc2vwiLlTkf2xnyvlVOAMXlrmueSNrVxUoOGRzpK_fci7UQqhXtn2DDrjEgHLzW77baCUbY6nqH4w48HOBwzsCN7Y6dpkZkns7IK5pFKZs4WwYxYbAU6Q0"
)

func TestCurrentPromptContextReadsPostgresHTTPRBACAndTenantIsolation(t *testing.T) {
	pool := newPromptContextPostgresPool(t)
	preparePromptContextPostgres(t, pool)

	vaults, err := storage.NewPostgresSecretVaultLoader(
		pool,
		[]byte(promptContextPythonMasterKey),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer vaults.Destroy()
	chat, err := handler.NewCurrentChatConfigVaultReader(vaults)
	if err != nil {
		t.Fatal(err)
	}
	projectContext, err := handler.NewCurrentProjectContextRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := handler.NewCurrentRoutes(
		chat,
		projectContext,
		apimw.AuthConfig{
			PrincipalValidator: authsvc.NewPrincipalValidator(pool),
			ForwardedIdentityVerifier: promptContextPeerVerifierFunc(
				func(request *http.Request) error {
					host, _, splitErr := net.SplitHostPort(request.RemoteAddr)
					if splitErr == nil && (host == "127.0.0.1" || host == "::1") {
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
	// routes (CurrentRoutes) is exercised directly, not through
	// platformapi.NewRouter: its own ServeHTTP already mounts both the
	// chat-config and project-context GETs against its own auth/RBAC — see
	// CURRENT_PARITY_EVIDENCE.md. The router every real deployment reaches
	// mounts CurrentPromptContextReads' chat-config GET only; project-context
	// stays on the production router's own coreHandler.ProjectContext (a
	// different, prototype-stub implementation with a different default).
	// Routing this test through platformapi.NewRouter would test that
	// unrelated handler's behavior under the project-context path, not
	// CurrentPromptContextReads' own contract this test is actually about.
	server := httptest.NewServer(routes)
	defer server.Close()

	for _, test := range []struct {
		name       string
		path       string
		userID     string
		tokenID    string
		wantStatus int
		wantBody   string
		forbidBody string
	}{
		{
			name:       "chat values use regular hidden admin and defaults",
			path:       "/api/v2/elitea_core/chat_config/prompt_lib/1",
			userID:     "11",
			wantStatus: http.StatusOK,
			wantBody:   `{"chat_max_upload_count":70,"chat_max_upload_size_mb":250,"chat_max_file_upload_size_mb":175,"chat_max_image_upload_count":12,"chat_max_image_upload_size_mb":3}`,
			forbidBody: "999",
		},
		{
			name:       "active PAT uses the same project RBAC",
			path:       "/api/v2/elitea_core/chat_config/prompt_lib/1",
			userID:     "11",
			tokenID:    "501",
			wantStatus: http.StatusOK,
			wantBody:   `{"chat_max_upload_count":70,"chat_max_upload_size_mb":250,"chat_max_file_upload_size_mb":175,"chat_max_image_upload_count":12,"chat_max_image_upload_size_mb":3}`,
		},
		{
			name:       "active PAT reads project context through the same RBAC",
			path:       "/api/v2/elitea_core/project_context/prompt_lib/1/project-context",
			userID:     "11",
			tokenID:    "501",
			wantStatus: http.StatusOK,
			wantBody:   `{"id":41,"content":"Project one context","enabled":false,"updated_at":"2026-07-27T13:14:15.120000"}`,
		},
		{
			name:       "project context exact stored projection",
			path:       "/api/v2/elitea_core/project_context/prompt_lib/1/project-context",
			userID:     "11",
			wantStatus: http.StatusOK,
			wantBody:   `{"id":41,"content":"Project one context","enabled":false,"updated_at":"2026-07-27T13:14:15.120000"}`,
			forbidBody: "tenant-two-canary",
		},
		{
			name:       "missing project context defaults",
			path:       "/api/v2/elitea_core/project_context/prompt_lib/2/project-context",
			userID:     "12",
			wantStatus: http.StatusOK,
			wantBody:   `{"id":null,"content":"","enabled":true,"updated_at":null}`,
			forbidBody: "Project one context",
		},
		{
			name:       "project two chat reads its own vault",
			path:       "/api/v2/elitea_core/chat_config/prompt_lib/2",
			userID:     "12",
			wantStatus: http.StatusOK,
			wantBody:   `{"chat_max_upload_count":999,"chat_max_upload_size_mb":250,"chat_max_file_upload_size_mb":175,"chat_max_image_upload_count":10,"chat_max_image_upload_size_mb":3}`,
			forbidBody: "70",
		},
		{
			name:       "cross tenant denied before project two data",
			path:       "/api/v2/elitea_core/chat_config/prompt_lib/2",
			userID:     "11",
			wantStatus: http.StatusForbidden,
			forbidBody: "999",
		},
		{
			name:       "cross tenant denied before project two context",
			path:       "/api/v2/elitea_core/project_context/prompt_lib/2/project-context",
			userID:     "11",
			wantStatus: http.StatusForbidden,
			forbidBody: "tenant-two-canary",
		},
		{
			name:       "wrong chat permission",
			path:       "/api/v2/elitea_core/chat_config/prompt_lib/1",
			userID:     "13",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "wrong project context permission",
			path:       "/api/v2/elitea_core/project_context/prompt_lib/1/project-context",
			userID:     "16",
			wantStatus: http.StatusForbidden,
			forbidBody: "Project one context",
		},
		{
			name:       "central role name fallback grants chat endpoint",
			path:       "/api/v2/elitea_core/chat_config/prompt_lib/4",
			userID:     "15",
			wantStatus: http.StatusOK,
			wantBody:   `{"chat_max_upload_count":44,"chat_max_upload_size_mb":250,"chat_max_file_upload_size_mb":175,"chat_max_image_upload_count":10,"chat_max_image_upload_size_mb":3}`,
		},
		{
			name:       "central role name fallback grants project context PAT",
			path:       "/api/v2/elitea_core/project_context/prompt_lib/4/project-context",
			userID:     "15",
			tokenID:    "502",
			wantStatus: http.StatusOK,
			wantBody:   `{"id":44,"content":"Central fallback context","enabled":true,"updated_at":"2026-07-27T13:14:15"}`,
		},
		{
			name:       "suspended principal",
			path:       "/api/v2/elitea_core/project_context/prompt_lib/1/project-context",
			userID:     "14",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "suspended project",
			path:       "/api/v2/elitea_core/project_context/prompt_lib/3/project-context",
			userID:     "11",
			wantStatus: http.StatusForbidden,
			forbidBody: "suspended-project-canary",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, server.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.tokenID == "" {
				request.Header.Set("X-Auth-Type", "user")
				request.Header.Set("X-Auth-ID", test.userID)
			} else {
				request.Header.Set("X-Auth-Type", "token")
				request.Header.Set("X-Auth-ID", test.tokenID)
				request.Header.Set("X-Auth-User-ID", test.userID)
			}
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
				t.Fatalf("status=%d want=%d body=%s", response.StatusCode, test.wantStatus, body)
			}
			if test.wantBody != "" && strings.TrimSpace(string(body)) != test.wantBody {
				t.Fatalf("body=%q want=%q", body, test.wantBody)
			}
			if test.forbidBody != "" && strings.Contains(string(body), test.forbidBody) {
				t.Fatalf("protected tenant data leaked: %s", body)
			}
		})
	}

	emptyObject := "{}"
	setPromptContextDataFixture(t, pool, &emptyObject)
	status, body := getPromptContextFixture(
		t,
		server.Client(),
		server.URL+"/api/v2/elitea_core/project_context/prompt_lib/1/project-context",
	)
	if status != http.StatusOK ||
		strings.TrimSpace(body) != `{"id":41,"content":"","enabled":true,"updated_at":"2026-07-27T13:14:15.120000"}` {
		t.Fatalf("empty object status=%d body=%q", status, body)
	}

	for _, fixture := range []struct {
		name string
		data *string
	}{
		{name: "SQL NULL"},
		{name: "JSON null", data: promptContextString("null")},
		{name: "false", data: promptContextString("false")},
		{name: "zero", data: promptContextString("0")},
		{name: "string", data: promptContextString(`"context"`)},
		{name: "list", data: promptContextString("[]")},
	} {
		t.Run("persisted non-object "+fixture.name, func(t *testing.T) {
			setPromptContextDataFixture(t, pool, fixture.data)
			status, body := getPromptContextFixture(
				t,
				server.Client(),
				server.URL+"/api/v2/elitea_core/project_context/prompt_lib/1/project-context",
			)
			if status != http.StatusInternalServerError ||
				strings.TrimSpace(body) != `{"message":"Internal Server Error"}` {
				t.Fatalf("status=%d body=%q", status, body)
			}
		})
	}
}

func setPromptContextDataFixture(t *testing.T, pool *pgxpool.Pool, data *string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	if data == nil {
		_, err = pool.Exec(ctx, `UPDATE p_1.configuration SET data = NULL WHERE id = 41`)
	} else {
		_, err = pool.Exec(ctx, `UPDATE p_1.configuration SET data = $1::jsonb WHERE id = 41`, *data)
	}
	if err != nil {
		t.Fatalf("set project-context fixture: %v", err)
	}
}

func getPromptContextFixture(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	response, err := client.Do(request)
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
	return response.StatusCode, string(body)
}

func preparePromptContextPostgres(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	adminToken, err := centrysecrets.RewriteWrapped(
		[]byte(promptContextPythonMasterKey),
		[]byte(promptContextPythonWrappedKey),
		[]byte(promptContextPythonVaultToken),
		[]centrysecrets.Mutation{
			{Collection: centrysecrets.RegularSecrets, Name: "chat_max_upload_count", IntegerValue: promptContextInt64(90)},
			{Collection: centrysecrets.RegularSecrets, Name: "chat_max_upload_size_mb", IntegerValue: promptContextInt64(250)},
			{Collection: centrysecrets.RegularSecrets, Name: "chat_max_file_upload_size_mb", IntegerValue: promptContextInt64(175)},
			{Collection: centrysecrets.HiddenSecrets, Name: "chat_max_image_upload_size_mb", IntegerValue: promptContextInt64(9999)},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	projectOneToken, err := centrysecrets.RewriteUnwrapped(
		[]byte(promptContextPythonProjectKey),
		[]byte(promptContextPythonVaultToken),
		[]centrysecrets.Mutation{
			{Collection: centrysecrets.HiddenSecrets, Name: "chat_max_upload_count", IntegerValue: promptContextInt64(80)},
			{Collection: centrysecrets.RegularSecrets, Name: "chat_max_upload_count", IntegerValue: promptContextInt64(70)},
			{Collection: centrysecrets.HiddenSecrets, Name: "chat_max_image_upload_count", IntegerValue: promptContextInt64(12)},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	projectTwoToken, err := centrysecrets.RewriteUnwrapped(
		[]byte(promptContextPythonProjectKey),
		[]byte(promptContextPythonVaultToken),
		[]centrysecrets.Mutation{
			{Collection: centrysecrets.RegularSecrets, Name: "chat_max_upload_count", IntegerValue: promptContextInt64(999)},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	projectFourToken, err := centrysecrets.RewriteUnwrapped(
		[]byte(promptContextPythonProjectKey),
		[]byte(promptContextPythonVaultToken),
		[]centrysecrets.Mutation{
			{Collection: centrysecrets.RegularSecrets, Name: "chat_max_upload_count", IntegerValue: promptContextInt64(44)},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
    (1, FALSE),
    (2, FALSE),
    (3, TRUE),
    (4, FALSE);

CREATE TABLE public.auth_core__user (
    id INTEGER PRIMARY KEY,
    email TEXT,
    suspended BOOLEAN NOT NULL DEFAULT FALSE,
    last_login TIMESTAMP WITHOUT TIME ZONE
);
INSERT INTO public.auth_core__user (id, email, suspended) VALUES
    (11, 'project-one@elitea.example', FALSE),
    (12, 'project-two@elitea.example', FALSE),
    (13, 'context-only@elitea.example', FALSE),
    (14, 'suspended@elitea.example', TRUE),
    (15, 'central-fallback@elitea.example', FALSE),
    (16, 'chat-only@elitea.example', FALSE);
CREATE TABLE public.auth_core__token (
    id INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL,
    expires TIMESTAMP WITHOUT TIME ZONE
);
INSERT INTO public.auth_core__token (id, user_id, expires)
VALUES
    (501, 11, clock_timestamp() + INTERVAL '1 day'),
    (502, 15, clock_timestamp() + INTERVAL '1 day');

CREATE TABLE public.auth_core__role (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    mode TEXT NOT NULL
);
CREATE TABLE public.auth_core__role_permission (
    role_id INTEGER NOT NULL,
    permission TEXT NOT NULL
);
INSERT INTO public.auth_core__role (id, name, mode)
VALUES (1, 'fallback-viewer', 'default');
INSERT INTO public.auth_core__role_permission (role_id, permission) VALUES
    (1, 'models.chat.conversation.details'),
    (1, 'models.project_context.view');
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
    (102, 1, 'context-only'),
    (103, 1, 'chat-only'),
    (201, 2, 'viewer'),
    (301, 3, 'viewer'),
    (401, 4, 'fallback-viewer');
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission) VALUES
    (1, 101, 'models.chat.conversation.details'),
    (1, 101, 'models.project_context.view'),
    (1, 102, 'models.project_context.view'),
    (1, 103, 'models.chat.conversation.details'),
    (2, 201, 'models.chat.conversation.details'),
    (2, 201, 'models.project_context.view'),
    (3, 301, 'models.project_context.view');
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES
    (1, 11, 101),
    (1, 13, 102),
    (1, 14, 101),
    (1, 16, 103),
    (2, 12, 201),
    (3, 11, 301),
    (4, 15, 401);

CREATE TABLE centry.secrets_key (id TEXT PRIMARY KEY, data BYTEA NOT NULL);
CREATE TABLE centry.secrets_data (id TEXT PRIMARY KEY, data BYTEA NOT NULL);

CREATE SCHEMA p_1;
CREATE SCHEMA p_2;
CREATE SCHEMA p_3;
CREATE SCHEMA p_4;
CREATE TABLE p_1.configuration (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL,
    type TEXT NOT NULL,
    data JSONB,
    updated_at TIMESTAMP WITHOUT TIME ZONE
);
CREATE TABLE p_2.configuration (LIKE p_1.configuration INCLUDING ALL);
CREATE TABLE p_3.configuration (LIKE p_1.configuration INCLUDING ALL);
CREATE TABLE p_4.configuration (LIKE p_1.configuration INCLUDING ALL);
INSERT INTO p_1.configuration (id, project_id, type, data, updated_at)
VALUES (
    41,
    1,
    'project_context',
    '{"content":"Project one context","enabled":"off"}',
    TIMESTAMP '2026-07-27 13:14:15.120000'
);
INSERT INTO p_2.configuration (id, project_id, type, data, updated_at)
VALUES (
    42,
    1,
    'project_context',
    '{"content":"tenant-two-canary","enabled":true}',
    TIMESTAMP '2026-07-27 13:14:15'
);
INSERT INTO p_3.configuration (id, project_id, type, data, updated_at)
VALUES (
    43,
    3,
    'project_context',
    '{"content":"suspended-project-canary","enabled":true}',
    TIMESTAMP '2026-07-27 13:14:15'
);
INSERT INTO p_4.configuration (id, project_id, type, data, updated_at)
VALUES (
    44,
    4,
    'project_context',
    '{"content":"Central fallback context","enabled":true}',
    TIMESTAMP '2026-07-27 13:14:15'
);`); err != nil {
		t.Fatalf("prepare prompt-context PostgreSQL schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.secrets_key (id, data) VALUES
    ('admin', $1),
    ('project-1', $1),
    ('project-2', $1),
    ('project-4', $1)`,
		[]byte(promptContextPythonWrappedKey),
	); err != nil {
		t.Fatalf("prepare prompt-context vault keys: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.secrets_data (id, data) VALUES
    ('admin', $1),
    ('project-1', $2),
    ('project-2', $3),
    ('project-4', $4)`,
		adminToken,
		projectOneToken,
		projectTwoToken,
		projectFourToken,
	); err != nil {
		t.Fatalf("prepare prompt-context vault data: %v", err)
	}
}

func promptContextInt64(value int64) *int64 {
	return &value
}

func promptContextString(value string) *string {
	return &value
}

func newPromptContextPostgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const (
		environment        = "ELITEA_TEST_DATABASE_URL"
		requireEnvironment = "ELITEA_REQUIRE_PROMPT_CONTEXT_POSTGRES_TEST"
	)
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		if os.Getenv(requireEnvironment) == "true" {
			t.Fatalf("%s requires %s", requireEnvironment, environment)
		}
		t.Skipf("set %s to run the PostgreSQL prompt-context read integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.MaxConns = 2
	admin, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	databaseName := fmt.Sprintf("elitea_prompt_context_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		admin.Close()
		t.Fatal(err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropContext, dropCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropContext, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop integration database: %v", err)
		}
		admin.Close()
	})
	return pool
}

type promptContextPeerVerifierFunc func(*http.Request) error

func (function promptContextPeerVerifierFunc) VerifyForwardedIdentityPeer(
	request *http.Request,
) error {
	return function(request)
}
