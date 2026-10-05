package identityrepo

// The Form/pylon-parity plane's first login applies the SAME adoption rule as
// the SSO plane's joinAccountByEmail (adoption.go). Before, it linked any row
// whose address matched.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
)

func TestFormPlaneFirstLoginAppliesTheSharedAdoptionRule(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := newIdentityTestDatabase(t, ctx)

	var scimRow, federatedRow, bareRow int64
	if err := pool.QueryRow(ctx, `INSERT INTO public.auth_core__user (email, name)
		VALUES ('scim@corp.com', 'Provisioned') RETURNING id`).Scan(&scimRow); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, pool, `INSERT INTO elitea_auth.scim_users (user_id, external_id) VALUES ($1, 'entra-1')`, scimRow)
	if err := pool.QueryRow(ctx, `INSERT INTO public.auth_core__user (email, name)
		VALUES ('sso@corp.com', 'SSO') RETURNING id`).Scan(&federatedRow); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, pool, `INSERT INTO public.auth_core__user_provider (user_id, provider_ref)
		VALUES ($1, 'oidc:entra-subject')`, federatedRow)
	if err := pool.QueryRow(ctx, `INSERT INTO public.auth_core__user (email, name)
		VALUES ('legacy@corp.com', 'Legacy') RETURNING id`).Scan(&bareRow); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, pool, `INSERT INTO public.auth_core__user_provider (user_id, provider_ref)
		VALUES ($1, 'legacy-bare-subject')`, bareRow)

	login := func(policy identity.ProvisioningPolicy, reference, email string) (identity.ProvisionResult, error) {
		return newProvisionService(t, pool, policy).Provision(ctx, identity.ProvisionRequest{
			Assertion: identity.VerifiedAssertion{Provider: "form", ProviderReference: reference, Email: email},
		})
	}

	// A SCIM-provisioned row is refused with the setting off ...
	if _, err := login(identity.ProvisioningPolicy{}, "mallory", "scim@corp.com"); !errors.Is(err, identity.ErrIdentityConflict) {
		t.Fatalf("SCIM row with adopt_scim_users off: err = %v, want ErrIdentityConflict", err)
	}
	assertCount(t, ctx, pool, 0, `SELECT count(*) FROM public.auth_core__user_provider WHERE user_id = $1`, scimRow)

	// ... and a row another federated subject holds is refused whatever the setting.
	if _, err := login(identity.ProvisioningPolicy{AdoptSCIMUsers: true}, "mallory", "sso@corp.com"); !errors.Is(err, identity.ErrIdentityConflict) {
		t.Fatalf("federated row: err = %v, want ErrIdentityConflict", err)
	}
	assertCount(t, ctx, pool, 1, `SELECT count(*) FROM public.auth_core__user_provider WHERE user_id = $1`, federatedRow)

	// A pylon bare ref does not count as federated: still adoptable.
	adopted, err := login(identity.ProvisioningPolicy{}, "legacy-form-login", "legacy@corp.com")
	if err != nil || adopted.UserID != bareRow {
		t.Fatalf("bare-ref row: result = %+v, err = %v, want adoption of %d", adopted, err, bareRow)
	}

	// With the setting on, the SCIM-provisioned row is adopted.
	adopted, err = login(identity.ProvisioningPolicy{AdoptSCIMUsers: true}, "alice", "scim@corp.com")
	if err != nil || adopted.UserID != scimRow {
		t.Fatalf("SCIM row with adopt_scim_users on: result = %+v, err = %v, want adoption of %d", adopted, err, scimRow)
	}
}
