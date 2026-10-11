package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// elitea-vector's token introspection (ADR-0031) reads Facts for the token
// the signature check resolved. Only a live callback token with a grant for
// its own bound project answers; a PAT with the same name, binding and
// expiry answers ErrCallbackTokenFactsNotFound.
func TestCallbackTokenFactsAnswerOnlyLiveRecordedGrants(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	grants := NewCallbackTokenGrants(pool)
	ctx := context.Background()

	seedAuthCoreTables(t, pool)
	_, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS public.auth_core__token (
    id SERIAL PRIMARY KEY,
    uuid VARCHAR(36) UNIQUE,
    expires TIMESTAMP,
    user_id INTEGER REFERENCES auth_core__user(id) ON DELETE CASCADE,
    name TEXT
)`)
	require.NoError(t, err)
	var owner int64
	require.NoError(t, pool.QueryRow(ctx, `
INSERT INTO auth_core__user (email, name) VALUES ('facts-owner@autotest.local', 'Owner') RETURNING id`).Scan(&owner))
	project := int64(20)

	token := func(uuid string, expires *time.Time) int64 {
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `
INSERT INTO auth_core__token (uuid, expires, user_id, name)
VALUES ($1, $2, $3, 'callback') RETURNING id`, uuid, expires, owner).Scan(&id))
		_, err := pool.Exec(ctx, `
INSERT INTO elitea_identity.token_project_binding (token_id, project_id) VALUES ($1, $2)`, id, project)
		require.NoError(t, err)
		return id
	}
	record := func(uuid string) {
		require.NoError(t, grants.Record(ctx, CallbackTokenGrant{
			TokenUUID: uuid, OwnerID: owner, ProjectID: project,
			Provider: "deepwiki", Tool: "ask", OwnerToolkitID: 7,
		}))
	}
	later := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second)
	past := time.Now().UTC().Add(-time.Minute)
	live := token("b1111111-1111-4111-8111-111111111111", &later)
	record("b1111111-1111-4111-8111-111111111111")
	expired := token("b2222222-2222-4222-8222-222222222222", &past)
	record("b2222222-2222-4222-8222-222222222222")
	pat := token("b3333333-3333-4333-8333-333333333333", &later)

	facts, err := grants.Facts(ctx, live)
	require.NoError(t, err)
	require.Equal(t, CallbackTokenFacts{
		UserID: owner, ProjectID: project, ExpiresAt: later, Provider: "deepwiki",
	}, facts)

	for name, id := range map[string]int64{"expired": expired, "a PAT": pat, "absent": 999999, "malformed": 0} {
		_, err := grants.Facts(ctx, id)
		require.True(t, errors.Is(err, ErrCallbackTokenFactsNotFound), name)
	}
}
