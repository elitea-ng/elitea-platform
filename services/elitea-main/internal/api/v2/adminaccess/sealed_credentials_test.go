package adminaccess_test

// The sealed credential list of the console (ADR-0059 R2.17, issue 2610).
// The unit half drives the handler behind its route gate with a fake lister;
// the PostgreSQL half issues real credentials, bound and unbound, and holds
// the response to the row's own selector and digest.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	console "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

var sealedListClock = time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)

// fakeSealedLister answers fixed rows and records the actor.
type fakeSealedLister struct {
	rows  []console.ListedSealedCredential
	err   error
	actor int64
	calls int
}

func (f *fakeSealedLister) ListSealedCredentials(_ context.Context, actor int64) ([]console.ListedSealedCredential, error) {
	f.calls++
	f.actor = actor
	return f.rows, f.err
}

// serveSealedList drives GET /access/sealed_credentials behind the gate the
// production router puts in front of it.
func serveSealedList(t *testing.T, handler *console.SealedCredentialsHandler, resolver auth.PermissionResolver, user auth.User) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	handler.Register(r, apimw.RequireCentralPermissions(resolver, auth.PermissionModeAdministration, registry.AccessBreakglassManage))
	req := httptest.NewRequest(http.MethodGet, console.SealedCredentialsPath, nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// sealedListDirectory: user 1 holds a role with access.breakglass.manage,
// user 2 holds admin without it.
func sealedListDirectory() *directory {
	d := newDirectory()
	d.roles["breakglass_manager"] = []string{registry.AccessBreakglassManage}
	d.users[1] = []string{"breakglass_manager"}
	d.users[2] = []string{registry.RoleLLMManager}
	return d
}

func TestTheSealedCredentialListAnswersAHolderAndRefusesEveryoneElse(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	lister := &fakeSealedLister{rows: []console.ListedSealedCredential{{
		SealedCredential: registry.SealedCredential{
			ID: id, Selector: "0123456789abcdef01234567", Label: "envelope A", Level: registry.ElevationLevelSuperAdmin,
			UserID: 9, Split: true, IssuedAt: sealedListClock.Add(-time.Hour), ExpiresAt: sealedListClock.Add(time.Hour),
		},
		Account: console.BoundAccount{ID: 9, Name: "Break-glass account", Email: registry.ReservedBreakGlassEmail, Reserved: true},
	}}}
	handler := console.NewSealedCredentialsHandler(lister).WithClock(func() time.Time { return sealedListClock })
	d := sealedListDirectory()

	rec := serveSealedList(t, handler, d, auth.User{ID: "1", UserID: "1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("holder: %d %s", rec.Code, rec.Body)
	}
	if lister.actor != 1 {
		t.Errorf("the read was recorded for actor %d, want 1", lister.actor)
	}
	if strings.Contains(rec.Body.String(), "0123456789abcdef01234567") || strings.Contains(rec.Body.String(), "selector") {
		t.Fatalf("the list carries the selector: %s", rec.Body)
	}
	var body struct {
		Total int `json:"total"`
		Rows  []struct {
			ID, Label, Level, State string
			Split, Unbound          bool
			BoundUser               struct {
				ID    int64
				Email string
			} `json:"bound_user"`
			UsedAt *time.Time `json:"used_at"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 1 || len(body.Rows) != 1 {
		t.Fatalf("body = %+v", body)
	}
	row := body.Rows[0]
	if row.ID != id.String() || row.Level != registry.ElevationLevelSuperAdmin || row.State != registry.SealedStateActive ||
		!row.Split || !row.Unbound || row.BoundUser.ID != 9 || row.BoundUser.Email != registry.ReservedBreakGlassEmail ||
		row.UsedAt != nil {
		t.Fatalf("row = %+v", row)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	lister.calls = 0
	if rec := serveSealedList(t, handler, d, auth.User{ID: "2", UserID: "2"}); rec.Code != http.StatusForbidden {
		t.Fatalf("a caller without access.breakglass.manage: %d %s, want 403", rec.Code, rec.Body)
	}
	if lister.calls != 0 {
		t.Fatal("a refused caller reached the list (and its audit record)")
	}
}

func TestTheSealedCredentialListFailsClosed(t *testing.T) {
	t.Parallel()
	d := sealedListDirectory()
	holder := auth.User{ID: "1", UserID: "1"}
	if rec := serveSealedList(t, console.NewSealedCredentialsHandler(nil), d, holder); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no lister: %d, want 503", rec.Code)
	}
	failing := &fakeSealedLister{err: errors.New("the audit append failed")}
	rec := serveSealedList(t, console.NewSealedCredentialsHandler(failing), d, holder)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "append") {
		t.Fatalf("a failed read: %d %s, want a bare 500", rec.Code, rec.Body)
	}

	// A principal with no owning account is refused, not served under an
	// audit record that names nobody, even if a gate let it through.
	lister := &fakeSealedLister{}
	req := httptest.NewRequest(http.MethodGet, console.SealedCredentialsPath, nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "1", TokenID: "token-1", AuthType: "token"}))
	rec = httptest.NewRecorder()
	console.NewSealedCredentialsHandler(lister).List(rec, req)
	if rec.Code != http.StatusUnauthorized || lister.calls != 0 {
		t.Fatalf("no owning account: %d with %d reads, want 401 and none", rec.Code, lister.calls)
	}
}

/* ── on PostgreSQL ─────────────────────────────────────────────────────── */

// The real list: a bound and an unbound credential, issued by the code the
// operator command runs. The response holds neither selector nor digest, the
// reserved account is the bound user of the unbound one, and the read is
// recorded with the source `console`.
func TestTheSealedCredentialListOnPostgreSQL(t *testing.T) {
	pool := newSealedListDatabase(t)
	ctx := t.Context()
	appender, err := securityaudit.NewPostgresAppender("elitea-main/sealed-list-test")
	if err != nil {
		t.Fatal(err)
	}
	var person int64
	if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__user (email, name) VALUES ('custodian@autotest.local', 'Custodian') RETURNING id`).Scan(&person); err != nil {
		t.Fatal(err)
	}
	var bound, unbound registry.IssuedSealedCredential
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		if bound, err = registry.IssueSealedCredential(ctx, tx, appender, registry.SealedIssue{
			UserID: person, Label: "bound envelope", At: sealedListClock.Add(-2 * time.Hour),
		}); err != nil {
			return err
		}
		unbound, err = registry.IssueSealedCredential(ctx, tx, appender, registry.SealedIssue{
			Unbound: true, Ticket: "INC-4711", Label: "safe B", Level: registry.ElevationLevelSuperAdmin, Split: true,
			At: sealedListClock.Add(-time.Hour),
		})
		return err
	}); err != nil {
		t.Fatalf("issue: %v", err)
	}
	var digests []string
	rows, err := pool.Query(ctx, `SELECT verifier_digest FROM elitea_auth.sealed_credentials`)
	if err != nil {
		t.Fatal(err)
	}
	if digests, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(digests) != 2 {
		t.Fatalf("digests: %v %v", digests, err)
	}

	handler := console.NewSealedCredentialsHandler(console.NewPostgresSealedCredentials(pool, appender)).
		WithClock(func() time.Time { return sealedListClock })
	d := sealedListDirectory()
	d.users[person] = []string{"breakglass_manager"}
	rec := serveSealedList(t, handler, d, auth.User{ID: fmt.Sprint(person), UserID: fmt.Sprint(person)})
	if rec.Code != http.StatusOK {
		t.Fatalf("holder: %d %s", rec.Code, rec.Body)
	}
	text := rec.Body.String()
	for _, secret := range []string{bound.Selector, unbound.Selector, bound.Verifier, unbound.Verifier,
		bound.Verifier[:16], unbound.Verifier[len(unbound.Verifier)-16:], digests[0], digests[1], "$argon2id$"} {
		if strings.Contains(text, secret) {
			t.Fatalf("the list carries %q: %s", secret, text)
		}
	}
	var body struct {
		Total int `json:"total"`
		Rows  []struct {
			ID        string `json:"id"`
			Label     string `json:"label"`
			Unbound   bool   `json:"unbound"`
			BoundUser struct {
				ID    int64  `json:"id"`
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"bound_user"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 2 || len(body.Rows) != 2 {
		t.Fatalf("body = %s", text)
	}
	// Newest first: the unbound one.
	reserved, owner := body.Rows[0], body.Rows[1]
	if reserved.ID != unbound.ID.String() || !reserved.Unbound || reserved.BoundUser.Email != registry.ReservedBreakGlassEmail ||
		reserved.BoundUser.ID != unbound.UserID {
		t.Fatalf("the unbound credential is not listed with the reserved account: %+v", reserved)
	}
	if owner.ID != bound.ID.String() || owner.Unbound || owner.BoundUser.ID != person || owner.BoundUser.Name != "Custodian" {
		t.Fatalf("the bound credential: %+v", owner)
	}
	var events int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event -> 'data' ->> 'action' = $1 AND event::text LIKE '%"source:console"%' AND event::text LIKE '%"credentials:2"%'`,
		registry.SealedCredentialListAction).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("%d recorded console reads, want 1", events)
	}
}

// newSealedListDatabase is an isolated database with 001_initial.sql and the
// shared corpus applied.
func newSealedListDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}
	return isolatedDatabase(t, databaseURL, "elitea_sealed_list")
}

// isolatedDatabase creates a database named after prefix, applies
// 001_initial.sql and the shared corpus, and drops it at cleanup.
func isolatedDatabase(t *testing.T, databaseURL, prefix string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	databaseName := fmt.Sprintf("%s_%d_%d", prefix, os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse the database URL: %v", err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, err := pgxpool.New(dropCtx, databaseURL)
		if err != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)")
	})
	bootstrap, err := os.ReadFile("../../../infra/db/migrations/001_initial.sql")
	if err != nil {
		t.Fatalf("read the bootstrap schema: %v", err)
	}
	if _, err := pool.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatalf("apply the bootstrap schema: %v", err)
	}
	if err := migrate.New(pool, migrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared corpus: %v", err)
	}
	return pool
}
