package nativeauth_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
)

// The registry is a per-replica cache (15 s). A client disabled on ANOTHER
// replica — this replica's cache still says active — must not be able to
// exchange a code: the disable's transaction revoked only the families that
// existed then, so a family minted afterwards would outlive the disable.
// The exchange re-checks the client inside its own transaction.
func TestNativeExchangeRefusesAClientDisabledBehindAStaleRegistry(t *testing.T) {
	s := newStack(t)
	userID := s.seedUser("stale-registry@example.com")
	code := s.signIn(userID, "stale-registry@example.com")

	disabled := fileClient()
	disabled.Enabled = false
	if _, err := s.store.UpsertClient(context.Background(), disabled, 0); err != nil {
		t.Fatalf("disable client: %v", err)
	}
	// No s.registry.Invalidate(): this replica did not see the save.
	if _, active, err := s.registry.Active(context.Background(), testClientID); err != nil || !active {
		t.Fatalf("precondition: the cached registry should still say active (active=%v err=%v)", active, err)
	}

	recorder, body := s.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"client_id": {testClientID}, "code_verifier": {testVerifier},
	})
	if recorder.Code != http.StatusUnauthorized || body.Error != "invalid_client" {
		t.Fatalf("exchange for a disabled client = %d %s, want 401 invalid_client", recorder.Code, recorder.Body.String())
	}
	var families int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM elitea_auth.native_sessions WHERE client_id = $1`, testClientID).Scan(&families); err != nil {
		t.Fatal(err)
	}
	if families != 0 {
		t.Fatalf("%d device families created for a disabled client, want 0", families)
	}
}

// A DB row that shadows the file entry and is ENABLED still exchanges; the
// in-transaction check reads the DB layer, not just the file layer.
func TestNativeExchangeHonoursAnEnabledDatabaseRow(t *testing.T) {
	s := newStack(t)
	userID := s.seedUser("db-row@example.com")
	if _, err := s.store.UpsertClient(context.Background(), fileClient(), 0); err != nil {
		t.Fatalf("save client: %v", err)
	}
	s.registry.Invalidate()
	if got := s.exchange(s.signIn(userID, "db-row@example.com")); got.AccessToken == "" {
		t.Fatalf("exchange = %+v", got)
	}
}
