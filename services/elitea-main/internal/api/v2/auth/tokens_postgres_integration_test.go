package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	identity "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	dbschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/schema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func TestPostgresTokenRepositoryLifecycle(t *testing.T) {
	adminURL := os.Getenv("ELITEA_AUTH_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("set ELITEA_AUTH_TEST_DATABASE_URL to an isolated PostgreSQL admin database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_auth_test_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = adminPool.Exec(context.Background(),
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
			databaseName,
		)
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+identifier)
	}()

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, dbschema.AuthCoreBaselineSQLCProjection); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, dbschema.CentryProjectsBaselineSQLCProjection); err != nil {
		t.Fatal(err)
	}
	// The REAL migration files, not hand-copied projections. They carry the
	// to_regclass guard and the foreign key, so this test proves both apply
	// against a database that already has auth_core.
	applyTokenMigrations(ctx, t, pool, 1)
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, suspended)
VALUES (7, 'owner@example.test', false), (42, 'collision@example.test', false);`); err != nil {
		t.Fatal(err)
	}

	repository := newPostgresTokenRepository(pool)
	expires := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Microsecond)
	created, err := repository.Create(ctx, tokenCreateInput{
		OwnerID: 7,
		Name:    stringAddress("ci-token"),
		Expires: &expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.UserID != 7 || created.ID <= 0 || created.UUID == nil || !validTokenUUID(*created.UUID) || created.Name == nil || *created.Name != "ci-token" {
		t.Fatalf("created = %+v", created)
	}
	if created.ProjectID != nil {
		t.Fatalf("created project = %d, want unbound", *created.ProjectID)
	}
	// The issue stamp is written inside the create transaction, so a committed
	// token always has one — it is what tells a key that has lived a month from
	// one whose whole life is twelve hours when the expiry notice is decided.
	var issuedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT issued_at FROM elitea_identity.token_lifecycle WHERE token_id = $1`,
		created.ID,
	).Scan(&issuedAt); err != nil {
		t.Fatalf("read issue stamp for token %d: %v", created.ID, err)
	}
	if issuedAt == nil {
		t.Fatalf("token %d committed with no issue stamp", created.ID)
	}
	const signingKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	encoded, err := (&Handler{tokenSigningKey: []byte(signingKey)}).signBaselineToken(created)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authsvc.NewLocalValidator(pool, signingKey).ValidateToken(ctx, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if principal.TokenID != fmt.Sprintf("%d", created.ID) || principal.UserID != "7" || principal.ID != "7" {
		t.Fatalf("validated principal = %+v", principal)
	}
	currentUser, err := authsvc.NewCurrentUserResolver(pool).Resolve(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	if currentUser.ID != 7 || currentUser.Email == nil || *currentUser.Email != "owner@example.test" {
		t.Fatalf("current user = %+v", currentUser)
	}
	if currentUser.Name != nil || currentUser.LastLogin != nil || currentUser.Suspended {
		t.Fatalf("current user nullable/state fields = %+v", currentUser)
	}
	principalValidator := authsvc.NewPrincipalValidator(pool)
	forwarded, err := principalValidator.ValidatePrincipal(ctx, identity.User{
		ID:       fmt.Sprintf("%d", created.ID),
		TokenID:  fmt.Sprintf("%d", created.ID),
		UserID:   "7",
		Email:    "-",
		AuthType: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if forwarded.ID != "7" || forwarded.UserID != "7" || forwarded.TokenID != fmt.Sprintf("%d", created.ID) || forwarded.Email != "owner@example.test" {
		t.Fatalf("forwarded principal = %+v", forwarded)
	}
	session, err := principalValidator.ValidatePrincipal(ctx, identity.User{
		ID:       "42",
		UserID:   "7",
		Email:    "untrusted@example.test",
		AuthType: "session",
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "7" || session.UserID != "7" || session.Email != "owner@example.test" {
		t.Fatalf("session principal = %+v", session)
	}

	listed, err := repository.List(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed = %+v", listed)
	}
	var nullableMetadataID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__token (uuid, user_id, name)
VALUES (NULL, 7, NULL)
RETURNING id`).Scan(&nullableMetadataID); err != nil {
		t.Fatal(err)
	}
	listed, err = repository.List(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	var nullableMetadataFound bool
	for _, record := range listed {
		if record.ID == nullableMetadataID {
			nullableMetadataFound = true
			if record.UUID != nil || record.Name != nil {
				t.Fatalf("nullable PAT metadata was coerced: %+v", record)
			}
		}
	}
	if !nullableMetadataFound {
		t.Fatalf("nullable PAT %d is missing from list: %+v", nullableMetadataID, listed)
	}
	got, err := repository.GetOwned(ctx, 7, *created.UUID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("get = %+v, err=%v", got, err)
	}
	if _, err := repository.GetOwned(ctx, 42, *created.UUID); !errors.Is(err, errTokenNotFound) {
		t.Fatalf("cross-owner get error = %v, want errTokenNotFound", err)
	}
	if err := repository.DeleteOwned(ctx, 42, *created.UUID); !errors.Is(err, errTokenForbidden) {
		t.Fatalf("cross-owner delete error = %v, want errTokenForbidden", err)
	}
	if err := repository.DeleteOwned(ctx, 7, *created.UUID); err != nil {
		t.Fatal(err)
	}
	if _, err := authsvc.NewLocalValidator(pool, signingKey).ValidateToken(ctx, encoded); err == nil {
		t.Fatal("deleted PAT remained valid")
	}
	if _, err := repository.GetOwned(ctx, 7, *created.UUID); !errors.Is(err, errTokenNotFound) {
		t.Fatalf("deleted token get error = %v, want errTokenNotFound", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE public.auth_core__user SET suspended = true WHERE id = 7`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Create(ctx, tokenCreateInput{OwnerID: 7, Name: stringAddress("blocked-token")}); !errors.Is(err, errTokenForbidden) {
		t.Fatalf("suspended-owner create error = %v, want errTokenForbidden", err)
	}
}

const tokenProjectBindingMigration = "shared/0071_token_project_binding.sql"

// tokenLifecycleMigration creates elitea_identity.token_lifecycle, which the
// PAT-create transaction writes the issue stamp into (tokens.go Create →
// repos.RecordPATIssued). A fixture that builds auth_core by projection has to
// apply it too, or every Create fails with 42P01 the way a deployment that
// skipped the migration would.
const tokenLifecycleMigration = "shared/0125_token_lifecycle.sql"

// applyTokenMigrations runs the real migration files this package's token paths
// depend on, `times` times each — more than once proves idempotency, which is
// what the ledgered runner assumes of every file it re-reads.
func applyTokenMigrations(ctx context.Context, t *testing.T, pool *pgxpool.Pool, times int) {
	t.Helper()
	for _, name := range []string{tokenProjectBindingMigration, tokenLifecycleMigration} {
		statements, err := platformmigrations.Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for range times {
			if _, err := pool.Exec(ctx, string(statements)); err != nil {
				t.Fatalf("apply %s: %v", name, err)
			}
		}
	}
}

// TestPostgresTokenProjectBindingLifecycle is the §4 half of ADR-0018 measured
// against a real database: the membership predicate, the refusal, the fact that
// a refusal writes no token row, and the cascade that removes a binding with
// its token.
func TestPostgresTokenProjectBindingLifecycle(t *testing.T) {
	adminURL := os.Getenv("ELITEA_AUTH_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("set ELITEA_AUTH_TEST_DATABASE_URL to an isolated PostgreSQL admin database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_auth_binding_test_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = adminPool.Exec(context.Background(),
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
			databaseName,
		)
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+identifier)
	}()

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	for _, statements := range []string{
		dbschema.AuthCoreBaselineSQLCProjection,
		dbschema.CentryProjectsBaselineSQLCProjection,
	} {
		if _, err := pool.Exec(ctx, statements); err != nil {
			t.Fatal(err)
		}
	}
	// Applied TWICE. The migrations must be idempotent, and a second run is the
	// only check that proves it.
	applyTokenMigrations(ctx, t, pool, 2)

	// Project 5 has the owner as a member. Project 6 does not. Project 7 has an
	// assignment but is suspended, so the predicate must refuse it as well.
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, suspended) VALUES (7, 'owner@example.test', false);
INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success, suspended) VALUES
    (5, 'member-project', 7, '{}'::json, true, false),
    (6, 'other-project', 7, '{}'::json, true, false),
    (8, 'suspended-project', 7, '{}'::json, true, true);
INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES
    (1, 5, 'admin'), (2, 8, 'admin');
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES
    (5, 7, 1), (8, 7, 2);`); err != nil {
		t.Fatal(err)
	}

	repository := newPostgresTokenRepository(pool)
	bound, err := repository.Create(ctx, tokenCreateInput{
		OwnerID:   7,
		Name:      stringAddress("bound-token"),
		ProjectID: int64Address(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if bound.ProjectID == nil || *bound.ProjectID != 5 {
		t.Fatalf("created binding = %v, want 5", bound.ProjectID)
	}

	// The binding must be readable through the list and get paths, which is the
	// only way a user can see what a key bills.
	got, err := repository.GetOwned(ctx, 7, *bound.UUID)
	if err != nil || got.ProjectID == nil || *got.ProjectID != 5 {
		t.Fatalf("get binding = %v, err = %v", got.ProjectID, err)
	}
	listed, err := repository.List(ctx, 7)
	if err != nil || len(listed) != 1 || listed[0].ProjectID == nil || *listed[0].ProjectID != 5 {
		t.Fatalf("listed = %+v, err = %v", listed, err)
	}

	// The signed bearer string is unchanged by the binding, and the validator
	// reports the binding from storage on the row it already reads.
	const signingKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	encoded, err := (&Handler{tokenSigningKey: []byte(signingKey)}).signBaselineToken(bound)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authsvc.NewLocalValidator(pool, signingKey).ValidateToken(ctx, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if principal.TokenProjectID == nil || *principal.TokenProjectID != 5 {
		t.Fatalf("validated principal binding = %v, want 5", principal.TokenProjectID)
	}
	if principal.TokenProjectActive == nil || !*principal.TokenProjectActive {
		t.Fatalf("bound project active = %v, want true for an active project", principal.TokenProjectActive)
	}
	if principal.BoundProjectRefused() {
		t.Fatal("an active bound project was refused")
	}

	// DEFECT: suspending a project revokes no binding. ProjectSuspend runs one
	// UPDATE on centry.project. A token bound to that project therefore kept
	// resolving to it at the /llm edge. It also kept spending its budget and
	// its provider credentials. An UNBOUND caller naming the same project was
	// already refused by IsCurrentUserProjectMember, which requires
	// `suspended IS FALSE`. The validator now reports the state on the row it
	// already reads, and the edge refuses the request.
	if _, err := pool.Exec(ctx, `UPDATE centry.project SET suspended = true WHERE id = 5`); err != nil {
		t.Fatal(err)
	}
	suspended, err := authsvc.NewLocalValidator(pool, signingKey).ValidateToken(ctx, encoded)
	if err != nil {
		t.Fatalf("a bound token must still authenticate after its project is suspended: %v", err)
	}
	if suspended.TokenProjectID == nil || *suspended.TokenProjectID != 5 {
		t.Fatalf("binding after suspension = %v, want 5", suspended.TokenProjectID)
	}
	if suspended.TokenProjectActive == nil || *suspended.TokenProjectActive {
		t.Fatalf("bound project active = %v, want false after suspension", suspended.TokenProjectActive)
	}
	if !suspended.BoundProjectRefused() {
		t.Fatal("a token bound to a suspended project was not refused")
	}

	// Suspension is reversible, so the refusal must lift with it. A fix that
	// deleted the binding could never restore this.
	if _, err := pool.Exec(ctx, `UPDATE centry.project SET suspended = false WHERE id = 5`); err != nil {
		t.Fatal(err)
	}
	restored, err := authsvc.NewLocalValidator(pool, signingKey).ValidateToken(ctx, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if restored.BoundProjectRefused() {
		t.Fatal("the binding stayed refused after the project was unsuspended")
	}

	// REGRESSION GUARD for the query change. Most tokens carry no binding. The
	// second join must stay a LEFT JOIN: an inner join on centry.project drops
	// every unbound row, which breaks authentication for the majority of
	// callers with "token not found".
	unbound, err := repository.Create(ctx, tokenCreateInput{
		OwnerID: 7,
		Name:    stringAddress("unbound-token"),
	})
	if err != nil {
		t.Fatal(err)
	}
	unboundEncoded, err := (&Handler{tokenSigningKey: []byte(signingKey)}).signBaselineToken(unbound)
	if err != nil {
		t.Fatal(err)
	}
	unboundPrincipal, err := authsvc.NewLocalValidator(pool, signingKey).ValidateToken(ctx, unboundEncoded)
	if err != nil {
		t.Fatalf("an unbound token must still authenticate: %v", err)
	}
	if unboundPrincipal.TokenProjectID != nil {
		t.Fatalf("unbound principal binding = %v, want nil", unboundPrincipal.TokenProjectID)
	}
	if unboundPrincipal.TokenProjectActive != nil {
		t.Fatalf("unbound principal project state = %v, want nil (nothing to report on)", unboundPrincipal.TokenProjectActive)
	}
	if unboundPrincipal.BoundProjectRefused() {
		t.Fatal("an unbound token was refused")
	}
	if err := repository.DeleteOwned(ctx, 7, *unbound.UUID); err != nil {
		t.Fatal(err)
	}

	for name, projectID := range map[string]int64{
		"non-member project": 6,
		"suspended project":  8,
		"unknown project":    999,
	} {
		t.Run(name, func(t *testing.T) {
			before := countTokens(ctx, t, pool)
			if _, err := repository.Create(ctx, tokenCreateInput{
				OwnerID:   7,
				Name:      stringAddress("refused-token"),
				ProjectID: &projectID,
			}); !errors.Is(err, errTokenProjectForbidden) {
				t.Fatalf("create error = %v, want errTokenProjectForbidden", err)
			}
			if after := countTokens(ctx, t, pool); after != before {
				t.Fatalf("token rows = %d, want %d; a refused project must create no token", after, before)
			}
		})
	}

	// Deleting a token removes its binding. Two independent mechanisms do this:
	// DeleteOwned deletes the binding in the same transaction, and the foreign
	// key cascades. This first case runs with both in place.
	if err := repository.DeleteOwned(ctx, 7, *bound.UUID); err != nil {
		t.Fatal(err)
	}
	if got := countBindings(ctx, t, pool, bound.ID); got != 0 {
		t.Fatalf("bindings after token delete = %d, want 0", got)
	}

	// The case that discriminates. The assertion above passes on the cascade
	// alone, so it cannot see whether the application deletes anything. Migration
	// 0071 guards its foreign key with to_regclass, and a guard that skips is
	// never revisited, so a database that ran the migration before auth_core
	// existed has the table and no constraint for its whole life
	// (spec-llm-project-scope §3.1). Drop the constraint and the application
	// delete is the only mechanism left.
	if _, err := pool.Exec(ctx,
		`ALTER TABLE elitea_identity.token_project_binding
             DROP CONSTRAINT token_project_binding_token_id_fkey`,
	); err != nil {
		t.Fatalf("drop the binding foreign key: %v", err)
	}
	uncascaded, err := repository.Create(ctx, tokenCreateInput{
		OwnerID:   7,
		Name:      stringAddress("uncascaded-token"),
		ProjectID: int64Address(5),
	})
	if err != nil {
		t.Fatalf("create a second bound token: %v", err)
	}
	if got := countBindings(ctx, t, pool, uncascaded.ID); got != 1 {
		t.Fatalf("the second binding was not written: got %d rows, want 1", got)
	}
	if err := repository.DeleteOwned(ctx, 7, *uncascaded.UUID); err != nil {
		t.Fatal(err)
	}
	if got := countBindings(ctx, t, pool, uncascaded.ID); got != 0 {
		t.Fatalf("bindings after token delete without the cascade = %d, want 0 — "+
			"the application must delete the binding itself, because a database "+
			"whose to_regclass guard skipped has no constraint to fall back on", got)
	}
}

func countBindings(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tokenID int64) int {
	t.Helper()
	var bindings int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM elitea_identity.token_project_binding WHERE token_id = $1`,
		tokenID,
	).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	return bindings
}

// TestTokenProjectBindingMigrationAppliesWithoutAuthCore proves the
// to_regclass guard. elitea-migrate can run before 001_initial.sql creates
// auth_core, and an unguarded REFERENCES clause would make 0070 a hard failure
// on exactly those databases.
func TestTokenProjectBindingMigrationAppliesWithoutAuthCore(t *testing.T) {
	adminURL := os.Getenv("ELITEA_AUTH_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("set ELITEA_AUTH_TEST_DATABASE_URL to an isolated PostgreSQL admin database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_auth_noauthcore_test_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = adminPool.Exec(context.Background(),
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
			databaseName,
		)
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+identifier)
	}()

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	bindingMigration, err := platformmigrations.Files.ReadFile(tokenProjectBindingMigration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(bindingMigration)); err != nil {
		t.Fatalf("apply %s without auth_core: %v", tokenProjectBindingMigration, err)
	}
	// The table must still exist. The credential read path LEFT JOINs it on
	// every request, and a missing relation is 42P01 for every caller.
	var present *string
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('elitea_identity.token_project_binding')::text`,
	).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present == nil {
		t.Fatal("token_project_binding was not created without auth_core")
	}
	var constraints int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM pg_constraint
WHERE conname = 'token_project_binding_token_id_fkey'`).Scan(&constraints); err != nil {
		t.Fatal(err)
	}
	if constraints != 0 {
		t.Fatalf("foreign keys = %d, want 0 without auth_core", constraints)
	}
}

func countTokens(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.auth_core__token`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	return total
}

func int64Address(value int64) *int64 {
	return &value
}
