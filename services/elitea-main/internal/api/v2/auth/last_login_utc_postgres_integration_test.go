package auth

// auth_core__user.last_login is `timestamp` WITHOUT time zone holding the
// UTC wall clock, like every legacy-era naive column. now() stored into it
// goes through the SESSION time zone, so a first-login INSERT that wrote a
// bare now() stored local time on a non-UTC session. The pool here runs in
// the testpg zone (America/Toronto by default), so that bug is visible.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
)

func TestPostgresLoginStoresLastLoginAsTheUTCWallClock(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx := context.Background()

	var zone string
	require.NoError(t, pool.QueryRow(ctx, `SELECT current_setting('TimeZone')`).Scan(&zone))
	require.Equal(t, testpg.TimeZone(), zone, "the provisioning pool must run in the testpg zone")

	const skew = 5 * time.Minute
	lastLogin := func(id int) time.Time {
		t.Helper()
		var stored *time.Time
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT last_login FROM auth_core__user WHERE id = $1`, id).Scan(&stored))
		require.NotNil(t, stored, "last_login is NULL")
		return *stored
	}
	within := func(name string, stored, before, after time.Time) {
		t.Helper()
		if stored.Before(before.Add(-skew)) || stored.After(after.Add(skew)) {
			t.Errorf("%s: last_login = %v, want the UTC wall clock between %v and %v (session %s)",
				name, stored, before, after, zone)
		}
	}

	// First login: joinAccountByEmail's INSERT branch.
	before := time.Now().UTC()
	created, err := resolveOnce(t, pool, OIDCProviderRefPrefix+"utc-sub", "utc@corp.com")
	require.NoError(t, err)
	within("first login (INSERT)", lastLogin(created), before, time.Now().UTC())

	// An existing account without a provider link, signing in for the first
	// time through OIDC: the ON CONFLICT branch.
	var existing int
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO auth_core__user (email, name, last_login)
		 VALUES ('legacy@corp.com', 'Legacy', (now() AT TIME ZONE 'UTC') - interval '30 days') RETURNING id`,
	).Scan(&existing))
	before = time.Now().UTC()
	adopted, err := resolveOnce(t, pool, OIDCProviderRefPrefix+"legacy-sub", "legacy@corp.com")
	require.NoError(t, err)
	require.Equal(t, existing, adopted)
	within("first provider login of an existing account (ON CONFLICT)", lastLogin(existing), before, time.Now().UTC())

	// A returning login, by subject: reuseLinkedAccount's UPDATE.
	_, err = pool.Exec(ctx,
		`UPDATE auth_core__user SET last_login = (now() AT TIME ZONE 'UTC') - interval '30 days' WHERE id = $1`, created)
	require.NoError(t, err)
	before = time.Now().UTC()
	again, err := resolveOnce(t, pool, OIDCProviderRefPrefix+"utc-sub", "utc@corp.com")
	require.NoError(t, err)
	require.Equal(t, created, again)
	within("returning login (UPDATE)", lastLogin(created), before, time.Now().UTC())
}
