package migrations_test

// Shared 0258 on an UPGRADED database (issue 2558): the corpus is applied up
// to 0257, the deployment's providers and links are written, and only then
// does the upgrade apply 0258. Every existing user must keep signing in, so
// each federated link is bound to the enabled provider of its protocol.

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const bindingsMigration = "0258_identity_provider_bindings.sql"

// withoutFile is the corpus with one migration hidden: the release before it.
type withoutFile struct {
	fs.FS
	hidden string
}

func (w withoutFile) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(w.FS, name)
	if err != nil {
		return nil, err
	}
	kept := entries[:0:0]
	for _, entry := range entries {
		if entry.Name() != w.hidden {
			kept = append(kept, entry)
		}
	}
	return kept, nil
}

// newPoolBefore0258 builds a database at shared head 0257.
func newPoolBefore0258(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(databaseURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the migration-corpus integration test", databaseURLEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	databaseName := fmt.Sprintf("elitea_bindings_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
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
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})
	bootstrap, err := os.ReadFile(bootstrapSchemaSQ)
	if err != nil {
		t.Fatalf("read bootstrap schema: %v", err)
	}
	if _, err := pool.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatalf("apply bootstrap schema: %v", err)
	}
	if err := migrate.New(pool, withoutFile{FS: migrations.Files, hidden: bindingsMigration}).ApplyShared(ctx); err != nil {
		t.Fatalf("apply shared migrations up to 0257: %v", err)
	}
	var present *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('elitea_auth.identity_bindings')::text`).Scan(&present); err != nil {
		t.Fatalf("to_regclass: %v", err)
	}
	if present != nil {
		t.Fatal("the pre-upgrade database already holds identity_bindings: 0258 was not hidden")
	}
	return pool
}

func upgradeTo0258(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := migrate.New(pool, migrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("upgrade to 0258: %v", err)
	}
}

func seedUpgradeExec(t *testing.T, pool *pgxpool.Pool, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func bindingsOf(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT provider_ref || ' -> ' || provider_key || ' @ ' || issuer || ' #' || user_id
FROM elitea_auth.identity_bindings ORDER BY 1`)
	if err != nil {
		t.Fatalf("read bindings: %v", err)
	}
	collected, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read bindings: %v", err)
	}
	return collected
}

func TestTheUpgradeBindsEveryLinkToTheEnabledProviderOfItsProtocol(t *testing.T) {
	pool := newPoolBefore0258(t)
	seedUpgradeExec(t, pool,
		`INSERT INTO public.auth_core__user (id, email, name) VALUES
		    (101, 'root@corp.example', 'Root'), (102, 'saml@corp.example', 'Saml'),
		    (103, 'legacy@corp.example', 'Legacy'), (104, 'invited@corp.example', 'Invited')`,
		// The SAME subject text in both protocols: two links, two providers.
		`INSERT INTO public.auth_core__user_provider (user_id, provider_ref) VALUES
		    (101, 'oidc:alice'), (102, 'saml:alice'), (101, 'oidc:second'),
		    (103, 'bare-legacy-subject'), (104, 'cirro:invite:token:abc')`,
		`INSERT INTO elitea_auth.identity_providers (provider_key, kind, display_name, enabled, config) VALUES
		    ('corporate', 'oidc', 'Corporate', true, '{"issuer": "https://idp.corp.example", "client_id": "elitea"}'),
		    ('retired', 'oidc', 'Retired', false, '{"issuer": "https://old.example", "client_id": "elitea"}'),
		    ('adfs', 'saml', 'ADFS', true, '{"idp_entity_id": " urn:adfs:corp ", "idp_sso_url": "https://adfs/sso"}')`,
	)

	upgradeTo0258(t, pool)

	got := strings.Join(bindingsOf(t, pool), "\n")
	want := strings.Join([]string{
		"oidc:alice -> corporate @ https://idp.corp.example #101",
		"oidc:second -> corporate @ https://idp.corp.example #101",
		"saml:alice -> adfs @ urn:adfs:corp #102",
	}, "\n")
	if got != want {
		t.Fatalf("bindings after the upgrade:\n%s\nwant:\n%s", got, want)
	}

	// A binding never outlives its link.
	seedUpgradeExec(t, pool, `DELETE FROM public.auth_core__user_provider WHERE provider_ref = 'oidc:second'`)
	if bindings := bindingsOf(t, pool); len(bindings) != 2 {
		t.Fatalf("the binding of a deleted link survived: %v", bindings)
	}
}

// A deployment federated by OIDC_ISSUER_URL has no stored provider, so its
// links stay unbound; the environment provider binds each on its next sign-in.
func TestTheUpgradeLeavesLinksUnboundWithoutAStoredProvider(t *testing.T) {
	pool := newPoolBefore0258(t)
	seedUpgradeExec(t, pool,
		`INSERT INTO public.auth_core__user (id, email, name) VALUES (101, 'root@corp.example', 'Root')`,
		`INSERT INTO public.auth_core__user_provider (user_id, provider_ref) VALUES (101, 'oidc:root')`,
	)

	upgradeTo0258(t, pool)

	if bindings := bindingsOf(t, pool); len(bindings) != 0 {
		t.Fatalf("links were bound with no stored provider: %v", bindings)
	}
}
