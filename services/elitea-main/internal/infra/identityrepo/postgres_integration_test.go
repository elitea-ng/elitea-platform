package identityrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
	dbschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/schema"
)

func TestNewPostgresRepositoryRequiresPool(t *testing.T) {
	if _, err := NewPostgresRepository(nil); err == nil {
		t.Fatal("expected missing PostgreSQL pool error")
	}
}

func TestPostgresIdentityProvisioningCurrentBaseline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := newIdentityTestDatabase(t, ctx)

	mustExec(t, ctx, pool, `
INSERT INTO public.auth_core__role (name, mode) VALUES
    ('super_admin', 'administration'),
    ('admin', 'administration'),
    ('viewer', 'administration'),
    ('viewer', 'default');
INSERT INTO public.auth_core__project_role (project_id, name) VALUES
    (7, 'viewer'),
    (7, 'public_admin');`)

	t.Run("new user, initial admin, and missing project roles", func(t *testing.T) {
		service := newProvisionService(t, pool, identity.ProvisioningPolicy{
			InitialGlobalAdmins: []string{"Initial-Admin"},
			ProjectEnrollment: identity.ProjectEnrollmentPolicy{
				ProjectID:      7,
				AllowedDomains: "centry.user",
				// This is the effective deployment override, not only the
				// elitea_core plugin default. Roles absent from project 7 are
				// intentionally ignored by the repository.
				AdditionalGlobalAdminRoles: []string{
					"system", "admin", "editor", "viewer", "prompt_lib_public",
					"prompt_lib_moderators", "public_admin",
				},
			},
		})
		result, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
			Provider:          "oidc",
			ProviderReference: "Initial-Admin",
			GivenName:         "Initial",
			FamilyName:        "Admin",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if result.UserID <= 0 {
			t.Fatalf("result = %+v", result)
		}
		assertUser(t, ctx, pool, result.UserID, "initial-admin@centry.user", "Initial Admin", true, false)
		assertCount(t, ctx, pool, 1, `SELECT count(*) FROM public.auth_core__user_provider WHERE user_id = $1 AND provider_ref = 'Initial-Admin'`, result.UserID)
		assertCount(t, ctx, pool, 1, `
SELECT count(*)
FROM public.auth_core__user_role AS ur
JOIN public.auth_core__role AS role ON role.id = ur.role_id
WHERE ur.user_id = $1 AND role.mode = 'administration' AND role.name = 'super_admin'`, result.UserID)
		assertProjectRoles(t, ctx, pool, result.UserID, 7, []string{"viewer"})

		// Repeated browser logins re-evaluate the project policy, remain
		// idempotent, update last_login with the database clock, and preserve
		// an already populated name.
		mustExec(t, ctx, pool, `UPDATE public.auth_core__user SET last_login = TIMESTAMP '2000-01-01 00:00:00', name = 'Preserved' WHERE id = $1`, result.UserID)
		repeated, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
			Provider:          "oidc",
			ProviderReference: "Initial-Admin",
			Name:              "Replacement",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if repeated.UserID != result.UserID {
			t.Fatalf("repeated user = %d, want %d", repeated.UserID, result.UserID)
		}
		assertUser(t, ctx, pool, result.UserID, "initial-admin@centry.user", "Preserved", true, false)
		assertProjectRoles(t, ctx, pool, result.UserID, 7, []string{"public_admin", "viewer"})
	})

	t.Run("existing exact email is linked", func(t *testing.T) {
		var userID int64
		if err := pool.QueryRow(ctx, `INSERT INTO public.auth_core__user (email, name) VALUES ('existing@example.com', NULL) RETURNING id`).Scan(&userID); err != nil {
			t.Fatal(err)
		}
		mustExec(t, ctx, pool, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE name = 'viewer' AND mode = 'default'`, userID)
		service := newProvisionService(t, pool, identity.ProvisioningPolicy{
			InitialGlobalAdmins: []string{"Existing-Ref"},
			ProjectEnrollment:   identity.ProjectEnrollmentPolicy{ProjectID: 7, AllowedDomains: "example.com"},
		})
		result, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
			Provider:          "saml",
			ProviderReference: "Existing-Ref",
			Email:             "EXISTING@EXAMPLE.COM",
			Name:              "Existing Name",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if result.UserID != userID {
			t.Fatalf("user = %d, want existing %d", result.UserID, userID)
		}
		assertUser(t, ctx, pool, userID, "existing@example.com", "Existing Name", true, false)
		assertCount(t, ctx, pool, 1, `SELECT count(*) FROM public.auth_core__user_provider WHERE user_id = $1 AND provider_ref = 'Existing-Ref'`, userID)
		assertCount(t, ctx, pool, 1, `
SELECT count(*)
FROM public.auth_core__user_role AS ur
JOIN public.auth_core__role AS role ON role.id = ur.role_id
WHERE ur.user_id = $1 AND role.mode = 'administration' AND role.name = 'super_admin'`, userID)
		assertProjectRoles(t, ctx, pool, userID, 7, []string{"viewer"})
	})

	t.Run("raw provider mapping wins over asserted email", func(t *testing.T) {
		var mappedID, emailCollisionID int64
		if err := pool.QueryRow(ctx, `INSERT INTO public.auth_core__user (email, name) VALUES ('mapped@example.com', 'Keep Me') RETURNING id`).Scan(&mappedID); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `INSERT INTO public.auth_core__user (email, name) VALUES ('collision@example.com', 'Other User') RETURNING id`).Scan(&emailCollisionID); err != nil {
			t.Fatal(err)
		}
		mustExec(t, ctx, pool, `INSERT INTO public.auth_core__user_provider (user_id, provider_ref) VALUES ($1, 'raw-mapping')`, mappedID)

		service := newProvisionService(t, pool, identity.ProvisioningPolicy{
			ProjectEnrollment: identity.ProjectEnrollmentPolicy{ProjectID: 7, AllowedDomains: "example.com"},
		})
		result, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
			Provider:          "oidc",
			ProviderReference: "raw-mapping",
			Email:             "collision@example.com",
			Name:              "Do Not Replace",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if result.UserID != mappedID {
			t.Fatalf("user = %d, want mapped %d", result.UserID, mappedID)
		}
		assertUser(t, ctx, pool, mappedID, "mapped@example.com", "Keep Me", true, false)
		assertUser(t, ctx, pool, emailCollisionID, "collision@example.com", "Other User", false, false)
	})

	t.Run("existing administration role suppresses initial super admin", func(t *testing.T) {
		var userID int64
		if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__user (email, name)
VALUES ('existing-role@example.com', 'Existing Role')
RETURNING id`).Scan(&userID); err != nil {
			t.Fatal(err)
		}
		mustExec(t, ctx, pool, `
INSERT INTO public.auth_core__user_provider (user_id, provider_ref)
VALUES ($1, 'existing-administration-role')`, userID)
		mustExec(t, ctx, pool, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id
FROM public.auth_core__role
WHERE name = 'viewer' AND mode = 'administration'`, userID)

		service := newProvisionService(t, pool, identity.ProvisioningPolicy{
			InitialGlobalAdmins: []string{"existing-administration-role"},
		})
		result, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
			Provider:          "oidc",
			ProviderReference: "existing-administration-role",
			Email:             "different@example.com",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if result.UserID != userID {
			t.Fatalf("user = %d, want %d", result.UserID, userID)
		}
		assertCount(t, ctx, pool, 1, `
SELECT count(*)
FROM public.auth_core__user_role AS ur
JOIN public.auth_core__role AS role ON role.id = ur.role_id
WHERE ur.user_id = $1 AND role.mode = 'administration' AND role.name = 'viewer'`, userID)
		assertCount(t, ctx, pool, 0, `
SELECT count(*)
FROM public.auth_core__user_role AS ur
JOIN public.auth_core__role AS role ON role.id = ur.role_id
WHERE ur.user_id = $1 AND role.mode = 'administration' AND role.name = 'super_admin'`, userID)
	})

	t.Run("suspended identity rolls back all login effects", func(t *testing.T) {
		var userID int64
		if err := pool.QueryRow(ctx, `INSERT INTO public.auth_core__user (email, name, suspended) VALUES ('suspended@example.com', '', true) RETURNING id`).Scan(&userID); err != nil {
			t.Fatal(err)
		}
		mustExec(t, ctx, pool, `INSERT INTO public.auth_core__user_provider (user_id, provider_ref) VALUES ($1, 'suspended-ref')`, userID)
		service := newProvisionService(t, pool, identity.ProvisioningPolicy{
			InitialGlobalAdmins: []string{"suspended-ref"},
			ProjectEnrollment: identity.ProjectEnrollmentPolicy{
				ProjectID:                  7,
				AllowedDomains:             "example.com",
				AdditionalGlobalAdminRoles: []string{"public_admin"},
			},
		})
		_, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
			Provider:          "oidc",
			ProviderReference: "suspended-ref",
			Email:             "different@example.com",
			Name:              "Must Not Persist",
		}})
		if !errors.Is(err, identity.ErrIdentitySuspended) {
			t.Fatalf("error = %v, want %v", err, identity.ErrIdentitySuspended)
		}
		assertUser(t, ctx, pool, userID, "suspended@example.com", "", false, true)
		assertCount(t, ctx, pool, 0, `SELECT count(*) FROM public.auth_core__user_role WHERE user_id = $1`, userID)
		assertCount(t, ctx, pool, 0, `SELECT count(*) FROM public.auth_core__project_user_role WHERE user_id = $1`, userID)
	})

	t.Run("suspended exact email is not linked to a new provider", func(t *testing.T) {
		var userID int64
		if err := pool.QueryRow(ctx, `INSERT INTO public.auth_core__user (email, name, suspended) VALUES ('suspended-email@example.com', '', true) RETURNING id`).Scan(&userID); err != nil {
			t.Fatal(err)
		}
		service := newProvisionService(t, pool, identity.ProvisioningPolicy{
			ProjectEnrollment: identity.ProjectEnrollmentPolicy{ProjectID: 7, AllowedDomains: "example.com"},
		})
		_, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
			Provider:          "saml",
			ProviderReference: "new-provider-for-suspended-email",
			Email:             "SUSPENDED-EMAIL@EXAMPLE.COM",
			Name:              "Must Not Persist",
		}})
		if !errors.Is(err, identity.ErrIdentitySuspended) {
			t.Fatalf("error = %v, want %v", err, identity.ErrIdentitySuspended)
		}
		assertUser(t, ctx, pool, userID, "suspended-email@example.com", "", false, true)
		assertCount(t, ctx, pool, 0, `SELECT count(*) FROM public.auth_core__user_provider WHERE provider_ref = 'new-provider-for-suspended-email'`)
	})

	t.Run("eligible login ignores all roles missing from target project", func(t *testing.T) {
		service := newProvisionService(t, pool, identity.ProvisioningPolicy{
			ProjectEnrollment: identity.ProjectEnrollmentPolicy{
				ProjectID:                  99,
				AllowedDomains:             "*",
				AdditionalGlobalAdminRoles: []string{"does_not_exist"},
			},
		})
		result, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
			Provider:          "form",
			ProviderReference: "missing-role-user",
		}})
		if err != nil {
			t.Fatal(err)
		}
		assertCount(t, ctx, pool, 0, `SELECT count(*) FROM public.auth_core__project_user_role WHERE user_id = $1`, result.UserID)
	})

	t.Run("concurrent same provider converges on one identity", func(t *testing.T) {
		service := newProvisionService(t, pool, identity.ProvisioningPolicy{
			ProjectEnrollment: identity.ProjectEnrollmentPolicy{ProjectID: 7, AllowedDomains: "example.com"},
		})
		const callers = 12
		start := make(chan struct{})
		results := make(chan identity.ProvisionResult, callers)
		errorsCh := make(chan error, callers)
		var wait sync.WaitGroup
		for range callers {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				result, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
					Provider:          "oidc",
					ProviderReference: "concurrent-provider",
					Email:             "CONCURRENT@EXAMPLE.COM",
					Name:              "Concurrent User",
				}})
				if err != nil {
					errorsCh <- err
					return
				}
				results <- result
			}()
		}
		close(start)
		wait.Wait()
		close(results)
		close(errorsCh)
		for err := range errorsCh {
			t.Errorf("concurrent provision: %v", err)
		}

		var userID int64
		resultCount := 0
		for result := range results {
			resultCount++
			if userID == 0 {
				userID = result.UserID
			}
			if result.UserID != userID {
				t.Errorf("user = %d, want converged %d", result.UserID, userID)
			}
		}
		if resultCount != callers {
			t.Fatalf("successful callers = %d, want %d", resultCount, callers)
		}
		assertCount(t, ctx, pool, 1, `SELECT count(*) FROM public.auth_core__user WHERE email = 'concurrent@example.com'`)
		assertCount(t, ctx, pool, 1, `SELECT count(*) FROM public.auth_core__user_provider WHERE provider_ref = 'concurrent-provider'`)
		assertProjectRoles(t, ctx, pool, userID, 7, []string{"viewer"})
	})

	t.Run("concurrent provider references converge on the unique exact email", func(t *testing.T) {
		const callers = 12
		initialAdmins := make([]string, callers)
		for caller := range callers {
			initialAdmins[caller] = fmt.Sprintf("shared-email-provider-%d", caller)
		}
		service := newProvisionService(t, pool, identity.ProvisioningPolicy{
			InitialGlobalAdmins: initialAdmins,
			ProjectEnrollment:   identity.ProjectEnrollmentPolicy{ProjectID: 7, AllowedDomains: "example.com"},
		})
		start := make(chan struct{})
		results := make(chan identity.ProvisionResult, callers)
		errorsCh := make(chan error, callers)
		var wait sync.WaitGroup
		for caller := range callers {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				result, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
					Provider:          "oidc",
					ProviderReference: fmt.Sprintf("shared-email-provider-%d", caller),
					Email:             "SHARED@EXAMPLE.COM",
					Name:              "Shared Email User",
				}})
				if err != nil {
					errorsCh <- err
					return
				}
				results <- result
			}()
		}
		close(start)
		wait.Wait()
		close(results)
		close(errorsCh)
		for err := range errorsCh {
			t.Errorf("concurrent provision: %v", err)
		}

		var userID int64
		resultCount := 0
		for result := range results {
			resultCount++
			if userID == 0 {
				userID = result.UserID
			}
			if result.UserID != userID {
				t.Errorf("user = %d, want converged %d", result.UserID, userID)
			}
		}
		if resultCount != callers {
			t.Fatalf("successful callers = %d, want %d", resultCount, callers)
		}
		assertCount(t, ctx, pool, 1, `SELECT count(*) FROM public.auth_core__user WHERE email = 'shared@example.com'`)
		assertCount(t, ctx, pool, callers, `SELECT count(*) FROM public.auth_core__user_provider WHERE provider_ref LIKE 'shared-email-provider-%'`)
		assertCount(t, ctx, pool, 1, `
SELECT count(*)
FROM public.auth_core__user_role AS ur
JOIN public.auth_core__role AS role ON role.id = ur.role_id
WHERE ur.user_id = $1 AND role.mode = 'administration' AND role.name = 'super_admin'`, userID)
		assertProjectRoles(t, ctx, pool, userID, 7, []string{"viewer"})
	})

	t.Run("cancellation interrupts an in-flight provider lock wait", func(t *testing.T) {
		const providerReference = "blocked-provider-reference"
		holder, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(context.Background()) }()
		if _, err := holder.Exec(ctx, `
SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, providerReference); err != nil {
			t.Fatal(err)
		}

		attemptCtx, cancelAttempt := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancelAttempt()
		service := newProvisionService(t, pool, identity.ProvisioningPolicy{})
		_, err = service.Provision(attemptCtx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
			Provider:          "oidc",
			ProviderReference: providerReference,
			Email:             "blocked@example.com",
		}})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want %v", err, context.DeadlineExceeded)
		}
		assertCount(t, ctx, pool, 0, `SELECT count(*) FROM public.auth_core__user WHERE email = 'blocked@example.com'`)
		assertCount(t, ctx, pool, 0, `SELECT count(*) FROM public.auth_core__user_provider WHERE provider_ref = $1`, providerReference)
	})
}

func TestPostgresIdentityProvisioningRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := newIdentityTestDatabase(t, ctx)

	// The injected failure occurs after the user and provider inserts and
	// proves the repository owns one rollback.
	failProvisioningAfterUserWrite(t, ctx, pool)
	service := newProvisionService(t, pool, identity.ProvisioningPolicy{})
	_, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
		Provider:          "oidc",
		ProviderReference: "rollback-provider",
		Email:             "rollback@example.com",
		Name:              "Rollback User",
	}})
	if !errors.Is(err, identity.ErrProvisioningFailed) {
		t.Fatalf("error = %v, want %v", err, identity.ErrProvisioningFailed)
	}
	assertCount(t, ctx, pool, 0, `SELECT count(*) FROM public.auth_core__user WHERE email = 'rollback@example.com'`)
	assertCount(t, ctx, pool, 0, `SELECT count(*) FROM public.auth_core__user_provider WHERE provider_ref = 'rollback-provider'`)
}

func newProvisionService(t *testing.T, pool *pgxpool.Pool, policy identity.ProvisioningPolicy) *identity.ProvisionService {
	t.Helper()
	repository, err := NewPostgresRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.NewProvisionService(repository, policy)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func newIdentityTestDatabase(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("ELITEA_AUTH_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("set ELITEA_AUTH_TEST_DATABASE_URL to an isolated PostgreSQL admin database")
	}
	adminConfig, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)

	databaseName := fmt.Sprintf("elitea_identity_test_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(cleanupCtx,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
			databaseName,
		)
		_, _ = adminPool.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+identifier)
	})

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, dbschema.AuthCoreBaselineSQLCProjection); err != nil {
		t.Fatal(err)
	}
	// The SCIM side table the shared adoption guard reads (adoption.go), from
	// the migration's own file.
	migration, err := os.ReadFile("../../../migrations/shared/0096_scim_provisioning.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	return pool
}

// failProvisioningAfterUserWrite makes every UPDATE of auth_core__user raise,
// so a provisioning attempt fails at TouchProvisionedAuthUser: after the user
// and provider rows are written and before any role or credential is.
func failProvisioningAfterUserWrite(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, ctx, pool, `
CREATE FUNCTION public.identityrepo_test_fail_user_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'identityrepo test: injected provisioning failure';
END
$$;
CREATE TRIGGER identityrepo_test_fail_user_update
BEFORE UPDATE ON public.auth_core__user
FOR EACH ROW EXECUTE FUNCTION public.identityrepo_test_fail_user_update();`)
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, statement string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, statement, args...); err != nil {
		t.Fatal(err)
	}
}

func assertCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int64, statement string, args ...any) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(ctx, statement, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("count = %d, want %d for %s", got, want, statement)
	}
}

func assertUser(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	userID int64,
	wantEmail string,
	wantName string,
	wantLastLogin bool,
	wantSuspended bool,
) {
	t.Helper()
	var email, name string
	var lastLogin *time.Time
	var suspended bool
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(email, ''), COALESCE(name, ''), last_login, suspended
FROM public.auth_core__user
WHERE id = $1`, userID).Scan(&email, &name, &lastLogin, &suspended); err != nil {
		t.Fatal(err)
	}
	if email != wantEmail || name != wantName || (lastLogin != nil) != wantLastLogin || suspended != wantSuspended {
		t.Fatalf("user = (email=%q name=%q last_login=%v suspended=%t), want (%q %q present=%t %t)",
			email, name, lastLogin, suspended, wantEmail, wantName, wantLastLogin, wantSuspended)
	}
	if wantLastLogin && lastLogin.Before(time.Now().UTC().Add(-time.Minute)) {
		t.Fatalf("last_login = %v, expected recent database clock", lastLogin)
	}
}

func assertProjectRoles(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID int64, projectID int32, want []string) {
	t.Helper()
	rows, err := pool.Query(ctx, `
SELECT role.name
FROM public.auth_core__project_user_role AS user_role
JOIN public.auth_core__project_role AS role ON role.id = user_role.role_id
WHERE user_role.user_id = $1 AND user_role.project_id = $2`, userID, projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make([]string, 0, len(want))
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			t.Fatal(err)
		}
		got = append(got, role)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("project roles = %v, want %v", got, want)
	}
}
