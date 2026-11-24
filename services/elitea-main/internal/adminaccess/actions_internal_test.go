package adminaccess

import "testing"

// TestTokenAllowedHelperDeclaresTheException pins the builder the backend
// guide documents for a critical action a token may call.
func TestTokenAllowedHelperDeclaresTheException(t *testing.T) {
	t.Parallel()
	entry := post("/fixture/trust", AdminAuthUsersSuperAdmin).critical(ElevationSealed).
		tokenAllowed("an inherit token of a global administrator repairs an identity provider until the sunset date")
	table, err := NewActions([]Action{entry}, registry)
	if err != nil {
		t.Fatalf("NewActions refused the documented exception: %v", err)
	}
	got := table.Entries()[0]
	if got.Token != TokenAllowed || got.TokenReason == "" {
		t.Errorf("got token %q with reason %q; want allowed with the reason", got.Token, got.TokenReason)
	}
	if _, err := NewActions([]Action{entry.tokenAllowed(" ")}, registry); err == nil {
		t.Error("NewActions accepted token: allowed on a critical action with a blank reason")
	}
}
