package scimdirectory

// The ADR-0025 deactivation hook at every SCIM write that can suspend: the
// account's native device sessions and its browser sessions are revoked in the
// same transaction, and reactivation resurrects neither.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var sessionSequence int

// seedSessions gives the user one live native device session and one live
// browser session, and returns both ids.
func seedSessions(t *testing.T, store *Store, userID int) (string, string) {
	t.Helper()
	ctx := context.Background()
	sessionSequence++
	var device string
	require.NoError(t, store.pool.QueryRow(ctx, `
		INSERT INTO elitea_auth.native_sessions (user_id, client_id, device_name, platform, idle_timeout_seconds)
		VALUES ($1, 'dev.elitea.test', 'Phone', 'ios', 2592000) RETURNING id::text`, userID).Scan(&device))
	browser := fmt.Sprintf("browser-session-for-the-deactivation-hook-%d-%d", userID, sessionSequence)
	now := time.Now().UTC()
	_, err := store.pool.Exec(ctx, `
		INSERT INTO elitea_auth.browser_sessions
		    (id, user_id, email, provider, provider_session_index, created_at, last_seen_at, expires_at, idle_timeout_seconds)
		VALUES ($1, $2, 'x@corp.com', 'oidc', '', $3, $3, $4, 3600)`, browser, userID, now, now.Add(time.Hour))
	require.NoError(t, err)
	return device, browser
}

func requireRevoked(t *testing.T, store *Store, device, browser string) {
	t.Helper()
	ctx := context.Background()
	var reason *string
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT revoke_reason FROM elitea_auth.native_sessions WHERE id = $1`, device).Scan(&reason))
	require.NotNil(t, reason, "the native device session must be revoked in the suspension's transaction")
	require.Equal(t, "user_deactivated", *reason)
	var browserRevoked bool
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT revoked_at IS NOT NULL FROM elitea_auth.browser_sessions WHERE id = $1`, browser).Scan(&browserRevoked))
	require.True(t, browserRevoked, "the browser session must be revoked in the suspension's transaction")
}

func requireLive(t *testing.T, store *Store, device, browser string) {
	t.Helper()
	var nativeLive, browserLive bool
	ctx := context.Background()
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT revoked_at IS NULL FROM elitea_auth.native_sessions WHERE id = $1`, device).Scan(&nativeLive))
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT revoked_at IS NULL FROM elitea_auth.browser_sessions WHERE id = $1`, browser).Scan(&browserLive))
	require.True(t, nativeLive && browserLive, "a write that does not suspend must not revoke")
}

func TestEverySCIMDeactivationRevokesDevicesAndBrowserSessions(t *testing.T) {
	pool := newDirectoryPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	newUser := func(name string) User {
		user, err := store.Create(ctx, User{UserName: name + "@corp.com", Active: true, ActiveStated: true})
		require.NoError(t, err)
		return user
	}
	inactive := false

	t.Run("PATCH active:false (SetActive)", func(t *testing.T) {
		user := newUser("patch")
		device, browser := seedSessions(t, store, user.ID)
		_, err := store.SetActive(ctx, user.ID, false)
		require.NoError(t, err)
		requireRevoked(t, store, device, browser)
		// Reactivation restores the account, never the sessions.
		_, err = store.SetActive(ctx, user.ID, true)
		require.NoError(t, err)
		requireRevoked(t, store, device, browser)
	})
	t.Run("DELETE (Deactivate)", func(t *testing.T) {
		user := newUser("delete")
		device, browser := seedSessions(t, store, user.ID)
		require.NoError(t, store.Deactivate(ctx, user.ID))
		requireRevoked(t, store, device, browser)
	})
	t.Run("PUT active:false (Replace)", func(t *testing.T) {
		user := newUser("put")
		device, browser := seedSessions(t, store, user.ID)
		_, err := store.Replace(ctx, user.ID, User{UserName: "put@corp.com", Active: false, ActiveStated: true})
		require.NoError(t, err)
		requireRevoked(t, store, device, browser)
	})
	t.Run("PATCH with several operations (ApplyUserChanges)", func(t *testing.T) {
		user := newUser("changes")
		device, browser := seedSessions(t, store, user.ID)
		_, err := store.ApplyUserChanges(ctx, user.ID, UserChanges{Active: &inactive})
		require.NoError(t, err)
		requireRevoked(t, store, device, browser)
	})
	t.Run("adoption with active:false (Create)", func(t *testing.T) {
		user := newUser("adopt")
		device, browser := seedSessions(t, store, user.ID)
		_, err := store.Create(ctx, User{UserName: "adopt@corp.com", Active: false, ActiveStated: true})
		require.NoError(t, err)
		requireRevoked(t, store, device, browser)
	})
	t.Run("writes that do not suspend revoke nothing", func(t *testing.T) {
		user := newUser("keep")
		device, browser := seedSessions(t, store, user.ID)
		_, err := store.Create(ctx, User{UserName: "keep@corp.com"}) // replay without `active`
		require.NoError(t, err)
		name := "Keeper"
		_, err = store.ApplyUserChanges(ctx, user.ID, UserChanges{DisplayName: &name})
		require.NoError(t, err)
		_, err = store.Replace(ctx, user.ID, User{UserName: "keep@corp.com", Active: true, ActiveStated: true})
		require.NoError(t, err)
		requireLive(t, store, device, browser)
	})
}
