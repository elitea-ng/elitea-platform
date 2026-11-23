package auth

// The first-login grant of identity.initial_global_admins
// (ELITEA_INITIAL_GLOBAL_ADMINS) leaves a history row (issue 2499, ADR-0059
// R0.10a): source `bootstrap`, no actor, in the sign-in's own transaction.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/identity"
)

func TestAnInitialAdminFirstSignInWritesOneBootstrapHistoryRow(t *testing.T) {
	pool := newFirstLoginPool(t)
	handler := (&OIDCHandler{pool: pool}).WithFirstLoginPolicy(
		FirstLoginPolicy{InitialGlobalAdmins: []string{"email:alice@corp.com"}})

	userID := signInWithVerifiedOIDC(t, handler,
		"00000000-1111-2222-3333-555555555555", "alice@corp.com", true)
	// The next sign-in grants nothing, so it records nothing.
	again := signInWithVerifiedOIDC(t, handler,
		"00000000-1111-2222-3333-555555555555", "alice@corp.com", true)
	require.Equal(t, userID, again)

	type row struct {
		role, mode, action, source string
		actor                      *int
	}
	rows, err := pool.Query(context.Background(), `
SELECT role_name, role_mode, action, source, actor_user_id
FROM elitea_auth.admin_role_grants WHERE user_id = $1 ORDER BY id`, userID)
	require.NoError(t, err)
	defer rows.Close()
	var history []row
	for rows.Next() {
		var r row
		require.NoError(t, rows.Scan(&r.role, &r.mode, &r.action, &r.source, &r.actor))
		history = append(history, r)
	}
	require.NoError(t, rows.Err())

	require.Len(t, history, 1, "the first sign-in of an initial administrator wrote %d history rows", len(history))
	require.Equal(t, identity.InitialAdministrationRole, history[0].role)
	require.Equal(t, identity.InitialAdministrationMode, history[0].mode)
	require.Equal(t, "granted", history[0].action)
	require.Equal(t, "bootstrap", history[0].source)
	require.Nil(t, history[0].actor, "a bootstrap grant has no actor")
}
