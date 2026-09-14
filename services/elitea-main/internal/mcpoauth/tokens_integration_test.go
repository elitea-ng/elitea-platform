package mcpoauth_test

import (
	"bytes"
	"context"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
	"time"
)

func TestDelegatedTokenReferencesScopeExpiryRevocationAndReplacement(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	migration, err := os.ReadFile("../../migrations/shared/0130_mcp_oauth_tokens.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = pool.Exec(ctx, string(migration))
		require.NoError(t, err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	store, err := mcpoauth.NewTokens(pool, key)
	require.NoError(t, err)
	binding := mcpoauth.TokenBinding{ProjectID: 2, ActorID: 11, ToolkitID: 4, Resource: "https://resource.example/mcp"}
	token := mcpoauth.AccessToken{AccessToken: "fixture-access-token", TokenType: "Bearer", SessionID: "session-1"}
	reference, err := store.Save(ctx, binding, token, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, reference.Reference, 43)
	require.EqualValues(t, 1, reference.Revision)
	var encrypted []byte
	require.NoError(t, pool.QueryRow(ctx, "SELECT encrypted_token FROM elitea_auth.mcp_oauth_tokens WHERE id=$1", reference.Reference).Scan(&encrypted))
	require.NotContains(t, string(encrypted), token.AccessToken)
	require.NotContains(t, string(encrypted), token.SessionID)
	replica, err := mcpoauth.NewTokens(pool, key)
	require.NoError(t, err)
	admitted, err := replica.Validate(ctx, reference.Reference, binding)
	require.NoError(t, err)
	require.Equal(t, reference.Revision, admitted.Revision)
	loaded, err := replica.Load(ctx, reference.Reference, binding)
	require.NoError(t, err)
	require.Equal(t, token, loaded)
	for name, change := range map[string]func(*mcpoauth.TokenBinding){
		"project":  func(b *mcpoauth.TokenBinding) { b.ProjectID++ },
		"actor":    func(b *mcpoauth.TokenBinding) { b.ActorID++ },
		"toolkit":  func(b *mcpoauth.TokenBinding) { b.ToolkitID++ },
		"resource": func(b *mcpoauth.TokenBinding) { b.Resource += "/foreign" },
	} {
		t.Run(name, func(t *testing.T) {
			foreign := binding
			change(&foreign)
			_, err := replica.Validate(ctx, reference.Reference, foreign)
			require.ErrorIs(t, err, mcpoauth.ErrTokenUnavailable)
			got, err := replica.Load(ctx, reference.Reference, foreign)
			require.ErrorIs(t, err, mcpoauth.ErrTokenUnavailable)
			require.Empty(t, got.AccessToken)
			require.NoError(t, replica.Revoke(ctx, reference.Reference, foreign))
			_, err = replica.Validate(ctx, reference.Reference, binding)
			require.NoError(t, err)
		})
	}
	wrongKey, err := mcpoauth.NewTokens(pool, bytes.Repeat([]byte{8}, 32))
	require.NoError(t, err)
	_, err = wrongKey.Load(ctx, reference.Reference, binding)
	require.ErrorIs(t, err, mcpoauth.ErrTokenUnavailable)
	other, err := store.Save(ctx, binding, token, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.NotEqual(t, reference.Reference, other.Reference)
	_, err = pool.Exec(ctx, "UPDATE elitea_auth.mcp_oauth_tokens SET encrypted_token=$1 WHERE id=$2", encrypted, other.Reference)
	require.NoError(t, err)
	_, err = replica.Load(ctx, other.Reference, binding)
	require.ErrorIs(t, err, mcpoauth.ErrTokenUnavailable)
	require.NoError(t, store.Revoke(ctx, reference.Reference, binding))
	require.NoError(t, store.Revoke(ctx, reference.Reference, binding))
	_, err = replica.Validate(ctx, reference.Reference, binding)
	require.ErrorIs(t, err, mcpoauth.ErrTokenUnavailable)
	_, err = replica.Load(ctx, reference.Reference, binding)
	require.ErrorIs(t, err, mcpoauth.ErrTokenUnavailable)
	expired, err := store.Save(ctx, binding, token, time.Now().Add(time.Hour))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE elitea_auth.mcp_oauth_tokens SET expires_at=now()-interval '1 second' WHERE id=$1", expired.Reference)
	require.NoError(t, err)
	_, err = replica.Validate(ctx, expired.Reference, binding)
	require.ErrorIs(t, err, mcpoauth.ErrTokenUnavailable)
	_, err = replica.Load(ctx, expired.Reference, binding)
	require.ErrorIs(t, err, mcpoauth.ErrTokenUnavailable)
	canceled, stop := context.WithCancel(ctx)
	stop()
	_, err = store.Save(canceled, binding, token, time.Now().Add(time.Hour))
	require.ErrorIs(t, err, context.Canceled)
}

func TestDelegatedTokenStorageRequiresKey(t *testing.T) {
	_, err := mcpoauth.NewTokens(nil, nil)
	require.Error(t, err)
}
