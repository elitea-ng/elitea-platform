package admin

// The super_admin re-bind (issue 2558) against the shipped corpus: the links
// bound under a provider's old issuer move onto its current one, the old
// binding is gone, and one security audit event is in the outbox.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// newRebindCorpusPool is a private database holding the bootstrap schema and
// the shipped shared corpus, so the binding table carries its real foreign key.
func newRebindCorpusPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv("ELITEA_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	require.NoError(t, err)
	name := fmt.Sprintf("elitea_rebind_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+quoted)
	require.NoError(t, err)
	config, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	config.ConnConfig.Database = name
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})
	require.NoError(t, db.RunMigrations(ctx, pool))
	require.NoError(t, migrate.New(pool, migrations.Files).ApplyShared(ctx))
	return pool
}

func TestASuperAdminRebindsTheAccountsOfARotatedIssuer(t *testing.T) {
	pool := newRebindCorpusPool(t)
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO public.auth_core__user (id, email, name) VALUES (920001, 'root@corp.example', 'Root'),
		    (920002, 'other@corp.example', 'Other')`,
		`INSERT INTO public.auth_core__user_provider (user_id, provider_ref) VALUES
		    (920001, 'oidc:root'), (920002, 'oidc:other'), (920002, 'saml:other')`,
		`INSERT INTO elitea_auth.identity_providers (provider_key, kind, display_name, enabled, config) VALUES
		    ('corporate', 'oidc', 'Corporate', true,
		     '{"issuer": "https://new.corp.example", "client_id": "elitea", "redirect_uri": "https://e/cb"}')`,
		`INSERT INTO elitea_auth.identity_bindings (provider_ref, provider_key, issuer, user_id) VALUES
		    ('oidc:root', 'corporate', 'https://old.corp.example', 920001),
		    ('oidc:other', 'corporate', 'https://old.corp.example', 920002),
		    ('saml:other', 'adfs', 'urn:adfs', 920002)`,
	} {
		_, err := pool.Exec(ctx, statement)
		require.NoError(t, err, statement)
	}

	handler := NewHandler(pool,
		WithIdentityProviders(identityproviders.NewStore(pool), newRecordingVault()), superAdmin)
	recorder, annotation := audited(
		providerRequest(http.MethodPost, "corporate", `{"from_issuer":"https://old.corp.example"}`),
		handler.IdentityProviderRebind)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var body struct {
		Rebound int64  `json:"rebound"`
		Issuer  string `json:"issuer"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.EqualValues(t, 2, body.Rebound)
	require.Equal(t, "https://new.corp.example", body.Issuer)
	require.Equal(t, AuditIdentityRebound, annotation.Action)

	rows, err := pool.Query(ctx, `
SELECT provider_ref || '@' || provider_key || '@' || issuer FROM elitea_auth.identity_bindings ORDER BY 1`)
	require.NoError(t, err)
	var bindings []string
	for rows.Next() {
		var binding string
		require.NoError(t, rows.Scan(&binding))
		bindings = append(bindings, binding)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{
		"oidc:other@corporate@https://new.corp.example",
		"oidc:root@corporate@https://new.corp.example",
		"saml:other@adfs@urn:adfs",
	}, bindings, "the old issuer must stop matching, and the SAML link is not this provider's")

	var events int
	require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event_type = 'ai.elitea.security.v1.identity.link.changed'
  AND event->'data'->>'action' = 'identity.binding.rebound'`).Scan(&events))
	require.Equal(t, 1, events)

	// The same request again moves nothing: the old binding is gone.
	again, _ := audited(
		providerRequest(http.MethodPost, "corporate", `{"from_issuer":"https://old.corp.example"}`),
		handler.IdentityProviderRebind)
	require.Equal(t, http.StatusOK, again.Code)
	require.Contains(t, again.Body.String(), `"rebound":0`)
}
