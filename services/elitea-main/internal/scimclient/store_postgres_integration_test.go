package scimclient

// The SCIM client store against a real PostgreSQL.
//
// The schema is created from shared migration 0134's own file, read from the
// embedded corpus, so this test cannot pass against a schema the migration does
// not create.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func TestABearerSecretAuthenticatesAndIsStoredOnlyAsAHash(t *testing.T) {
	pool := newClientPool(t)
	store := NewStore(pool, 0)
	ctx := context.Background()

	creator := 7
	issued, err := store.Create(ctx, "  Entra ID  ", MethodBearer, &creator)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(issued.Secret, PrefixBearerSecret))
	require.Equal(t, "Entra ID", issued.Client.Name)
	require.Empty(t, issued.Client.ClientID)
	require.Equal(t, issued.Secret[len(issued.Secret)-4:], issued.Client.SecretHint)

	// The secret itself is nowhere in the row.
	var hash, hint string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT secret_hash, secret_hint FROM elitea_auth.scim_clients WHERE id = $1`, issued.Client.ID).Scan(&hash, &hint))
	require.Equal(t, HashSecret(issued.Secret), hash)
	require.NotContains(t, hash, issued.Secret)

	principal, err := store.Authenticate(ctx, issued.Secret)
	require.NoError(t, err)
	require.Equal(t, issued.Client.ID, principal.ID)
	require.Equal(t, "scim:Entra ID", principal.ActorLabel())

	// A wrong secret of the right shape is rejected.
	other, err := randomToken(PrefixBearerSecret, secretBytes)
	require.NoError(t, err)
	_, err = store.Authenticate(ctx, other)
	require.ErrorIs(t, err, ErrRejected)

	// The listing never carries the secret: the type has no field for it.
	clients, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, clients, 1)
	require.Equal(t, creator, *clients[0].CreatedBy)

	// last_used_at is written once, then throttled.
	store.TouchLastUsed(ctx, principal.ID)
	listed, err := store.Get(ctx, principal.ID)
	require.NoError(t, err)
	require.NotNil(t, listed.LastUsedAt)
}

func TestRotateInvalidatesTheOldSecretImmediately(t *testing.T) {
	pool := newClientPool(t)
	store := NewStore(pool, 0)
	ctx := context.Background()

	issued, err := store.Create(ctx, "okta", MethodBearer, nil)
	require.NoError(t, err)
	rotated, err := store.Rotate(ctx, issued.Client.ID)
	require.NoError(t, err)
	require.NotEqual(t, issued.Secret, rotated.Secret)
	require.NotNil(t, rotated.Client.RotatedAt)

	_, err = store.Authenticate(ctx, issued.Secret)
	require.ErrorIs(t, err, ErrRejected, "the old secret must stop working at once")
	_, err = store.Authenticate(ctx, rotated.Secret)
	require.NoError(t, err)
}

func TestClientCredentialsIssueAnAccessTokenThatAuthenticates(t *testing.T) {
	pool := newClientPool(t)
	store := NewStore(pool, 0)
	ctx := context.Background()

	issued, err := store.Create(ctx, "entra-oauth", MethodClientCredentials, nil)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(issued.Client.ClientID, PrefixClientID))
	require.True(t, strings.HasPrefix(issued.Secret, PrefixClientSecret))

	// The client secret is not itself a SCIM bearer credential.
	_, err = store.Authenticate(ctx, issued.Secret)
	require.ErrorIs(t, err, ErrRejected)

	_, _, err = store.IssueAccessToken(ctx, issued.Client.ClientID, issued.Secret+"x")
	require.ErrorIs(t, err, ErrRejected)
	_, _, err = store.IssueAccessToken(ctx, "scimc_AAAAAAAAAAAAAAAAAAAAAA", issued.Secret)
	require.ErrorIs(t, err, ErrRejected)

	token, principal, err := store.IssueAccessToken(ctx, issued.Client.ClientID, issued.Secret)
	require.NoError(t, err)
	require.Equal(t, time.Hour, token.ExpiresIn)
	require.Equal(t, issued.Client.ID, principal.ID)
	require.True(t, strings.HasPrefix(token.Token, PrefixAccessToken))

	authenticated, err := store.Authenticate(ctx, token.Token)
	require.NoError(t, err)
	require.Equal(t, issued.Client.ID, authenticated.ID)

	// An expired access token is rejected.
	_, err = pool.Exec(ctx, `UPDATE elitea_auth.scim_access_tokens SET expires_at = now() - interval '1 second'`)
	require.NoError(t, err)
	_, err = store.Authenticate(ctx, token.Token)
	require.ErrorIs(t, err, ErrRejected)

	// Rotation revokes every outstanding access token.
	fresh, _, err := store.IssueAccessToken(ctx, issued.Client.ClientID, issued.Secret)
	require.NoError(t, err)
	rotated, err := store.Rotate(ctx, issued.Client.ID)
	require.NoError(t, err)
	_, err = store.Authenticate(ctx, fresh.Token)
	require.ErrorIs(t, err, ErrRejected)
	_, _, err = store.IssueAccessToken(ctx, issued.Client.ClientID, issued.Secret)
	require.ErrorIs(t, err, ErrRejected, "the old client secret must stop working at once")
	_, _, err = store.IssueAccessToken(ctx, issued.Client.ClientID, rotated.Secret)
	require.NoError(t, err)
}

func TestARevokedClientNoLongerAuthenticates(t *testing.T) {
	pool := newClientPool(t)
	store := NewStore(pool, 0)
	ctx := context.Background()

	bearer, err := store.Create(ctx, "bearer", MethodBearer, nil)
	require.NoError(t, err)
	oauth, err := store.Create(ctx, "oauth", MethodClientCredentials, nil)
	require.NoError(t, err)
	token, _, err := store.IssueAccessToken(ctx, oauth.Client.ClientID, oauth.Secret)
	require.NoError(t, err)

	for _, id := range []int64{bearer.Client.ID, oauth.Client.ID} {
		revoked, err := store.Revoke(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, revoked.RevokedAt)
	}
	_, err = store.Authenticate(ctx, bearer.Secret)
	require.ErrorIs(t, err, ErrRejected)
	_, err = store.Authenticate(ctx, token.Token)
	require.ErrorIs(t, err, ErrRejected)
	_, _, err = store.IssueAccessToken(ctx, oauth.Client.ClientID, oauth.Secret)
	require.ErrorIs(t, err, ErrRejected)

	_, err = store.Rotate(ctx, bearer.Client.ID)
	require.ErrorIs(t, err, ErrRevoked)

	require.NoError(t, store.Delete(ctx, bearer.Client.ID))
	require.ErrorIs(t, store.Delete(ctx, bearer.Client.ID), ErrNotFound)
	_, err = store.Revoke(ctx, bearer.Client.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestClientNamesAreUniqueAndMethodsAreChecked(t *testing.T) {
	pool := newClientPool(t)
	store := NewStore(pool, 0)
	ctx := context.Background()

	_, err := store.Create(ctx, "Entra", MethodBearer, nil)
	require.NoError(t, err)
	_, err = store.Create(ctx, " entra ", MethodClientCredentials, nil)
	require.ErrorIs(t, err, ErrDuplicateName)
	_, err = store.Create(ctx, "other", "password", nil)
	require.ErrorIs(t, err, ErrInvalidMethod)
	_, err = store.Create(ctx, "", MethodBearer, nil)
	require.ErrorIs(t, err, ErrInvalidName)
}

func newClientPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)
	require.NoError(t, adminPool.Ping(ctx))

	databaseName := fmt.Sprintf("elitea_scimclient_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+quoted)
	require.NoError(t, err)

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
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

	migration, err := platformmigrations.Files.ReadFile("shared/0135_scim_clients.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(migration))
	require.NoError(t, err)
	// Applying it twice must succeed: the runner may replay an idempotent file.
	_, err = pool.Exec(ctx, string(migration))
	require.NoError(t, err)
	return pool
}
