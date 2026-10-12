package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The test_tool source-tool gate admits a call only for the token that
// authenticated it, carrying a grant the minting facade recorded: owned by
// the caller, bound to the project, unexpired, for the named provider/tool,
// with the toolkit among its sources. A PAT with the same name, binding and
// expiry has no record and is admitted nowhere.
func TestCallbackTokenGrantsAdmitOnlyTheRecordedGrant(t *testing.T) {
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
	var owner, stranger int64
	require.NoError(t, pool.QueryRow(ctx, `
INSERT INTO auth_core__user (email, name) VALUES ('grant-owner@autotest.local', 'Owner') RETURNING id`).Scan(&owner))
	require.NoError(t, pool.QueryRow(ctx, `
INSERT INTO auth_core__user (email, name) VALUES ('grant-stranger@autotest.local', 'Stranger') RETURNING id`).Scan(&stranger))
	project, otherProject := int64(10), int64(11)

	token := func(uuid string, expires *time.Time, bound int64) int64 {
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `
INSERT INTO auth_core__token (uuid, expires, user_id, name)
VALUES ($1, $2, $3, 'inventory callback (project 10)') RETURNING id`,
			uuid, expires, owner).Scan(&id))
		_, err := pool.Exec(ctx, `
INSERT INTO elitea_identity.token_project_binding (token_id, project_id) VALUES ($1, $2)`, id, bound)
		require.NoError(t, err)
		return id
	}
	record := func(uuid string, tool string, sources ...int32) {
		require.NoError(t, grants.Record(ctx, CallbackTokenGrant{
			TokenUUID: uuid, OwnerID: owner, ProjectID: project,
			Provider: "inventory", Tool: tool, OwnerToolkitID: 7, SourceToolkitIDs: sources,
		}))
	}
	later, past := time.Now().UTC().Add(10*time.Minute), time.Now().UTC().Add(-time.Minute)
	live := token("a1111111-1111-4111-8111-111111111111", &later, project)
	record("a1111111-1111-4111-8111-111111111111", "investigate", 101, 103)
	expired := token("a2222222-2222-4222-8222-222222222222", &past, project)
	record("a2222222-2222-4222-8222-222222222222", "investigate", 101)
	wrongTool := token("a3333333-3333-4333-8333-333333333333", &later, project)
	record("a3333333-3333-4333-8333-333333333333", "smart_normalize_types", 101)
	pat := token("a4444444-4444-4444-8444-444444444444", &later, project)

	// Record refuses a token bound elsewhere, owned by somebody else, or absent.
	elsewhere := "a5555555-5555-4555-8555-555555555555"
	token(elsewhere, &later, otherProject)
	for name, grant := range map[string]CallbackTokenGrant{
		"bound to another project": {TokenUUID: elsewhere, OwnerID: owner, ProjectID: project},
		"another owner":            {TokenUUID: "a1111111-1111-4111-8111-111111111111", OwnerID: stranger, ProjectID: project},
		"no such token":            {TokenUUID: "a9999999-9999-4999-8999-999999999999", OwnerID: owner, ProjectID: project},
	} {
		grant.Provider, grant.Tool, grant.OwnerToolkitID = "inventory", "investigate", 7
		require.True(t, errors.Is(grants.Record(ctx, grant), ErrCallbackGrantNotRecorded), name)
	}

	cases := []struct {
		name                       string
		project, user, token, tool int64
		toolName                   string
		want                       bool
	}{
		{"the recorded grant, a listed source", project, owner, live, 101, "investigate", true},
		{"the other listed source", project, owner, live, 103, "investigate", true},
		{"a toolkit the grant does not list", project, owner, live, 102, "investigate", false},
		{"another project", otherProject, owner, live, 101, "investigate", false},
		{"another caller", project, stranger, live, 101, "investigate", false},
		{"an expired grant", project, owner, expired, 101, "investigate", false},
		{"a grant for another tool", project, owner, wrongTool, 101, "investigate", false},
		{"a PAT with the same name and binding", project, owner, pat, 101, "investigate", false},
		{"a malformed id", project, owner, 0, 101, "investigate", false},
	}
	for _, tc := range cases {
		got, err := grants.AdmitsSourceTool(ctx, tc.project, tc.user, tc.token, tc.tool, "inventory", tc.toolName)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}

	// A deleted grant (a revoked token cascades to it) admits nothing.
	_, err = pool.Exec(ctx, `DELETE FROM elitea_identity.callback_token_grant WHERE token_id = $1`, live)
	require.NoError(t, err)
	got, err := grants.AdmitsSourceTool(ctx, project, owner, live, 101, "inventory", "investigate")
	require.NoError(t, err)
	require.False(t, got, "a deleted grant still admits")
}
