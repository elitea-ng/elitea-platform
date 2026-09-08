package mcpoauth_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ELITEA_TEST_DATABASE_URL is required for isolated PostgreSQL proof")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.MaxConns = 2
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	name := fmt.Sprintf("elitea_dcr_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE DATABASE "+quoted)
	require.NoError(t, err)
	cfg = cfg.Copy()
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		_, err := admin.Exec(cleanup, "DROP DATABASE "+quoted)
		if err != nil {
			t.Errorf("remove isolated test database: %v", err)
		}
		admin.Close()
	})
	migration, err := os.ReadFile("../../migrations/shared/0124_mcp_oauth_clients.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = pool.Exec(ctx, string(migration))
		require.NoError(t, err)
	}
	return pool
}

func TestConfidentialClientSurvivesReplacementAndRejectsSubstitution(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	key := bytes.Repeat([]byte{7}, 32)
	store, err := mcpoauth.NewClients(pool, key)
	require.NoError(t, err)
	binding := mcpoauth.Binding{ProjectID: 2, ActorID: 11, ClientID: "issued-client", TokenEndpoint: "https://issuer.example/token", Resource: "https://resource.example/mcp"}
	reference, err := store.Save(ctx, binding, "fixture-client-secret", time.Time{})
	require.NoError(t, err)
	require.Len(t, reference, 43)
	var encrypted []byte
	require.NoError(t, pool.QueryRow(ctx, "SELECT encrypted_credentials FROM elitea_auth.mcp_oauth_clients WHERE id=$1", reference).Scan(&encrypted))
	require.NotContains(t, string(encrypted), "fixture-client-secret")

	replacement, err := mcpoauth.NewClients(pool, key)
	require.NoError(t, err)
	secret, err := replacement.Load(ctx, reference, binding)
	require.NoError(t, err)
	require.Equal(t, "fixture-client-secret", secret)
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			secret, err := replacement.Load(ctx, reference, binding)
			if err != nil || secret != "fixture-client-secret" {
				t.Error("concurrent replica read failed")
			}
		})
	}
	group.Wait()

	for name, change := range map[string]func(*mcpoauth.Binding){
		"project":  func(b *mcpoauth.Binding) { b.ProjectID++ },
		"actor":    func(b *mcpoauth.Binding) { b.ActorID++ },
		"client":   func(b *mcpoauth.Binding) { b.ClientID += "-other" },
		"endpoint": func(b *mcpoauth.Binding) { b.TokenEndpoint += "/other" },
		"resource": func(b *mcpoauth.Binding) { b.Resource += "/other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := binding
			change(&changed)
			value, err := replacement.Load(ctx, reference, changed)
			require.ErrorIs(t, err, mcpoauth.ErrClientUnavailable)
			require.Empty(t, value)
		})
	}
	_, err = replacement.Load(ctx, reference[:42], binding)
	require.ErrorIs(t, err, mcpoauth.ErrClientUnavailable)
	wrongKey, err := mcpoauth.NewClients(pool, bytes.Repeat([]byte{8}, 32))
	require.NoError(t, err)
	_, err = wrongKey.Load(ctx, reference, binding)
	require.ErrorIs(t, err, mcpoauth.ErrClientUnavailable)

	otherReference, err := store.Save(ctx, binding, "other-secret", time.Time{})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE elitea_auth.mcp_oauth_clients SET encrypted_credentials=$1 WHERE id=$2", encrypted, otherReference)
	require.NoError(t, err)
	_, err = store.Load(ctx, otherReference, binding)
	require.ErrorIs(t, err, mcpoauth.ErrClientUnavailable)

	_, err = pool.Exec(ctx, "UPDATE elitea_auth.mcp_oauth_clients SET secret_expires_at=now()-interval '1 second' WHERE id=$1", reference)
	require.NoError(t, err)
	_, err = store.Load(ctx, reference, binding)
	require.ErrorIs(t, err, mcpoauth.ErrClientUnavailable)

	_, err = pool.Exec(ctx, "UPDATE elitea_auth.mcp_oauth_clients SET idle_expires_at=now()-interval '1 second' WHERE id=$1", otherReference)
	require.NoError(t, err)
	_, err = store.Load(ctx, otherReference, binding)
	require.ErrorIs(t, err, mcpoauth.ErrClientUnavailable)
	cancelled, stop := context.WithCancel(ctx)
	stop()
	_, err = store.Save(cancelled, binding, "fixture", time.Time{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestConfidentialClientPruningIsBounded(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO elitea_auth.mcp_oauth_clients
 (id,project_id,actor_id,client_id,token_endpoint,resource,encrypted_credentials,idle_expires_at)
 SELECT lpad(n::text,43,'0'),2,11,'client','https://issuer.example/token','',decode(repeat('01',29),'hex'),now()-interval '1 day'
 FROM generate_series(1,129) n`)
	require.NoError(t, err)
	require.NoError(t, sqlcgen.New(pool).PruneMCPOAuthClients(ctx))
	var count int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM elitea_auth.mcp_oauth_clients").Scan(&count))
	require.Equal(t, 1, count)
}

func TestConfidentialClientRequiresEncryptionKey(t *testing.T) {
	_, err := mcpoauth.NewClients(nil, nil)
	require.Error(t, err)
}
