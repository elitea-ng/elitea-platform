package scim_test

// A SCIM deprovisioning of the last administrator (ADR-0059 R0.11, issue
// 2500), against a database built from the real corpus (001_initial.sql plus
// every ledgered shared migration). The deactivation is refused with a SCIM
// error, status 409; the account stays active with its roles; its session
// cookie and its token are refused at the next request; and one critical
// security audit event is written.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/scim"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/scimdirectory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	corpusDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"
	corpusBootstrap      = "../../infra/db/migrations/001_initial.sql"
)

var (
	corpusTemplateOnce sync.Once
	corpusTemplate     string
	corpusTemplateErr  error
	corpusDBSeq        atomic.Uint64
)

func newCorpusPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(corpusDatabaseURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", corpusDatabaseURLEnv)
	}
	ctx, cancel := dbtest.BuildContext(context.Background())
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	corpusTemplateOnce.Do(func() {
		bootstrap, err := os.ReadFile(corpusBootstrap)
		if err != nil {
			corpusTemplateErr = err
			return
		}
		corpusTemplate, corpusTemplateErr = dbtest.EnsureTemplate(ctx, adminPool, dbtest.Spec{
			Files: platformmigrations.Files, Seed: string(bootstrap),
		})
	})
	if corpusTemplateErr != nil {
		t.Fatalf("build the corpus template: %v", corpusTemplateErr)
	}
	name := fmt.Sprintf("elitea_scim_last_admin_%d_%d_%d", os.Getpid(), time.Now().UnixNano(), corpusDBSeq.Add(1))
	if err := dbtest.CreateFromTemplate(ctx, adminPool, corpusTemplate, name); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", corpusDatabaseURLEnv, err)
	}
	parsed.Path = "/" + name
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dropCancel()
		dropPool, err := pgxpool.New(dropCtx, databaseURL)
		if err != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}

// lastAdministrator makes the deployment's only holder of super_admin, with a
// live browser session and a personal access token. It returns the user id,
// the session cookie value and the token uuid.
func lastAdministrator(t *testing.T, pool *pgxpool.Pool, sessions *browsersession.Manager) (int, string, string) {
	t.Helper()
	ctx := t.Context()
	var id int
	if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__user (email, name) VALUES ('r011-scim-root@corp.example', 'Root') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("create the administrator: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE mode = 'administration' AND name = 'super_admin'`, id); err != nil {
		t.Fatalf("give super_admin: %v", err)
	}
	cookie, err := sessions.Create(ctx, browsersession.NewSession{
		UserID: int64(id), Email: "r011-scim-root@corp.example", Provider: "oidc",
	})
	if err != nil {
		t.Fatalf("create the browser session: %v", err)
	}
	token := uuid.NewString()
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__token (uuid, user_id, name) VALUES ($1, $2, 'ci')`, token, id); err != nil {
		t.Fatalf("create the token: %v", err)
	}
	return id, cookie, token
}

func scimPatch(t *testing.T, router http.Handler, id int, operations ...map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"schemas":    []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
		"Operations": operations,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/Users/%d", id), bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/scim+json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func deprovisionEvents(t *testing.T, pool *pgxpool.Pool, userID int) []map[string]any {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT event FROM elitea_runtime.security_audit_outbox
WHERE event->'data'->>'action' = $1 AND event->'data'->>'resource_id' = $2`,
		adminaccess.AccessActionDeprovisionRefused, fmt.Sprint(userID))
	if err != nil {
		t.Fatalf("read the security audit outbox: %v", err)
	}
	events, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		t.Fatalf("read the security audit outbox: %v", err)
	}
	return events
}

// Acceptance (3).
func TestASCIMDeactivationOfTheLastAdministratorEndsTheAccessAndKeepsTheAccount(t *testing.T) {
	pool := newCorpusPool(t)
	store, err := browsersession.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := browsersession.NewManager(store, browsersession.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	id, cookie, token := lastAdministrator(t, pool, sessions)
	if _, err := sessions.Validate(t.Context(), cookie); err != nil {
		t.Fatalf("fixture: the session must be live before the request: %v", err)
	}
	router := scim.NewHandler(scimdirectory.NewStore(pool)).Routes()

	recorder := scimPatch(t, router, id, map[string]any{"op": "replace", "path": "active", "value": false})

	// A SCIM error body with status 409, naming the operator command.
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	schemas, _ := body["schemas"].([]any)
	if len(schemas) != 1 || schemas[0] != "urn:ietf:params:scim:api:messages:2.0:Error" || body["status"] != "409" {
		t.Fatalf("not a SCIM error with status 409: %s", recorder.Body.String())
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, adminaccess.SuccessorCommand) {
		t.Fatalf("the detail must name %q: %q", adminaccess.SuccessorCommand, detail)
	}

	// The account stays active, with its roles.
	var suspended bool
	if err := pool.QueryRow(t.Context(),
		`SELECT suspended FROM public.auth_core__user WHERE id = $1`, id).Scan(&suspended); err != nil || suspended {
		t.Fatalf("the account must stay active (suspended %v, err %v)", suspended, err)
	}
	if counts, err := adminaccess.CountAdministrators(t.Context(), pool); err != nil || counts.SuperAdmins != 1 {
		t.Fatalf("the account must keep super_admin: %+v, err %v", counts, err)
	}

	// Its session cookie and its token are refused at the next request.
	if _, err := sessions.Validate(t.Context(), cookie); !errors.Is(err, browsersession.ErrRevoked) {
		t.Fatalf("the session cookie must be refused as revoked, got %v", err)
	}
	if _, err := sqlcgen.New(pool).GetActivePATPrincipalByUUID(t.Context(), token); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the token must be refused, got %v", err)
	}

	// One critical event.
	events := deprovisionEvents(t, pool, id)
	if len(events) != 1 {
		t.Fatalf("got %d deprovision events, want 1: %v", len(events), events)
	}
	encoded, _ := json.Marshal(events[0])
	if !strings.Contains(string(encoded), "severity:critical") {
		t.Fatalf("the event must be critical: %s", encoded)
	}

	// SCIM DELETE is a deactivation, and is refused the same way (a second
	// event: each refusal ended the access again).
	request := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/Users/%d", id), nil)
	deleted := httptest.NewRecorder()
	router.ServeHTTP(deleted, request)
	if deleted.Code != http.StatusConflict {
		t.Fatalf("DELETE status = %d, want 409 (body %s)", deleted.Code, deleted.Body.String())
	}
}

// With a second administrator, deprovisioning one of them is refused by the
// older rule (a directory never suspends an administrator: 403 mutability),
// and ends nothing: the last-administrator branch is for the last one only.
func TestASCIMDeactivationOfAnotherAdministratorKeepsTheProtectedRefusal(t *testing.T) {
	pool := newCorpusPool(t)
	store, err := browsersession.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := browsersession.NewManager(store, browsersession.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	id, cookie, _ := lastAdministrator(t, pool, sessions)
	if _, err := pool.Exec(t.Context(), `
WITH second AS (
    INSERT INTO public.auth_core__user (email, name) VALUES ('r011-scim-second@corp.example', 'Second') RETURNING id
)
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT second.id, role.id FROM second, public.auth_core__role role
WHERE role.mode = 'administration' AND role.name = 'super_admin'`); err != nil {
		t.Fatalf("create a second administrator: %v", err)
	}
	router := scim.NewHandler(scimdirectory.NewStore(pool)).Routes()

	recorder := scimPatch(t, router, id, map[string]any{"op": "replace", "path": "active", "value": false})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", recorder.Code, recorder.Body.String())
	}
	if _, err := sessions.Validate(t.Context(), cookie); err != nil {
		t.Fatalf("the protected refusal ends nothing: %v", err)
	}
	if events := deprovisionEvents(t, pool, id); len(events) != 0 {
		t.Fatalf("no deprovision event for an administrator who is not the last: %v", events)
	}
}
