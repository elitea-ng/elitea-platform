package repos

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A provider invocation's execution id is `callback-<token uuid>`
// (providerhost/material.CallbackSettings): the edge keeps it only for the
// token that authenticated the call, owned by the caller, bound to the
// project, and not long expired.
func TestExecutionAttributionVerifierAcceptsOnlyTheCallersOwnCallbackToken(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	verifier := NewExecutionAttributionVerifier(pool)
	ctx := context.Background()

	// auth_core__* are pylon-owned: created in their 001_initial shape, as
	// every fixture in this package does (seedAuthCoreTables). The binding
	// table is shared/0071's and carries no foreign key to centry.project.
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
	var owner, stranger int64
	require.NoError(t, pool.QueryRow(ctx, `
INSERT INTO auth_core__user (email, name) VALUES ('callback-owner@autotest.local', 'Owner') RETURNING id`).Scan(&owner))
	require.NoError(t, pool.QueryRow(ctx, `
INSERT INTO auth_core__user (email, name) VALUES ('callback-stranger@autotest.local', 'Stranger') RETURNING id`).Scan(&stranger))
	project, otherProject := int64(10), int64(11)

	token := func(uuid string, user int64, expires *time.Time, bound *int64) string {
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `
INSERT INTO auth_core__token (uuid, expires, user_id, name) VALUES ($1, $2, $3, 'deepwiki callback') RETURNING id`,
			uuid, expires, user).Scan(&id))
		if bound != nil {
			_, err := pool.Exec(ctx, `
INSERT INTO elitea_identity.token_project_binding (token_id, project_id) VALUES ($1, $2)`, id, *bound)
			require.NoError(t, err)
		}
		return strconv.FormatInt(id, 10)
	}
	now := time.Now().UTC()
	later, lately, longAgo := now.Add(10*time.Minute), now.Add(-time.Minute), now.Add(-time.Hour)
	live := token("11111111-1111-4111-8111-111111111111", owner, &later, &project)
	justExpired := token("22222222-2222-4222-8222-222222222222", owner, &lately, &project)
	expired := token("33333333-3333-4333-8333-333333333333", owner, &longAgo, &project)
	unbound := token("44444444-4444-4444-8444-444444444444", owner, &later, nil)
	other := token("55555555-5555-4555-8555-555555555555", owner, &later, &project)

	p, u := strconv.FormatInt(project, 10), strconv.FormatInt(owner, 10)
	cases := []struct {
		name, project, user, tokenID, uuid string
		want                               bool
	}{
		{"the token that authenticated the call", p, u, live, "11111111-1111-4111-8111-111111111111", true},
		{"a token that expired moments ago", p, u, justExpired, "22222222-2222-4222-8222-222222222222", true},
		{"a token that expired long ago", p, u, expired, "33333333-3333-4333-8333-333333333333", false},
		{"an unbound token", p, u, unbound, "44444444-4444-4444-8444-444444444444", false},
		{"another token of the same caller names this one", p, u, other, "11111111-1111-4111-8111-111111111111", false},
		{"another person", p, strconv.FormatInt(stranger, 10), live, "11111111-1111-4111-8111-111111111111", false},
		{"another project", strconv.FormatInt(otherProject, 10), u, live, "11111111-1111-4111-8111-111111111111", false},
		{"a uuid nobody minted", p, u, live, "66666666-6666-4666-8666-666666666666", false},
		{"an unparseable token id", p, u, "x", "11111111-1111-4111-8111-111111111111", false},
	}
	for _, tc := range cases {
		got, err := verifier.VerifyCallbackExecution(ctx, tc.project, tc.user, tc.tokenID, tc.uuid)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}
}
